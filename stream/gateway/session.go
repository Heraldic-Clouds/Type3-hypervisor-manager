package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/protocol"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/token"
)

// Session is one browser ↔ gateway ↔ agent stream.
type Session struct {
	g       *Gateway
	sid     string
	secret  string
	claims  token.Claims
	profile protocol.Profile
	log     *slog.Logger
	started time.Time

	pc    *webrtc.PeerConnection
	link  *protocol.LinkConn
	conn  net.Conn
	ready protocol.LinkReadyMsg
	video *webrtc.TrackLocalStaticSample
	audio *webrtc.TrackLocalStaticSample
	bwe   atomic.Pointer[cc.BandwidthEstimator]

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	mu         sync.Mutex
	control    *webrtc.DataChannel
	state      string
	targetKbps int
	clientMax  int
	fps        int
	lowSince   time.Time
	highSince  time.Time
	lastKeyReq time.Time
	agentStats protocol.LinkStatsMsg
	client     protocol.Control // last client stats
	suspended  bool
	closeCode  int

	videoBytes, audioBytes, inputEvents, droppedInput atomic.Uint64
	lastPTS                                           uint64
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Session) setState(st string) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
	s.log.Info("session state", "state", st)
}

func (g *Gateway) dialAgent(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: g.cfg.AgentDialTimeout}
	return d.DialContext(ctx, "tcp", addr)
}

func (g *Gateway) startSession(ctx context.Context, claims token.Claims, tok string, hello protocol.Control,
	codecs []string, addr, offer string, log *slog.Logger) (*Session, string, error) {

	prof := protocol.ClampProfile(hello.Profile, claims.Profile)
	w, h := prof.W, prof.H
	if hello.Screen != nil && hello.Screen.W > 0 && hello.Screen.H > 0 {
		// Never stream more pixels than the client can show.
		w, h = min(w, hello.Screen.W*max(1, int(hello.Screen.DPR))), min(h, hello.Screen.H*max(1, int(hello.Screen.DPR)))
	}
	conn, err := g.dialAgent(ctx, addr)
	if err != nil {
		return nil, "", codeError{protocol.CodeAgentDown, "agent unreachable"}
	}
	link := protocol.NewLinkConn(conn)
	if err := link.WriteJSON(protocol.LinkHello, protocol.LinkHelloMsg{
		Token: tok, Codecs: codecs, Profile: prof.Name, W: w, H: h, FPS: prof.FPS, Kbps: prof.StartKbps,
	}); err != nil {
		conn.Close()
		return nil, "", codeError{protocol.CodeAgentDown, "agent link failed"}
	}
	conn.SetReadDeadline(time.Now().Add(g.cfg.ReadyTimeout))
	f, err := link.Read()
	if err != nil {
		conn.Close()
		return nil, "", codeError{protocol.CodeAgentDown, "agent did not answer"}
	}
	if f.Kind == protocol.LinkBye {
		conn.Close()
		var b protocol.ByeMsg
		json.Unmarshal(f.Payload, &b)
		return nil, "", codeError{b.Code, b.Reason}
	}
	var ready protocol.LinkReadyMsg
	if f.Kind != protocol.LinkReady || json.Unmarshal(f.Payload, &ready) != nil {
		conn.Close()
		return nil, "", codeError{protocol.CodeAgentDown, "bad agent ready"}
	}
	conn.SetReadDeadline(time.Time{})

	sctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		g: g, sid: claims.Session, secret: randomHex(24), claims: claims, profile: prof,
		log: log, started: time.Now(), link: link, conn: conn, ready: ready,
		ctx: sctx, cancel: cancel, state: "negotiating",
		targetKbps: prof.StartKbps, fps: ready.FPS,
	}
	answer, err := s.negotiate(offer)
	if err != nil {
		s.Close(protocol.CodeEncoderFailed, "negotiation failed")
		return nil, "", codeError{protocol.CodeMalformed, err.Error()}
	}
	g.register(s)
	go s.readAgent()
	go s.heartbeat()
	go s.adapt()
	time.AfterFunc(15*time.Second, func() {
		s.mu.Lock()
		st := s.state
		s.mu.Unlock()
		if st == "negotiating" {
			s.Close(protocol.CodeAgentDown, "ICE/DTLS timeout")
		}
	})
	return s, answer, nil
}

func videoCodec(name string) []webrtc.RTPCodecParameters {
	fb := []webrtc.RTCPFeedback{{Type: "goog-remb"}, {Type: "transport-cc"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}}
	switch name {
	case protocol.AV1:
		return []webrtc.RTPCodecParameters{{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeAV1, ClockRate: 90000, RTCPFeedback: fb}, PayloadType: 45}}
	case protocol.H265:
		return []webrtc.RTPCodecParameters{{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH265, ClockRate: 90000, RTCPFeedback: fb}, PayloadType: 116}}
	}
	return []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f", RTCPFeedback: fb}, PayloadType: 106},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", RTCPFeedback: fb}, PayloadType: 102},
	}
}

func (s *Session) negotiate(offer string) (string, error) {
	m := &webrtc.MediaEngine{}
	for _, c := range videoCodec(s.ready.Codec) {
		if err := m.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			return "", err
		}
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1"},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return "", err
	}
	ir := &interceptor.Registry{}
	// Google congestion control on transport-wide feedback, without a pacer:
	// frames leave as soon as they are encoded and the encoder bitrate is
	// steered instead (PROTOCOL.md §5).
	ccf, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(s.profile.StartKbps*1000),
			gcc.SendSideBWEMinBitrate(500_000),
			gcc.SendSideBWEMaxBitrate(s.profile.MaxKbps*1000),
			gcc.SendSideBWEPacer(gcc.NewNoOpPacer()),
		)
	})
	if err != nil {
		return "", err
	}
	ccf.OnNewPeerConnection(func(_ string, e cc.BandwidthEstimator) { s.bwe.Store(&e) })
	ir.Add(ccf)
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, ir); err != nil {
		return "", err
	}
	if err := webrtc.ConfigureNack(m, ir); err != nil {
		return "", err
	}
	if err := webrtc.ConfigureRTCPReports(ir); err != nil {
		return "", err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(s.g.settingEngine()))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", err
	}
	s.pc = pc
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return "", err
	}
	vcap := videoCodec(s.ready.Codec)[0].RTPCodecCapability
	vcap.RTCPFeedback = nil
	if s.video, err = webrtc.NewTrackLocalStaticSample(vcap, "video", "mea-"+s.sid); err != nil {
		return "", err
	}
	vs, err := pc.AddTrack(s.video)
	if err != nil {
		return "", err
	}
	go s.readRTCP(vs)
	if s.ready.Audio {
		if s.audio, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "mea-"+s.sid); err != nil {
			return "", err
		}
		as, err := pc.AddTrack(s.audio)
		if err != nil {
			return "", err
		}
		go func() {
			buf := make([]byte, 1500)
			for {
				if _, _, err := as.Read(buf); err != nil {
					return
				}
			}
		}()
	}
	pc.OnConnectionStateChange(s.onState)
	pc.OnDataChannel(s.onDataChannel)
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return "", err
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		return "", errors.New("ICE gathering timeout")
	}
	return pc.LocalDescription().SDP, nil
}

func (s *Session) welcome() protocol.Control {
	return protocol.Control{
		T: "welcome", V: protocol.VersionJSON(protocol.Version), SID: s.sid, Codec: s.ready.Codec,
		W: s.ready.W, H: s.ready.H, FPS: s.ready.FPS, MaxKbps: s.profile.MaxKbps,
		Profile: s.profile.Name, Audio: s.ready.Audio,
		Caps: []string{"gamepad", "relative-mouse", "text", "suspend", "adaptive-bitrate"},
	}
}

func (s *Session) onState(st webrtc.PeerConnectionState) {
	switch st {
	case webrtc.PeerConnectionStateConnected:
		s.setState("streaming")
		s.requestKeyframe(true)
	case webrtc.PeerConnectionStateDisconnected:
		s.setState("recovering")
		s.agentControl(protocol.LinkControlMsg{T: "release"})
	case webrtc.PeerConnectionStateFailed:
		s.Close(protocol.CodeAgentDown, "connection failed")
	case webrtc.PeerConnectionStateClosed:
		s.Close(protocol.CodeNormal, "closed")
	}
}

func (s *Session) agentControl(m protocol.LinkControlMsg) {
	if err := s.link.WriteJSON(protocol.LinkControl, m); err != nil {
		s.log.Debug("agent control", "err", err)
	}
}

func (s *Session) requestKeyframe(force bool) {
	s.mu.Lock()
	if !force && time.Since(s.lastKeyReq) < 500*time.Millisecond {
		s.mu.Unlock()
		return
	}
	s.lastKeyReq = time.Now()
	s.mu.Unlock()
	s.agentControl(protocol.LinkControlMsg{T: "keyframe"})
}

func (s *Session) readRTCP(sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				s.requestKeyframe(false)
			}
		}
	}
}

// readAgent relays agent media to the browser.
func (s *Session) readAgent() {
	defer s.Close(protocol.CodeAgentDown, "agent link closed")
	for {
		s.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		f, err := s.link.Read()
		if err != nil {
			return
		}
		switch f.Kind {
		case protocol.LinkVideo:
			_, pts, au, err := protocol.ParseVideo(f.Payload)
			if err != nil {
				continue
			}
			s.mu.Lock()
			fps := max(s.fps, 1)
			s.mu.Unlock()
			d := time.Second / time.Duration(fps)
			if s.lastPTS != 0 && pts > s.lastPTS && pts-s.lastPTS < 1_000_000 {
				d = time.Duration(pts-s.lastPTS) * time.Microsecond
			}
			s.lastPTS = pts
			if err := s.video.WriteSample(media.Sample{Data: au, Duration: d}); err == nil {
				s.videoBytes.Add(uint64(len(au)))
				s.g.egressBytes.Add(uint64(len(au)))
			}
		case protocol.LinkAudio:
			if s.audio == nil {
				continue
			}
			if _, pkt, err := protocol.ParseAudio(f.Payload); err == nil {
				if s.audio.WriteSample(media.Sample{Data: pkt, Duration: 10 * time.Millisecond}) == nil {
					s.audioBytes.Add(uint64(len(pkt)))
					s.g.egressBytes.Add(uint64(len(pkt)))
				}
			}
		case protocol.LinkStats:
			var st protocol.LinkStatsMsg
			if json.Unmarshal(f.Payload, &st) == nil {
				s.mu.Lock()
				s.agentStats = st
				s.mu.Unlock()
			}
		case protocol.LinkBye:
			var b protocol.ByeMsg
			json.Unmarshal(f.Payload, &b)
			s.sendControl(protocol.Control{T: "error", Code: b.Code, Msg: b.Reason, Fatal: true})
			return
		}
	}
}

func (s *Session) heartbeat() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.agentControl(protocol.LinkControlMsg{T: "ping"})
			s.mu.Lock()
			st := s.agentStats
			target := s.targetKbps
			s.mu.Unlock()
			var g GPUSample
			if gs := s.g.cfg.Sampler.Last(); gs != nil {
				g = *gs
			}
			s.sendControl(protocol.Control{
				T: "sstats", EncMs: st.EncMs, CapMs: st.CapMs, FPS: int(st.FPS + 0.5), Kbps: int(st.Kbps),
				TargetKbps: target, Idle: st.Idle, GPU: g.GPUUtil, NVENC: g.EncUtil,
			})
		}
	}
}

// adapt steers the encoder from the congestion-control estimate (§5).
func (s *Session) adapt() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-t.C:
			e := s.bwe.Load()
			if e == nil {
				continue
			}
			est := (*e).GetTargetBitrate() / 1000
			s.mu.Lock()
			limit := s.profile.MaxKbps
			if s.clientMax > 0 {
				limit = min(limit, s.clientMax)
			}
			target := max(500, min(limit, est*85/100))
			changed := abs(target-s.targetKbps)*10 > s.targetKbps
			if changed {
				s.targetKbps = target
			}
			// halve the frame rate under sustained congestion, restore later
			fps, fpsChanged := s.fps, false
			if target*10 < s.profile.StartKbps*4 {
				s.highSince = time.Time{}
				if s.lowSince.IsZero() {
					s.lowSince = now
				} else if now.Sub(s.lowSince) > 5*time.Second && s.fps > s.profile.FPS/2 {
					s.fps, fps, fpsChanged = s.profile.FPS/2, s.profile.FPS/2, true
				}
			} else {
				s.lowSince = time.Time{}
				if s.highSince.IsZero() {
					s.highSince = now
				} else if now.Sub(s.highSince) > 15*time.Second && s.fps < s.profile.FPS {
					s.fps, fps, fpsChanged = s.profile.FPS, s.profile.FPS, true
				}
			}
			s.mu.Unlock()
			if changed {
				s.agentControl(protocol.LinkControlMsg{T: "bitrate", Kbps: target})
			}
			if fpsChanged {
				s.agentControl(protocol.LinkControlMsg{T: "fps", FPS: fps})
			}
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func (s *Session) sendControl(c protocol.Control) {
	s.mu.Lock()
	dc := s.control
	s.mu.Unlock()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	b, _ := json.Marshal(c)
	dc.SendText(string(b))
}

func (s *Session) onDataChannel(dc *webrtc.DataChannel) {
	switch dc.Label() {
	case protocol.LabelControl:
		s.mu.Lock()
		s.control = dc
		s.mu.Unlock()
		dc.OnOpen(func() { s.sendControl(s.welcome()) })
		dc.OnMessage(func(m webrtc.DataChannelMessage) { s.onControl(m.Data) })
	case protocol.LabelKey, protocol.LabelRT:
		reliable := dc.Label() == protocol.LabelKey
		limiter := newRateLimiter(map[bool]int{true: 400, false: 2000}[reliable])
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if !limiter.allow() {
				s.droppedInput.Add(1)
				return
			}
			in, ok, err := protocol.DecodeInput(m.Data)
			if err != nil || !ok {
				s.droppedInput.Add(1)
				return
			}
			// key/button events are only accepted on the reliable channel so
			// that a lost release cannot happen silently
			if protocol.Reliable(in.Type) != reliable {
				s.droppedInput.Add(1)
				return
			}
			s.inputEvents.Add(1)
			s.link.Write(protocol.LinkInput, m.Data)
		})
		if reliable {
			dc.OnClose(func() { s.agentControl(protocol.LinkControlMsg{T: "release"}) })
		}
	default:
		dc.Close()
	}
}

func (s *Session) onControl(b []byte) {
	if len(b) > 16<<10 {
		return
	}
	var c protocol.Control
	if json.Unmarshal(b, &c) != nil {
		return
	}
	switch c.T {
	case "ping":
		s.sendControl(protocol.Control{T: "pong", N: c.N, TS: c.TS})
	case "keyframe":
		s.requestKeyframe(false)
	case "release":
		s.agentControl(protocol.LinkControlMsg{T: "release"})
	case "suspend", "resume":
		s.mu.Lock()
		s.suspended = c.T == "suspend"
		s.mu.Unlock()
		s.agentControl(protocol.LinkControlMsg{T: c.T})
	case "stats":
		s.mu.Lock()
		s.client = c
		s.mu.Unlock()
	case "configure":
		s.mu.Lock()
		if c.MaxKbps > 0 {
			s.clientMax = max(500, c.MaxKbps)
		}
		if c.Profile != "" {
			s.profile = protocol.ClampProfile(c.Profile, s.claims.Profile)
		}
		if c.FPS > 0 {
			s.fps = min(c.FPS, s.profile.FPS)
		}
		fps, kbps := s.fps, min(s.targetKbps, s.profile.MaxKbps)
		if s.clientMax > 0 {
			kbps = min(kbps, s.clientMax)
		}
		s.targetKbps = kbps
		s.mu.Unlock()
		s.agentControl(protocol.LinkControlMsg{T: "fps", FPS: fps})
		s.agentControl(protocol.LinkControlMsg{T: "bitrate", Kbps: kbps})
		s.sendControl(protocol.Control{T: "configured", W: s.ready.W, H: s.ready.H, FPS: fps, Kbps: kbps, Codec: s.ready.Codec, Profile: s.profile.Name})
	case "bye":
		go s.Close(protocol.CodeNormal, "client bye")
	}
}

// Close ends the session once.
func (s *Session) Close(code int, reason string) {
	s.once.Do(func() {
		s.mu.Lock()
		s.closeCode = code
		s.state = "closed"
		s.mu.Unlock()
		s.log.Info("session closed", "code", code, "reason", reason,
			"videoMB", float64(s.videoBytes.Load())/1e6, "seconds", int(time.Since(s.started).Seconds()))
		s.sendControl(protocol.Control{T: "bye", Code: code, Reason: reason})
		s.link.WriteJSON(protocol.LinkBye, protocol.ByeMsg{Code: code, Reason: reason})
		s.cancel()
		s.conn.Close()
		if s.pc != nil {
			go func() {
				time.Sleep(100 * time.Millisecond) // let the bye message flush
				s.pc.Close()
			}()
		}
		s.g.unregister(s)
	})
}

type rateLimiter struct {
	mu     sync.Mutex
	rate   int
	tokens float64
	last   time.Time
}

func newRateLimiter(perSec int) *rateLimiter {
	return &rateLimiter{rate: perSec, tokens: float64(perSec)}
}

func (r *rateLimiter) allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if !r.last.IsZero() {
		r.tokens = min(float64(r.rate), r.tokens+now.Sub(r.last).Seconds()*float64(r.rate))
	}
	r.last = now
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}

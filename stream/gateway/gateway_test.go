package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/agent"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/agent/input"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/agent/media"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/protocol"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/token"
)

type recDesk struct {
	mu   sync.Mutex
	keys map[uint32]bool
	rel  int
}

func (d *recDesk) Key(k uint32, down bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.keys == nil {
		d.keys = map[uint32]bool{}
	}
	d.keys[k] = down
	return nil
}
func (d *recDesk) isDown(k uint32) bool       { d.mu.Lock(); defer d.mu.Unlock(); return d.keys[k] }
func (d *recDesk) Button(uint8, bool) error   { return nil }
func (d *recDesk) MoveAbs(_, _ uint16) error  { return nil }
func (d *recDesk) MoveRel(dx, dy int16) error { d.mu.Lock(); d.rel++; d.mu.Unlock(); return nil }
func (d *recDesk) Wheel(_, _ int16) error     { return nil }
func (d *recDesk) Close() error               { return nil }

type env struct {
	priv ed25519.PrivateKey
	gw   *Gateway
	api  *httptest.Server
	desk *recDesk
}

func newEnv(t *testing.T, limits Limits) *env {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	desk := &recDesk{}
	ag := agent.New(agent.Config{
		DesktopID: "desk-1", PublicKey: pub,
		NewVideo: func() media.VideoSource { return &media.FFmpeg{Encoder: "x264", TestPattern: true} },
		NewAudio: func() media.AudioSource { return &media.Audio{TestPattern: true} },
		Injector: &input.Injector{Desktop: desk},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go ag.Serve(ctx, ln)

	gw, err := New(Config{
		HostID: "host-1", PublicKey: pub, Loopback: true,
		Registry:       StaticRegistry{"desk-1": ln.Addr().String()},
		Limits:         limits,
		AllowedOrigins: []string{"https://miseenaby.me"},
		AdminToken:     "admin-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	api := httptest.NewServer(gw.Handler())
	t.Cleanup(api.Close)
	return &env{priv: priv, gw: gw, api: api, desk: desk}
}

func (e *env) token(t *testing.T, c token.Claims) string {
	if c.User == "" {
		c = token.Claims{User: "alice", Desktop: "desk-1", Session: token.NewID(), Host: "host-1", Entitlement: "eleet", Profile: "economy"}
	}
	s, _, err := token.Mint(e.priv, c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type client struct {
	pc      *webrtc.PeerConnection
	control *webrtc.DataChannel
	key, rt *webrtc.DataChannel
	welcome chan protocol.Control
	pong    chan protocol.Control
	video   atomic.Int64
	audio   atomic.Int64
	resp    CreateResponse
	status  int
	errBody apiError
}

func newClient(t *testing.T) *client {
	var se webrtc.SettingEngine
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	m := &webrtc.MediaEngine{}
	m.RegisterDefaultCodecs()
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	c := &client{pc: pc, welcome: make(chan protocol.Control, 1), pong: make(chan protocol.Control, 4)}
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	ordered, zero := false, uint16(0)
	c.control, _ = pc.CreateDataChannel(protocol.LabelControl, nil)
	c.key, _ = pc.CreateDataChannel(protocol.LabelKey, nil)
	c.rt, _ = pc.CreateDataChannel(protocol.LabelRT, &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &zero})
	c.control.OnMessage(func(m webrtc.DataChannelMessage) {
		var ctl protocol.Control
		json.Unmarshal(m.Data, &ctl)
		switch ctl.T {
		case "welcome":
			select {
			case c.welcome <- ctl:
			default:
			}
		case "pong":
			c.pong <- ctl
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				p, _, err := tr.ReadRTP()
				if err != nil {
					return
				}
				if tr.Kind() == webrtc.RTPCodecTypeVideo {
					c.video.Add(int64(len(p.Payload)))
				} else {
					c.audio.Add(int64(len(p.Payload)))
				}
			}
		}()
	})
	return c
}

func (c *client) connect(t *testing.T, e *env, tok string) {
	offer, err := c.pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	g := webrtc.GatheringCompletePromise(c.pc)
	c.pc.SetLocalDescription(offer)
	<-g
	hello := protocol.Control{T: "hello", V: json.RawMessage(`["mea-stream/1"]`), Codecs: []string{"h264"}, Profile: "balanced"}
	body, _ := json.Marshal(CreateRequest{Token: tok, SDP: c.pc.LocalDescription().SDP, Hello: &hello})
	req, _ := http.NewRequest("POST", e.api.URL+"/v1/sessions", bytes.NewReader(body))
	req.Header.Set("Origin", "https://miseenaby.me")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	c.status = res.StatusCode
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusCreated {
		json.Unmarshal(b, &c.errBody)
		return
	}
	if err := json.Unmarshal(b, &c.resp); err != nil {
		t.Fatal(err)
	}
	if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: c.resp.SDP}); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEndToEnd(t *testing.T) {
	e := newEnv(t, Limits{MaxSessions: 4})
	c := newClient(t)
	c.connect(t, e, e.token(t, token.Claims{}))
	if c.status != http.StatusCreated {
		t.Fatalf("status %d %+v", c.status, c.errBody)
	}
	// economy profile from the token caps the stream
	if c.resp.Welcome.W != 1280 || c.resp.Welcome.H != 720 || c.resp.Welcome.Codec != "h264" || !c.resp.Welcome.Audio {
		t.Fatalf("welcome %+v", c.resp.Welcome)
	}
	select {
	case w := <-c.welcome:
		if w.SID != c.resp.SID {
			t.Fatal("welcome sid mismatch")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no welcome on control channel")
	}
	eventually(t, 10*time.Second, "video RTP", func() bool { return c.video.Load() > 50_000 })
	eventually(t, 10*time.Second, "audio RTP", func() bool { return c.audio.Load() > 1_000 })

	c.control.SendText(`{"t":"ping","n":7,"ts":123}`)
	select {
	case p := <-c.pong:
		if p.N != 7 || p.TS != 123 {
			t.Fatal(p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no pong")
	}

	c.key.Send(protocol.Input{Type: protocol.InKey, Down: true, Keysym: 'w'}.Encode())
	c.rt.Send(protocol.Input{Type: protocol.InMotionRel, DX: 5, DY: 5}.Encode())
	// a key event on the unreliable channel must be refused
	c.rt.Send(protocol.Input{Type: protocol.InKey, Down: true, Keysym: 'x'}.Encode())
	eventually(t, 5*time.Second, "key at agent", func() bool { return e.desk.isDown('w') })
	eventually(t, 5*time.Second, "motion at agent", func() bool { e.desk.mu.Lock(); defer e.desk.mu.Unlock(); return e.desk.rel > 0 })
	time.Sleep(200 * time.Millisecond)
	if e.desk.isDown('x') {
		t.Fatal("key accepted on unreliable channel")
	}

	m := e.gw.Metrics()
	if m.Active != 1 || m.Sessions[0].State != "streaming" || m.Sessions[0].User != "alice" {
		t.Fatalf("metrics %+v", m)
	}

	// client release: the held key is let go
	c.control.SendText(`{"t":"release"}`)
	eventually(t, 5*time.Second, "release", func() bool { return !e.desk.isDown('w') })

	// hold again and drop the peer connection without saying goodbye
	c.key.Send(protocol.Input{Type: protocol.InKey, Down: true, Keysym: 'w'}.Encode())
	eventually(t, 5*time.Second, "key down again", func() bool { return e.desk.isDown('w') })
	c.pc.Close()
	eventually(t, 15*time.Second, "release after disconnect", func() bool { return !e.desk.isDown('w') })
	eventually(t, 15*time.Second, "session end", func() bool { return e.gw.Metrics().Active == 0 })
}

func TestReconnectReplacesSession(t *testing.T) {
	e := newEnv(t, Limits{MaxSessions: 1})
	c1 := newClient(t)
	c1.connect(t, e, e.token(t, token.Claims{}))
	if c1.status != http.StatusCreated {
		t.Fatal(c1.status, c1.errBody)
	}
	eventually(t, 10*time.Second, "first video", func() bool { return c1.video.Load() > 10_000 })
	// the limit is 1, but the same desktop reconnecting replaces its session
	c2 := newClient(t)
	c2.connect(t, e, e.token(t, token.Claims{}))
	if c2.status != http.StatusCreated {
		t.Fatal(c2.status, c2.errBody)
	}
	eventually(t, 10*time.Second, "second video", func() bool { return c2.video.Load() > 10_000 })
	if n := e.gw.Metrics().Active; n != 1 {
		t.Fatalf("%d sessions", n)
	}
}

func TestRejections(t *testing.T) {
	e := newEnv(t, Limits{MaxSessions: 4})
	cases := []struct {
		name   string
		claims token.Claims
		tok    string
		status int
		code   int
	}{
		{"wrong host", token.Claims{User: "a", Desktop: "desk-1", Session: "s", Host: "host-2", Entitlement: "eleet"}, "", 403, protocol.CodeForbidden},
		{"desktop elsewhere", token.Claims{User: "a", Desktop: "desk-9", Session: "s", Host: "host-1", Entitlement: "eleet"}, "", 404, protocol.CodeAgentDown},
		{"garbage", token.Claims{}, "abc.def", 401, protocol.CodeBadToken},
	}
	for _, tc := range cases {
		tok := tc.tok
		if tok == "" {
			tok = e.token(t, tc.claims)
		}
		c := newClient(t)
		c.connect(t, e, tok)
		if c.status != tc.status || c.errBody.Code != tc.code {
			t.Errorf("%s: %d %+v", tc.name, c.status, c.errBody)
		}
	}
	// replay of a token that was already used
	tok := e.token(t, token.Claims{})
	c := newClient(t)
	c.connect(t, e, tok)
	if c.status != 201 {
		t.Fatal(c.status, c.errBody)
	}
	c2 := newClient(t)
	c2.connect(t, e, tok)
	if c2.status != 401 || c2.errBody.Code != protocol.CodeBadToken {
		t.Fatalf("replay: %d %+v", c2.status, c2.errBody)
	}
	// unknown origin
	req, _ := http.NewRequest("POST", e.api.URL+"/v1/sessions", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Origin", "https://evil.example")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 403 {
		t.Fatal("foreign origin allowed")
	}
}

func TestCapacityAndAdmin(t *testing.T) {
	e := newEnv(t, Limits{MaxSessions: 0, Draining: true})
	c := newClient(t)
	c.connect(t, e, e.token(t, token.Claims{}))
	if c.status != 503 || c.errBody.Code != protocol.CodeCapacity {
		t.Fatalf("draining host admitted: %d %+v", c.status, c.errBody)
	}
	admin := httptest.NewServer(e.gw.AdminHandler())
	defer admin.Close()
	res, _ := http.Get(admin.URL + "/v1/metrics")
	if res.StatusCode != 401 {
		t.Fatal("metrics without token")
	}
	req, _ := http.NewRequest("PUT", admin.URL+"/v1/limits", bytes.NewReader([]byte(`{"maxSessions":2,"draining":false}`)))
	req.Header.Set("Authorization", "Bearer admin-secret")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 200 {
		t.Fatal("limits", res.StatusCode)
	}
	c = newClient(t)
	c.connect(t, e, e.token(t, token.Claims{}))
	if c.status != 201 {
		t.Fatalf("after undrain: %d %+v", c.status, c.errBody)
	}
	req, _ = http.NewRequest("GET", admin.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	res, _ = http.DefaultClient.Do(req)
	b, _ := io.ReadAll(res.Body)
	if !bytes.Contains(b, []byte("mea_stream_sessions_active{host=\"host-1\"} 1")) {
		t.Fatalf("prometheus: %s", b)
	}
	// client DELETE with the session secret
	req, _ = http.NewRequest("DELETE", e.api.URL+"/v1/sessions/"+c.resp.SID, nil)
	req.Header.Set("Authorization", "Bearer wrong")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 404 {
		t.Fatal("delete with wrong secret")
	}
	req.Header.Set("Authorization", "Bearer "+c.resp.Secret)
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != 204 {
		t.Fatal("delete", res.StatusCode)
	}
	eventually(t, 5*time.Second, "closed", func() bool { return e.gw.Metrics().Active == 0 })
}

func TestAdmitAndSMI(t *testing.T) {
	g, ok := ParseSMI("NVIDIA L4, 570.86.16, 37, 52, 4100, 23034, 3, 59.8, 2100, 51, 41.2")
	if !ok || g.EncUtil != 52 || g.EncSessions != 3 || g.Name != "NVIDIA L4" {
		t.Fatal(g)
	}
	if ok, _ := Admit(Limits{MaxSessions: 4, MaxEncoderPct: 50}, 1, &g); ok {
		t.Fatal("encoder saturation ignored")
	}
	if ok, _ := Admit(Limits{MaxSessions: 4, MaxEncoderPct: 90}, 1, &g); !ok {
		t.Fatal("should admit")
	}
	if ok, _ := Admit(Limits{MaxSessions: 1}, 1, nil); ok {
		t.Fatal("limit ignored")
	}
}

func TestRegistryValidation(t *testing.T) {
	for _, a := range []string{"169.254.169.254:80", "8.8.8.8:47100", "nonsense"} {
		if validateAgentAddr(a) == nil {
			t.Errorf("%s accepted", a)
		}
	}
	if validateAgentAddr("10.20.0.5:47100") != nil {
		t.Error("bridge address rejected")
	}
}

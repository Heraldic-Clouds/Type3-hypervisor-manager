// Package agent is the MEA Stream Agent that runs inside one Eleet LXD
// container. It accepts a single gateway link at a time (PROTOCOL.md §6),
// verifies the stream token against its own desktop id, captures and encodes
// that container's display and audio, and injects input into that session.
package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/agent/input"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/agent/media"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/protocol"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/token"
)

const (
	helloTimeout = 5 * time.Second
	// The gateway pings every second; three missed heartbeats end the link
	// and release all input (PROTOCOL.md §3.3).
	linkTimeout = 3500 * time.Millisecond
)

type Config struct {
	DesktopID string
	PublicKey ed25519.PublicKey
	Display   string

	// NewVideo returns the video backend (native helper when available).
	NewVideo func() media.VideoSource
	// NewAudio returns the audio backend, or nil to stream video only.
	NewAudio func() media.AudioSource
	Injector *input.Injector // nil disables input (read-only desktops)

	Log *slog.Logger
	Now func() time.Time
}

type Agent struct {
	cfg      Config
	verifier *token.Verifier
	log      *slog.Logger

	mu     sync.Mutex
	active *link
}

func New(cfg Config) *Agent {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Display == "" {
		cfg.Display = ":0"
	}
	return &Agent{
		cfg: cfg,
		log: cfg.Log,
		verifier: &token.Verifier{
			Public:  cfg.PublicKey,
			Desktop: cfg.DesktopID,
			Now:     cfg.Now,
		},
	}
}

// Serve accepts gateway links until ctx is done.
func (a *Agent) Serve(ctx context.Context, ln net.Listener) error {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go a.handle(ctx, c)
	}
}

// Active reports whether a gateway is attached (health endpoint).
func (a *Agent) Active() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active != nil
}

type link struct {
	conn   net.Conn
	lc     *protocol.LinkConn
	claims token.Claims
	video  media.VideoSource
	audio  media.AudioSource
	cancel context.CancelFunc
	done   chan struct{}
}

func (a *Agent) bye(lc *protocol.LinkConn, code int, reason string) {
	lc.WriteJSON(protocol.LinkBye, protocol.ByeMsg{Code: code, Reason: reason})
}

func (a *Agent) handle(parent context.Context, c net.Conn) {
	defer c.Close()
	lc := protocol.NewLinkConn(c)
	c.SetReadDeadline(time.Now().Add(helloTimeout))
	f, err := lc.Read()
	if err != nil || f.Kind != protocol.LinkHello {
		a.bye(lc, protocol.CodeMalformed, "expected hello")
		return
	}
	var hello protocol.LinkHelloMsg
	if err := json.Unmarshal(f.Payload, &hello); err != nil {
		a.bye(lc, protocol.CodeMalformed, "bad hello")
		return
	}
	claims, err := a.verifier.Verify(hello.Token)
	if err != nil {
		code := protocol.CodeBadToken
		if errors.Is(err, token.ErrWrongDesk) || errors.Is(err, token.ErrEntitlement) {
			code = protocol.CodeForbidden
		}
		a.log.Warn("rejected link", "remote", c.RemoteAddr().String(), "err", err)
		a.bye(lc, code, err.Error())
		return
	}
	prof := protocol.ClampProfile(hello.Profile, claims.Profile)
	cfg := media.Config{
		W: orDefault(hello.W, prof.W), H: orDefault(hello.H, prof.H),
		FPS: orDefault(hello.FPS, prof.FPS), Kbps: orDefault(hello.Kbps, prof.StartKbps),
		Display: a.cfg.Display,
	}
	cfg.W, cfg.H = min(cfg.W, prof.W), min(cfg.H, prof.H)
	cfg.FPS, cfg.Kbps = min(cfg.FPS, prof.FPS), min(cfg.Kbps, prof.MaxKbps)
	cfg.W, cfg.H = cfg.W&^1, cfg.H&^1 // encoders need even sizes

	video := a.cfg.NewVideo()
	cfg.Codec = protocol.SelectCodec(hello.Codecs, video.Codecs())
	if cfg.Codec == "" {
		a.bye(lc, protocol.CodeEncoderFailed, "no common codec")
		return
	}

	ctx, cancel := context.WithCancel(parent)
	l := &link{conn: c, lc: lc, claims: claims, video: video, cancel: cancel, done: make(chan struct{})}
	defer close(l.done)
	defer cancel()

	// One gateway link per desktop: a newer authorised link takes over.
	a.mu.Lock()
	old := a.active
	a.active = l
	a.mu.Unlock()
	if old != nil {
		a.bye(old.lc, protocol.CodeTakenOver, "taken over")
		old.cancel()
		old.conn.Close()
		<-old.done
	}
	defer func() {
		a.mu.Lock()
		if a.active == l {
			a.active = nil
		}
		a.mu.Unlock()
	}()

	if err := video.Start(ctx, cfg); err != nil {
		a.log.Error("video start", "err", err)
		a.bye(lc, protocol.CodeCaptureFailed, err.Error())
		return
	}
	defer video.Stop()

	hasAudio := false
	if a.cfg.NewAudio != nil {
		if au := a.cfg.NewAudio(); au != nil {
			if err := au.Start(ctx); err != nil {
				a.log.Warn("audio unavailable, streaming video only", "err", err)
			} else {
				l.audio, hasAudio = au, true
				defer au.Stop()
			}
		}
	}
	if a.cfg.Injector != nil {
		defer a.cfg.Injector.ReleaseAll()
	}

	if err := lc.WriteJSON(protocol.LinkReady, protocol.LinkReadyMsg{
		Codec: cfg.Codec, Codecs: video.Codecs(), W: cfg.W, H: cfg.H, FPS: cfg.FPS,
		Audio: hasAudio, Encoder: video.Name(),
	}); err != nil {
		return
	}
	a.log.Info("link up", "user", claims.User, "sid", claims.Session, "codec", cfg.Codec,
		"size", [2]int{cfg.W, cfg.H}, "fps", cfg.FPS, "kbps", cfg.Kbps, "encoder", video.Name())

	go a.pumpVideo(ctx, l)
	if l.audio != nil {
		go a.pumpAudio(ctx, l)
	}
	go a.pumpStats(ctx, l)
	a.readLoop(ctx, l, cfg)
	a.log.Info("link down", "sid", claims.Session)
}

func orDefault(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}

func (a *Agent) pumpVideo(ctx context.Context, l *link) {
	frames := l.video.Frames()
	for {
		select {
		case <-ctx.Done():
			return
		case au, ok := <-frames:
			if !ok {
				return
			}
			if err := l.lc.Write(protocol.LinkVideo, protocol.VideoPayload(au.Key, uint64(au.PTS.Microseconds()), au.Data)); err != nil {
				l.cancel()
				return
			}
		}
	}
}

func (a *Agent) pumpAudio(ctx context.Context, l *link) {
	pk := l.audio.Packets()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-pk:
			if !ok {
				return
			}
			if err := l.lc.Write(protocol.LinkAudio, protocol.AudioPayload(uint64(p.PTS.Microseconds()), p.Data)); err != nil {
				l.cancel()
				return
			}
		}
	}
}

func (a *Agent) pumpStats(ctx context.Context, l *link) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := l.video.Stats()
			l.lc.WriteJSON(protocol.LinkStats, protocol.LinkStatsMsg{
				FPS: s.FPS, Kbps: s.Kbps, EncMs: s.EncMs, CapMs: s.CapMs, Idle: s.Idle,
				Suspended: s.Suspended, Encoder: s.Encoder, Frames: s.Frames,
				Skipped: s.Skipped, Errors: s.Errors, W: s.W, H: s.H,
			})
		}
	}
}

func (a *Agent) readLoop(ctx context.Context, l *link, cfg media.Config) {
	for ctx.Err() == nil {
		l.conn.SetReadDeadline(time.Now().Add(linkTimeout))
		f, err := l.lc.Read()
		if err != nil {
			return
		}
		switch f.Kind {
		case protocol.LinkInput:
			if a.cfg.Injector == nil {
				continue
			}
			in, ok, err := protocol.DecodeInput(f.Payload)
			if err != nil || !ok {
				continue
			}
			if err := a.cfg.Injector.Apply(in); err != nil {
				a.log.Debug("input", "err", err)
			}
		case protocol.LinkControl:
			var m protocol.LinkControlMsg
			if json.Unmarshal(f.Payload, &m) != nil {
				continue
			}
			switch m.T {
			case "ping":
			case "bitrate":
				l.video.SetBitrate(min(m.Kbps, protocol.ClampProfile("high", l.claims.Profile).MaxKbps))
			case "fps":
				l.video.SetFPS(min(m.FPS, protocol.ClampProfile("high", l.claims.Profile).FPS))
			case "keyframe":
				l.video.ForceKeyframe()
			case "release":
				if a.cfg.Injector != nil {
					a.cfg.Injector.ReleaseAll()
				}
			case "suspend":
				l.video.Suspend(true)
				if a.cfg.Injector != nil {
					a.cfg.Injector.ReleaseAll()
				}
			case "resume":
				l.video.Suspend(false)
			}
		case protocol.LinkBye:
			return
		}
	}
}

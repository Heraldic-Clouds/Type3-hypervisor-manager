package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/agent/input"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/agent/media"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/protocol"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/token"
)

type fakeVideo struct {
	mu      sync.Mutex
	out     chan media.AccessUnit
	cfg     media.Config
	kbps    []int
	keys    int
	stopped bool
}

func (f *fakeVideo) Name() string     { return "fake" }
func (f *fakeVideo) Codecs() []string { return []string{"h264", "av1"} }
func (f *fakeVideo) Start(ctx context.Context, c media.Config) error {
	f.cfg = c
	f.out = make(chan media.AccessUnit, 4)
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				select {
				case f.out <- media.AccessUnit{Data: []byte{0, 0, 0, 1, 0x65, byte(i)}, Key: i == 0}:
				default:
				}
			}
		}
	}()
	return nil
}
func (f *fakeVideo) Frames() <-chan media.AccessUnit { return f.out }
func (f *fakeVideo) SetBitrate(k int)                { f.mu.Lock(); f.kbps = append(f.kbps, k); f.mu.Unlock() }
func (f *fakeVideo) SetFPS(int)                      {}
func (f *fakeVideo) ForceKeyframe()                  { f.mu.Lock(); f.keys++; f.mu.Unlock() }
func (f *fakeVideo) Suspend(bool)                    {}
func (f *fakeVideo) Stats() media.Stats              { return media.Stats{Encoder: "fake", FPS: 50} }
func (f *fakeVideo) Stop()                           { f.mu.Lock(); f.stopped = true; f.mu.Unlock() }

type nopDesk struct {
	mu   sync.Mutex
	keys map[uint32]bool
}

func (d *nopDesk) Key(k uint32, down bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.keys == nil {
		d.keys = map[uint32]bool{}
	}
	d.keys[k] = down
	return nil
}
func (d *nopDesk) down(k uint32) bool        { d.mu.Lock(); defer d.mu.Unlock(); return d.keys[k] }
func (d *nopDesk) Button(uint8, bool) error  { return nil }
func (d *nopDesk) MoveAbs(_, _ uint16) error { return nil }
func (d *nopDesk) MoveRel(_, _ int16) error  { return nil }
func (d *nopDesk) Wheel(_, _ int16) error    { return nil }
func (d *nopDesk) Close() error              { return nil }

type fixture struct {
	mu    sync.Mutex
	priv  ed25519.PrivateKey
	addr  string
	video *fakeVideo
	desk  *nopDesk
	inj   *input.Injector
}

func setup(t *testing.T) *fixture {
	pub, priv, _ := ed25519.GenerateKey(nil)
	fx := &fixture{priv: priv, video: &fakeVideo{}, desk: &nopDesk{}}
	fx.inj = &input.Injector{Desktop: fx.desk}
	ag := New(Config{
		DesktopID: "desk-a", PublicKey: pub,
		NewVideo: func() media.VideoSource {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			fx.video = &fakeVideo{}
			return fx.video
		},
		Injector: fx.inj,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go ag.Serve(ctx, ln)
	fx.addr = ln.Addr().String()
	return fx
}

func (fx *fixture) tok(t *testing.T, desk string) string {
	s, _, err := token.Mint(fx.priv, token.Claims{User: "u", Desktop: desk, Session: "s", Host: "h", Entitlement: "eleet", Profile: "balanced"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func dial(t *testing.T, addr, tok string, codecs []string) (*protocol.LinkConn, net.Conn, protocol.Frame) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	lc := protocol.NewLinkConn(c)
	lc.WriteJSON(protocol.LinkHello, protocol.LinkHelloMsg{Token: tok, Codecs: codecs, Profile: "high", W: 3840, H: 2160, FPS: 120, Kbps: 99999})
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := lc.Read()
	if err != nil {
		t.Fatal(err)
	}
	return lc, c, f
}

func TestLinkHappyPath(t *testing.T) {
	fx := setup(t)
	lc, c, f := dial(t, fx.addr, fx.tok(t, "desk-a"), []string{"h264", "h265", "av1"})
	defer c.Close()
	if f.Kind != protocol.LinkReady {
		t.Fatalf("kind %x: %s", f.Kind, f.Payload)
	}
	var r protocol.LinkReadyMsg
	json.Unmarshal(f.Payload, &r)
	// token allows balanced only: the high request is clamped
	if r.Codec != "av1" || r.W != 1920 || r.H != 1080 || r.FPS != 60 {
		t.Fatalf("ready %+v", r)
	}
	if fx.video.cfg.Kbps != 15000 {
		t.Fatalf("kbps not clamped: %d", fx.video.cfg.Kbps)
	}
	gotVideo := false
	for !gotVideo {
		f, err := lc.Read()
		if err != nil {
			t.Fatal(err)
		}
		gotVideo = f.Kind == protocol.LinkVideo
	}
	// hold a key, then drop the link: the agent must release it
	lc.Write(protocol.LinkInput, protocol.Input{Type: protocol.InKey, Down: true, Keysym: 'w'}.Encode())
	lc.WriteJSON(protocol.LinkControl, protocol.LinkControlMsg{T: "bitrate", Kbps: 4000})
	waitFor(t, func() bool { return fx.desk.down('w') })
	c.Close()
	waitFor(t, func() bool { return !fx.desk.down('w') })
	waitFor(t, func() bool { fx.video.mu.Lock(); defer fx.video.mu.Unlock(); return fx.video.stopped })
	if len(fx.video.kbps) != 1 || fx.video.kbps[0] != 4000 {
		t.Fatal(fx.video.kbps)
	}
}

func TestLinkRejectsOtherDesktopAndReplay(t *testing.T) {
	fx := setup(t)
	_, c, f := dial(t, fx.addr, fx.tok(t, "desk-b"), []string{"h264"})
	c.Close()
	var bye protocol.ByeMsg
	json.Unmarshal(f.Payload, &bye)
	if f.Kind != protocol.LinkBye || bye.Code != protocol.CodeForbidden {
		t.Fatalf("other desktop: %x %+v", f.Kind, bye)
	}
	tok := fx.tok(t, "desk-a")
	_, c1, f := dial(t, fx.addr, tok, []string{"h264"})
	defer c1.Close()
	if f.Kind != protocol.LinkReady {
		t.Fatal("first use rejected")
	}
	_, c2, f := dial(t, fx.addr, tok, []string{"h264"})
	defer c2.Close()
	json.Unmarshal(f.Payload, &bye)
	if f.Kind != protocol.LinkBye || bye.Code != protocol.CodeBadToken {
		t.Fatalf("replay accepted: %x %+v", f.Kind, bye)
	}
}

func TestLinkHeartbeatTimeoutReleasesInput(t *testing.T) {
	fx := setup(t)
	lc, c, _ := dial(t, fx.addr, fx.tok(t, "desk-a"), []string{"h264"})
	defer c.Close()
	lc.Write(protocol.LinkInput, protocol.Input{Type: protocol.InKey, Down: true, Keysym: 'a'}.Encode())
	waitFor(t, func() bool { return fx.desk.down('a') })
	// send nothing (no pings) and keep the socket open
	time.Sleep(linkTimeout + 500*time.Millisecond)
	if fx.desk.down('a') {
		t.Fatal("key still held after heartbeat loss")
	}
}

func TestTakeover(t *testing.T) {
	fx := setup(t)
	lc1, c1, _ := dial(t, fx.addr, fx.tok(t, "desk-a"), []string{"h264"})
	defer c1.Close()
	_, c2, f := dial(t, fx.addr, fx.tok(t, "desk-a"), []string{"h264"})
	defer c2.Close()
	if f.Kind != protocol.LinkReady {
		t.Fatal("second link not ready")
	}
	for {
		f, err := lc1.Read()
		if err != nil {
			break // closed is acceptable too
		}
		if f.Kind == protocol.LinkBye {
			var b protocol.ByeMsg
			json.Unmarshal(f.Payload, &b)
			if b.Code != protocol.CodeTakenOver {
				t.Fatal(b)
			}
			break
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestFitSize(t *testing.T) {
	cases := [][6]int{
		{1280, 720, 1920, 1080, 1280, 720},   // never upscale
		{2560, 1440, 1920, 1080, 1920, 1080}, // same aspect
		{1920, 1200, 1920, 1080, 1728, 1080}, // 16:10 into 16:9
		{3440, 1440, 1920, 1080, 1920, 803},  // ultrawide
	}
	for _, c := range cases {
		if w, h := fitSize(c[0], c[1], c[2], c[3]); w != c[4] || h != c[5] {
			t.Errorf("fitSize(%d,%d,%d,%d) = %d,%d want %d,%d", c[0], c[1], c[2], c[3], w, h, c[4], c[5])
		}
	}
}

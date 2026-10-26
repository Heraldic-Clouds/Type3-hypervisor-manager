package input

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/protocol"
)

type fakeDesk struct{ log []string }

func (f *fakeDesk) Key(k uint32, d bool) error {
	f.log = append(f.log, fmt.Sprint("key", k, d))
	return nil
}
func (f *fakeDesk) Button(b uint8, d bool) error {
	f.log = append(f.log, fmt.Sprint("btn", b, d))
	return nil
}
func (f *fakeDesk) MoveAbs(x, y uint16) error  { return nil }
func (f *fakeDesk) MoveRel(dx, dy int16) error { return nil }
func (f *fakeDesk) Wheel(dx, dy int16) error   { return nil }
func (f *fakeDesk) Close() error               { return nil }

type fakePads struct{ states []uint32 }

func (f *fakePads) Connect(uint8) error    { return nil }
func (f *fakePads) Disconnect(uint8) error { return nil }
func (f *fakePads) State(p uint8, b uint32, a [4]int16, t [2]uint8) error {
	f.states = append(f.states, b)
	return nil
}
func (f *fakePads) Close() error { return nil }

func TestReleaseAll(t *testing.T) {
	d := &fakeDesk{}
	j := &Injector{Desktop: d}
	j.Apply(protocol.Input{Type: protocol.InKey, Down: true, Keysym: 'w'})
	j.Apply(protocol.Input{Type: protocol.InKey, Down: true, Keysym: 0xffe1})
	j.Apply(protocol.Input{Type: protocol.InButton, Down: true, Button: 1})
	if j.Held() != 3 {
		t.Fatal(j.Held())
	}
	j.ReleaseAll()
	if j.Held() != 0 {
		t.Fatal("still held")
	}
	// releases are idempotent: a late key-up after ReleaseAll is not re-sent
	n := len(d.log)
	j.Apply(protocol.Input{Type: protocol.InKey, Down: false, Keysym: 'w'})
	if len(d.log) != n {
		t.Fatal("duplicate release injected")
	}
}

func TestGamepadStaleSnapshots(t *testing.T) {
	p := &fakePads{}
	j := &Injector{Desktop: &fakeDesk{}, Pads: p}
	j.Apply(protocol.Input{Type: protocol.InGamepad, Seq: 10, Buttons: 1})
	j.Apply(protocol.Input{Type: protocol.InGamepad, Seq: 9, Buttons: 0}) // reordered, dropped
	j.Apply(protocol.Input{Type: protocol.InGamepad, Seq: 11, Buttons: 2})
	if len(p.states) != 2 || p.states[1] != 2 {
		t.Fatal(p.states)
	}
	j.ReleaseAll()
	if p.states[len(p.states)-1] != 0 {
		t.Fatal("pad not centred")
	}
	// after a release the next snapshot is accepted whatever its seq
	j.Apply(protocol.Input{Type: protocol.InGamepad, Seq: 3, Buttons: 4})
	if p.states[len(p.states)-1] != 4 {
		t.Fatal(p.states)
	}
}

// TestXTest runs against a real Xvfb when available.
func TestXTest(t *testing.T) {
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("Xvfb not installed")
	}
	disp := ":97"
	cmd := exec.Command("Xvfb", disp, "-screen", "0", "640x480x24", "-nolisten", "tcp")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	var x *X11
	var err error
	for i := 0; i < 50; i++ {
		if x, err = OpenX11(disp); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	if w, h := x.Size(); w != 640 || h != 480 {
		t.Fatal(w, h)
	}
	if err := x.MoveAbs(32768, 65535); err != nil {
		t.Fatal(err)
	}
	px, py, _ := x.QueryPointer()
	if px != 319 || py != 479 {
		t.Fatalf("pointer at %d,%d", px, py)
	}
	x.MoveRel(-10, -20)
	px, py, _ = x.QueryPointer()
	if px != 309 || py != 459 {
		t.Fatalf("relative pointer at %d,%d", px, py)
	}
	for _, ks := range []uint32{'a', 0xff0d, 0x010003a9 /* Ω, unmapped */} {
		if err := x.Key(ks, true); err != nil {
			t.Fatalf("key %x: %v", ks, err)
		}
		if err := x.Key(ks, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.Wheel(0, -240); err != nil {
		t.Fatal(err)
	}
}

func TestUInputAvailable(t *testing.T) {
	u := &UInput{}
	if err := u.Available(); err != nil {
		t.Skip("no /dev/uinput:", err)
	}
	if os.Getenv("MEA_TEST_UINPUT") == "" {
		t.Skip("set MEA_TEST_UINPUT=1 to create a virtual pad")
	}
	if err := u.Connect(0); err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if err := u.State(0, 1|1<<12, [4]int16{100, -100, 0, 0}, [2]uint8{0, 255}); err != nil {
		t.Fatal(err)
	}
}

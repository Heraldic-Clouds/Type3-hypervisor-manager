package input

import (
	"errors"
	"fmt"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

const (
	xKeyPress      = 2
	xKeyRelease    = 3
	xButtonPress   = 4
	xButtonRelease = 5
	xMotion        = 6
)

// X11 injects keyboard and mouse input through the XTEST extension of the
// container's own X server (DISPLAY inside the container). It can only reach
// the display it was opened on.
type X11 struct {
	conn *xgb.Conn
	root xproto.Window
	w, h int

	mu      sync.Mutex
	keymap  map[uint32]xproto.Keycode
	spare   xproto.Keycode // keycode remapped for keysyms with no mapping
	minCode xproto.Keycode
	perCode int
}

// OpenX11 connects to display (e.g. ":0"; empty uses $DISPLAY).
func OpenX11(display string) (*X11, error) {
	c, err := xgb.NewConnDisplay(display)
	if err != nil {
		return nil, fmt.Errorf("x11: %w", err)
	}
	if err := xtest.Init(c); err != nil {
		c.Close()
		return nil, fmt.Errorf("x11: XTEST unavailable: %w", err)
	}
	setup := xproto.Setup(c)
	scr := setup.DefaultScreen(c)
	x := &X11{conn: c, root: scr.Root, w: int(scr.WidthInPixels), h: int(scr.HeightInPixels)}
	if err := x.loadKeymap(setup.MinKeycode, setup.MaxKeycode); err != nil {
		c.Close()
		return nil, err
	}
	return x, nil
}

func (x *X11) loadKeymap(min, max xproto.Keycode) error {
	r, err := xproto.GetKeyboardMapping(x.conn, min, byte(max-min+1)).Reply()
	if err != nil {
		return fmt.Errorf("x11: keymap: %w", err)
	}
	x.keymap = map[uint32]xproto.Keycode{}
	x.minCode, x.perCode = min, int(r.KeysymsPerKeycode)
	for i := 0; i < int(max-min+1); i++ {
		empty := true
		for j := 0; j < x.perCode; j++ {
			ks := uint32(r.Keysyms[i*x.perCode+j])
			if ks == 0 {
				continue
			}
			empty = false
			if _, ok := x.keymap[ks]; !ok {
				x.keymap[ks] = min + xproto.Keycode(i)
			}
		}
		if empty && x.spare == 0 && i > 8 {
			x.spare = min + xproto.Keycode(i)
		}
	}
	return nil
}

// Size returns the current root window size.
func (x *X11) Size() (int, int) {
	if g, err := xproto.GetGeometry(x.conn, xproto.Drawable(x.root)).Reply(); err == nil {
		x.w, x.h = int(g.Width), int(g.Height)
	}
	return x.w, x.h
}

func (x *X11) fake(typ, detail byte, rx, ry int16) error {
	return xtest.FakeInputChecked(x.conn, typ, detail, 0, x.root, rx, ry, 0).Check()
}

func (x *X11) code(keysym uint32) (xproto.Keycode, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if kc, ok := x.keymap[keysym]; ok {
		return kc, nil
	}
	if x.spare == 0 {
		return 0, errors.New("x11: keysym not mapped")
	}
	// Map the keysym onto the spare keycode (as xdotool does) so that
	// Unicode text and uncommon keys still reach applications.
	syms := make([]xproto.Keysym, x.perCode)
	for i := range syms {
		syms[i] = xproto.Keysym(keysym)
	}
	if err := xproto.ChangeKeyboardMappingChecked(x.conn, 1, x.spare, byte(x.perCode), syms).Check(); err != nil {
		return 0, err
	}
	for ks, kc := range x.keymap {
		if kc == x.spare {
			delete(x.keymap, ks)
		}
	}
	x.keymap[keysym] = x.spare
	return x.spare, nil
}

func (x *X11) Key(keysym uint32, down bool) error {
	kc, err := x.code(keysym)
	if err != nil {
		return err
	}
	t := byte(xKeyRelease)
	if down {
		t = xKeyPress
	}
	return x.fake(t, byte(kc), 0, 0)
}

// Browser button numbers → X11 buttons (8/9 are back/forward in both).
func (x *X11) Button(b uint8, down bool) error {
	t := byte(xButtonRelease)
	if down {
		t = xButtonPress
	}
	return x.fake(t, b, 0, 0)
}

func (x *X11) MoveAbs(ax, ay uint16) error {
	w, h := x.w, x.h
	px := int16(int(ax) * (w - 1) / 65535)
	py := int16(int(ay) * (h - 1) / 65535)
	return x.fake(xMotion, 0, px, py)
}

func (x *X11) MoveRel(dx, dy int16) error { return x.fake(xMotion, 1, dx, dy) }

func (x *X11) Wheel(dx, dy int16) error {
	click := func(b byte, n int) error {
		for ; n > 0; n-- {
			if err := x.fake(xButtonPress, b, 0, 0); err != nil {
				return err
			}
			if err := x.fake(xButtonRelease, b, 0, 0); err != nil {
				return err
			}
		}
		return nil
	}
	notches := func(v int16) int {
		n := (int(v) + 60*sign(v)) / 120
		if n == 0 && v != 0 {
			return sign(v)
		}
		return n
	}
	if ny := notches(dy); ny < 0 {
		if err := click(4, -ny); err != nil {
			return err
		}
	} else if err := click(5, ny); err != nil {
		return err
	}
	if nx := notches(dx); nx < 0 {
		return click(6, -nx)
	} else {
		return click(7, nx)
	}
}

func sign(v int16) int {
	if v < 0 {
		return -1
	}
	if v > 0 {
		return 1
	}
	return 0
}

func (x *X11) Close() error { x.conn.Close(); return nil }

// QueryPointer returns the pointer position (tests).
func (x *X11) QueryPointer() (int, int, error) {
	r, err := xproto.QueryPointer(x.conn, x.root).Reply()
	if err != nil {
		return 0, 0, err
	}
	return int(r.RootX), int(r.RootY), nil
}

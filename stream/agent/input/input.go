// Package input injects MEA Stream input events into the agent's own desktop
// session and guarantees that nothing stays pressed when a client goes away
// (docs/PROTOCOL.md §3.3).
package input

import (
	"log/slog"
	"sync"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/protocol"
)

// Pointer and keyboard backend (XTest on X11).
type Desktop interface {
	Key(keysym uint32, down bool) error
	Button(button uint8, down bool) error
	MoveAbs(x, y uint16) error // 0..65535 over the screen
	MoveRel(dx, dy int16) error
	Wheel(dx, dy int16) error // 120 units = one notch
	Close() error
}

// Gamepad backend (uinput).
type Pads interface {
	Connect(pad uint8) error
	Disconnect(pad uint8) error
	State(pad uint8, buttons uint32, axes [4]int16, triggers [2]uint8) error
	Close() error
}

// Injector applies events and tracks pressed state.
type Injector struct {
	Desktop Desktop
	Pads    Pads // may be nil when uinput is unavailable
	Log     *slog.Logger

	mu       sync.Mutex
	keys     map[uint32]bool
	buttons  map[uint8]bool
	padSeq   map[uint8]uint16
	padSeen  map[uint8]bool
	padOn    map[uint8]bool
	wheelAcc [2]int
}

func (j *Injector) init() {
	if j.keys == nil {
		j.keys = map[uint32]bool{}
		j.buttons = map[uint8]bool{}
		j.padSeq = map[uint8]uint16{}
		j.padSeen = map[uint8]bool{}
		j.padOn = map[uint8]bool{}
	}
	if j.Log == nil {
		j.Log = slog.Default()
	}
}

// Apply injects one event.
func (j *Injector) Apply(in protocol.Input) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.init()
	switch in.Type {
	case protocol.InKey:
		if in.Down == j.keys[in.Keysym] && !in.Down {
			return nil // duplicate release
		}
		if err := j.Desktop.Key(in.Keysym, in.Down); err != nil {
			return err
		}
		if in.Down {
			j.keys[in.Keysym] = true
		} else {
			delete(j.keys, in.Keysym)
		}
	case protocol.InButton:
		if !in.Down && !j.buttons[in.Button] {
			return nil
		}
		if err := j.Desktop.Button(in.Button, in.Down); err != nil {
			return err
		}
		if in.Down {
			j.buttons[in.Button] = true
		} else {
			delete(j.buttons, in.Button)
		}
	case protocol.InMotionAbs:
		return j.Desktop.MoveAbs(in.X, in.Y)
	case protocol.InMotionRel:
		return j.Desktop.MoveRel(in.DX, in.DY)
	case protocol.InWheel:
		return j.Desktop.Wheel(in.DX, in.DY)
	case protocol.InGamepadConn:
		if j.Pads == nil {
			return nil
		}
		if in.Connected && !j.padOn[in.Pad] {
			if err := j.Pads.Connect(in.Pad); err != nil {
				return err
			}
			j.padOn[in.Pad] = true
			delete(j.padSeen, in.Pad)
		} else if !in.Connected && j.padOn[in.Pad] {
			delete(j.padOn, in.Pad)
			delete(j.padSeen, in.Pad)
			return j.Pads.Disconnect(in.Pad)
		}
	case protocol.InGamepad:
		if j.Pads == nil {
			return nil
		}
		if j.padSeen[in.Pad] && !protocol.SeqNewer(in.Seq, j.padSeq[in.Pad]) {
			return nil // stale snapshot
		}
		if !j.padOn[in.Pad] {
			if err := j.Pads.Connect(in.Pad); err != nil {
				return err
			}
			j.padOn[in.Pad] = true
		}
		j.padSeen[in.Pad], j.padSeq[in.Pad] = true, in.Seq
		return j.Pads.State(in.Pad, in.Buttons, in.Axes, in.Triggers)
	case protocol.InText:
		for _, r := range in.Text {
			ks := RuneKeysym(r)
			if err := j.Desktop.Key(ks, true); err != nil {
				return err
			}
			if err := j.Desktop.Key(ks, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReleaseAll releases every held key and button and centres every pad.
func (j *Injector) ReleaseAll() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.init()
	for ks := range j.keys {
		if err := j.Desktop.Key(ks, false); err != nil {
			j.Log.Warn("release key", "keysym", ks, "err", err)
		}
		delete(j.keys, ks)
	}
	for b := range j.buttons {
		if err := j.Desktop.Button(b, false); err != nil {
			j.Log.Warn("release button", "button", b, "err", err)
		}
		delete(j.buttons, b)
	}
	if j.Pads != nil {
		for p := range j.padOn {
			if err := j.Pads.State(p, 0, [4]int16{}, [2]uint8{}); err != nil {
				j.Log.Warn("centre pad", "pad", p, "err", err)
			}
		}
	}
	clear(j.padSeen)
}

// Held reports the number of pressed keys and buttons (tests, metrics).
func (j *Injector) Held() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.keys) + len(j.buttons)
}

// Close releases everything and closes backends.
func (j *Injector) Close() {
	j.ReleaseAll()
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Pads != nil {
		j.Pads.Close()
	}
	j.Desktop.Close()
}

// RuneKeysym maps a Unicode code point to an X11 keysym.
func RuneKeysym(r rune) uint32 {
	switch {
	case r == '\n':
		return 0xff0d
	case r == '\t':
		return 0xff09
	case r >= 0x20 && r <= 0x7e, r >= 0xa0 && r <= 0xff:
		return uint32(r)
	}
	return 0x01000000 | uint32(r)
}

package protocol

import (
	"encoding/binary"
	"errors"
	"unicode/utf8"
)

// Input event types (§3.2).
const (
	InKey         = 0x01
	InButton      = 0x02
	InMotionRel   = 0x03
	InMotionAbs   = 0x04
	InWheel       = 0x05
	InGamepad     = 0x06
	InGamepadConn = 0x07
	InText        = 0x08
)

const MaxText = 8 << 10

var ErrInput = errors.New("protocol: malformed input event")

// Input is one decoded input event.
type Input struct {
	Type uint8

	Down   bool   // key, button
	Keysym uint32 // key
	Button uint8  // button

	DX, DY int16  // motion-rel, wheel
	X, Y   uint16 // motion-abs

	Pad       uint8 // gamepad, gamepad-conn
	Seq       uint16
	Buttons   uint32
	Axes      [4]int16
	Triggers  [2]uint8
	Connected bool

	Text string
}

// Reliable reports which channel an event type belongs on.
func Reliable(t uint8) bool {
	switch t {
	case InKey, InButton, InGamepadConn, InText:
		return true
	}
	return false
}

var le = binary.LittleEndian

func b2u(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// Encode serializes an event.
func (in Input) Encode() []byte {
	switch in.Type {
	case InKey:
		b := []byte{InKey, b2u(in.Down), 0, 0, 0, 0}
		le.PutUint32(b[2:], in.Keysym)
		return b
	case InButton:
		return []byte{InButton, b2u(in.Down), in.Button}
	case InMotionRel, InWheel:
		b := []byte{in.Type, 0, 0, 0, 0}
		le.PutUint16(b[1:], uint16(in.DX))
		le.PutUint16(b[3:], uint16(in.DY))
		return b
	case InMotionAbs:
		b := []byte{InMotionAbs, 0, 0, 0, 0}
		le.PutUint16(b[1:], in.X)
		le.PutUint16(b[3:], in.Y)
		return b
	case InGamepad:
		b := make([]byte, 20)
		b[0], b[1] = InGamepad, in.Pad
		le.PutUint16(b[2:], in.Seq)
		le.PutUint32(b[4:], in.Buttons)
		for i, a := range in.Axes {
			le.PutUint16(b[8+2*i:], uint16(a))
		}
		b[16], b[17] = in.Triggers[0], in.Triggers[1]
		return b[:18]
	case InGamepadConn:
		return []byte{InGamepadConn, in.Pad, b2u(in.Connected)}
	case InText:
		t := in.Text
		if len(t) > MaxText {
			t = t[:MaxText]
		}
		b := make([]byte, 3+len(t))
		b[0] = InText
		le.PutUint16(b[1:], uint16(len(t)))
		copy(b[3:], t)
		return b
	}
	return nil
}

// DecodeInput parses one event. Unknown types return ok=false with no error
// (§8: drop, don't fail).
func DecodeInput(b []byte) (in Input, ok bool, err error) {
	if len(b) == 0 {
		return in, false, ErrInput
	}
	in.Type = b[0]
	need := map[uint8]int{InKey: 6, InButton: 3, InMotionRel: 5, InMotionAbs: 5, InWheel: 5, InGamepad: 18, InGamepadConn: 3, InText: 3}
	n, known := need[in.Type]
	if !known {
		return in, false, nil
	}
	if len(b) < n {
		return in, false, ErrInput
	}
	switch in.Type {
	case InKey:
		in.Down, in.Keysym = b[1] != 0, le.Uint32(b[2:])
	case InButton:
		in.Down, in.Button = b[1] != 0, b[2]
		if in.Button == 0 || in.Button > 9 {
			return in, false, ErrInput
		}
	case InMotionRel, InWheel:
		in.DX, in.DY = int16(le.Uint16(b[1:])), int16(le.Uint16(b[3:]))
	case InMotionAbs:
		in.X, in.Y = le.Uint16(b[1:]), le.Uint16(b[3:])
	case InGamepad:
		in.Pad, in.Seq, in.Buttons = b[1], le.Uint16(b[2:]), le.Uint32(b[4:])
		for i := range in.Axes {
			in.Axes[i] = int16(le.Uint16(b[8+2*i:]))
		}
		in.Triggers = [2]uint8{b[16], b[17]}
	case InGamepadConn:
		in.Pad, in.Connected = b[1], b[2] != 0
	case InText:
		l := int(le.Uint16(b[1:]))
		if l > MaxText || len(b) < 3+l || !utf8.Valid(b[3:3+l]) {
			return in, false, ErrInput
		}
		in.Text = string(b[3 : 3+l])
	}
	if in.Pad > 3 && (in.Type == InGamepad || in.Type == InGamepadConn) {
		return in, false, ErrInput
	}
	return in, true, nil
}

// SeqNewer reports whether a is newer than b with 16-bit wrap-around.
func SeqNewer(a, b uint16) bool { return a != b && uint16(a-b) < 0x8000 }

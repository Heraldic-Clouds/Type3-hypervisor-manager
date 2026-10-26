package input

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// uinput ioctls (linux/uinput.h).
const (
	uiDevCreate  = 0x5501
	uiDevDestroy = 0x5502
	uiDevSetup   = 0x405c5503
	uiAbsSetup   = 0x401c5504
	uiSetEvBit   = 0x40045564
	uiSetKeyBit  = 0x40045565
	uiSetAbsBit  = 0x40045567

	evSyn = 0x00
	evKey = 0x01
	evAbs = 0x03

	absX, absY, absZ, absRX, absRY, absRZ = 0x00, 0x01, 0x02, 0x03, 0x04, 0x05
	absHat0X, absHat0Y                    = 0x10, 0x11
)

// W3C standard gamepad button index → Linux key code (Xbox 360 layout, which
// SDL and Steam recognise). Indices 6/7 (triggers) and 12–15 (d-pad) are
// reported as axes instead.
var padButtons = map[int]uint16{
	0: 0x130, 1: 0x131, 2: 0x134, 3: 0x133, // A B X Y (BTN_SOUTH EAST WEST NORTH)
	4: 0x136, 5: 0x137, // LB RB
	8: 0x13a, 9: 0x13b, // back start
	10: 0x13d, 11: 0x13e, // thumbs
	16: 0x13c, // guide
}

type uinputPad struct {
	f    *os.File
	last [32]bool
}

// UInput creates one virtual Xbox 360-style controller per browser pad in the
// container's /dev/uinput. LXD must pass /dev/uinput into the container (the
// Eleet profile does); devices are destroyed when the pad disconnects.
type UInput struct {
	Path string // default /dev/uinput
	mu   sync.Mutex
	pads map[uint8]*uinputPad
}

func (u *UInput) path() string {
	if u.Path != "" {
		return u.Path
	}
	return "/dev/uinput"
}

// Available reports whether uinput can be opened.
func (u *UInput) Available() error {
	f, err := os.OpenFile(u.path(), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

func ioctl(f *os.File, req uintptr, arg uintptr) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), req, arg)
	if e != 0 {
		return e
	}
	return nil
}

func (u *UInput) Connect(pad uint8) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.pads == nil {
		u.pads = map[uint8]*uinputPad{}
	}
	if _, ok := u.pads[pad]; ok {
		return nil
	}
	f, err := os.OpenFile(u.path(), os.O_WRONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("uinput: %w", err)
	}
	fail := func(err error) error { f.Close(); return fmt.Errorf("uinput: %w", err) }
	for _, ev := range []uintptr{evKey, evAbs, evSyn} {
		if err := ioctl(f, uiSetEvBit, ev); err != nil {
			return fail(err)
		}
	}
	for _, k := range padButtons {
		if err := ioctl(f, uiSetKeyBit, uintptr(k)); err != nil {
			return fail(err)
		}
	}
	abs := []struct {
		code     uint16
		min, max int32
		flat     int32
	}{
		{absX, -32768, 32767, 128}, {absY, -32768, 32767, 128},
		{absRX, -32768, 32767, 128}, {absRY, -32768, 32767, 128},
		{absZ, 0, 255, 0}, {absRZ, 0, 255, 0},
		{absHat0X, -1, 1, 0}, {absHat0Y, -1, 1, 0},
	}
	for _, a := range abs {
		if err := ioctl(f, uiSetAbsBit, uintptr(a.code)); err != nil {
			return fail(err)
		}
		// struct uinput_abs_setup { u16 code; (pad) ; input_absinfo {value,min,max,fuzz,flat,res} }
		var s [28]byte
		binary.LittleEndian.PutUint16(s[0:], a.code)
		binary.LittleEndian.PutUint32(s[8:], uint32(a.min))
		binary.LittleEndian.PutUint32(s[12:], uint32(a.max))
		binary.LittleEndian.PutUint32(s[20:], uint32(a.flat))
		if err := ioctl(f, uiAbsSetup, uintptr(unsafe.Pointer(&s[0]))); err != nil {
			return fail(err)
		}
	}
	// struct uinput_setup { input_id {bustype,vendor,product,version}; char name[80]; u32 ff_effects_max; }
	var s [92]byte
	binary.LittleEndian.PutUint16(s[0:], 0x03) // BUS_USB
	binary.LittleEndian.PutUint16(s[2:], 0x045e)
	binary.LittleEndian.PutUint16(s[4:], 0x028e)
	binary.LittleEndian.PutUint16(s[6:], 0x0110)
	copy(s[8:88], fmt.Sprintf("MEA Stream Gamepad %d", pad))
	if err := ioctl(f, uiDevSetup, uintptr(unsafe.Pointer(&s[0]))); err != nil {
		return fail(err)
	}
	if err := ioctl(f, uiDevCreate, 0); err != nil {
		return fail(err)
	}
	u.pads[pad] = &uinputPad{f: f}
	return nil
}

func (u *UInput) Disconnect(pad uint8) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	p, ok := u.pads[pad]
	if !ok {
		return nil
	}
	delete(u.pads, pad)
	ioctl(p.f, uiDevDestroy, 0)
	return p.f.Close()
}

func writeEvents(f *os.File, evs [][3]int32) error {
	b := make([]byte, 24*(len(evs)+1))
	for i, e := range append(evs, [3]int32{evSyn, 0, 0}) {
		o := 24 * i
		binary.LittleEndian.PutUint16(b[o+16:], uint16(e[0]))
		binary.LittleEndian.PutUint16(b[o+18:], uint16(e[1]))
		binary.LittleEndian.PutUint32(b[o+20:], uint32(e[2]))
	}
	_, err := f.Write(b)
	return err
}

func (u *UInput) State(pad uint8, buttons uint32, axes [4]int16, triggers [2]uint8) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	p, ok := u.pads[pad]
	if !ok {
		return errors.New("uinput: pad not connected")
	}
	bit := func(i int) bool { return buttons&(1<<i) != 0 }
	var evs [][3]int32
	for i, code := range padButtons {
		if v := bit(i); v != p.last[i] {
			p.last[i] = v
			val := int32(0)
			if v {
				val = 1
			}
			evs = append(evs, [3]int32{evKey, int32(code), val})
		}
	}
	hat := func(neg, pos int) int32 {
		switch {
		case bit(neg) && !bit(pos):
			return -1
		case bit(pos) && !bit(neg):
			return 1
		}
		return 0
	}
	evs = append(evs,
		[3]int32{evAbs, absX, int32(axes[0])}, [3]int32{evAbs, absY, int32(axes[1])},
		[3]int32{evAbs, absRX, int32(axes[2])}, [3]int32{evAbs, absRY, int32(axes[3])},
		[3]int32{evAbs, absZ, int32(triggers[0])}, [3]int32{evAbs, absRZ, int32(triggers[1])},
		[3]int32{evAbs, absHat0X, hat(14, 15)}, [3]int32{evAbs, absHat0Y, hat(12, 13)},
	)
	return writeEvents(p.f, evs)
}

func (u *UInput) Close() error {
	u.mu.Lock()
	ids := make([]uint8, 0, len(u.pads))
	for id := range u.pads {
		ids = append(ids, id)
	}
	u.mu.Unlock()
	for _, id := range ids {
		u.Disconnect(id)
	}
	return nil
}

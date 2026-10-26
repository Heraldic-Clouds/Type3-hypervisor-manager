// xprobe prints the X pointer position and the keysyms currently held, so the
// browser end-to-end test can check that input reached the desktop.
// "xprobe paint" animates the screen at 60 Hz so the stream carries motion.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

func main() {
	c, err := xgb.NewConn()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer c.Close()
	setup := xproto.Setup(c)
	root := setup.DefaultScreen(c).Root
	if len(os.Args) > 1 && os.Args[1] == "paint" {
		paint(c, setup.DefaultScreen(c))
		return
	}
	p, err := xproto.QueryPointer(c, root).Reply()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	km, err := xproto.QueryKeymap(c).Reply()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	first, n := setup.MinKeycode, byte(setup.MaxKeycode-setup.MinKeycode+1)
	m, err := xproto.GetKeyboardMapping(c, first, n).Reply()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	per := int(m.KeysymsPerKeycode)
	keys := []uint32{}
	for code := int(first); code <= int(setup.MaxKeycode); code++ {
		if km.Keys[code/8]&(1<<(code%8)) != 0 {
			keys = append(keys, uint32(m.Keysyms[(code-int(first))*per]))
		}
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{
		"x": p.RootX, "y": p.RootY, "buttons": p.Mask >> 8, "keys": keys,
	})
}

// paint sweeps a bar across the root window forever.
func paint(c *xgb.Conn, scr *xproto.ScreenInfo) {
	gc, _ := xproto.NewGcontextId(c)
	xproto.CreateGC(c, gc, xproto.Drawable(scr.Root), 0, nil)
	w, h := int(scr.WidthInPixels), int(scr.HeightInPixels)
	for i := 0; ; i++ {
		color := uint32(i*2654435761) & 0xffffff
		xproto.ChangeGC(c, gc, xproto.GcForeground, []uint32{color})
		x := (i * 16) % w
		xproto.PolyFillRectangle(c, xproto.Drawable(scr.Root), gc, []xproto.Rectangle{{X: int16(x), Y: 0, Width: 64, Height: uint16(h)}})
		xproto.GetInputFocus(c).Reply() // flush and pace
		time.Sleep(time.Second / 60)
	}
}

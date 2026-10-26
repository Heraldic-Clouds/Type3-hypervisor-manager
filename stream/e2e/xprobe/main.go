// xprobe prints the X pointer position and the keysyms currently held, so the
// browser end-to-end test can check that input reached the desktop.
package main

import (
	"encoding/json"
	"fmt"
	"os"

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

package protocol

import (
	"bytes"
	"encoding/json"
	"net"
	"testing"
)

func TestSelectCodec(t *testing.T) {
	cases := []struct {
		client, agent []string
		want          string
	}{
		{[]string{"h264", "av1"}, []string{"h264", "h265", "av1"}, "av1"},
		{[]string{"h264", "h265"}, []string{"h264", "av1"}, "h264"},
		{[]string{"h264"}, []string{"h264"}, "h264"},
		{[]string{"vp8"}, []string{"h264"}, ""},
	}
	for _, c := range cases {
		if got := SelectCodec(c.client, c.agent); got != c.want {
			t.Errorf("%v %v: got %q want %q", c.client, c.agent, got, c.want)
		}
	}
}

func TestClampProfile(t *testing.T) {
	if p := ClampProfile("high", "balanced"); p.Name != "balanced" {
		t.Fatal(p)
	}
	if p := ClampProfile("economy", "high"); p.Name != "economy" {
		t.Fatal(p)
	}
	if p := ClampProfile("bogus", "high"); p.Name != "balanced" {
		t.Fatal(p)
	}
}

func TestInputRoundTrip(t *testing.T) {
	events := []Input{
		{Type: InKey, Down: true, Keysym: 0xff0d},
		{Type: InButton, Down: false, Button: 3},
		{Type: InMotionRel, DX: -120, DY: 7},
		{Type: InMotionAbs, X: 65535, Y: 1},
		{Type: InWheel, DY: -240},
		{Type: InGamepad, Pad: 1, Seq: 65535, Buttons: 1<<0 | 1<<16, Axes: [4]int16{-32768, 32767, 0, 5}, Triggers: [2]uint8{255, 3}},
		{Type: InGamepadConn, Pad: 2, Connected: true},
		{Type: InText, Text: "héllo"},
	}
	for _, e := range events {
		got, ok, err := DecodeInput(e.Encode())
		if err != nil || !ok {
			t.Fatalf("%x: %v %v", e.Type, ok, err)
		}
		if got != e {
			t.Fatalf("round trip %+v != %+v", got, e)
		}
	}
}

func TestInputRejects(t *testing.T) {
	if _, ok, err := DecodeInput([]byte{0x99, 1, 2}); ok || err != nil {
		t.Fatal("unknown type must be dropped silently")
	}
	bad := [][]byte{
		{},
		{InKey, 1},
		{InButton, 1, 0},
		{InButton, 1, 200},
		{InGamepadConn, 9, 1},
		{InText, 5, 0, 'a'},
		{InText, 2, 0, 0xff, 0xfe},
	}
	for _, b := range bad {
		if _, ok, err := DecodeInput(b); ok || err == nil {
			t.Errorf("%x accepted", b)
		}
	}
}

func TestSeqNewer(t *testing.T) {
	if !SeqNewer(1, 0) || SeqNewer(0, 1) || !SeqNewer(0, 65535) || SeqNewer(5, 5) {
		t.Fatal("seq compare")
	}
}

func TestLink(t *testing.T) {
	a, b := net.Pipe()
	ca, cb := NewLinkConn(a), NewLinkConn(b)
	go func() {
		ca.WriteJSON(LinkHello, LinkHelloMsg{Token: "x", Codecs: []string{"h264"}})
		ca.Write(LinkVideo, VideoPayload(true, 42, []byte{0, 0, 0, 1, 0x65}))
	}()
	f, err := cb.Read()
	if err != nil || f.Kind != LinkHello {
		t.Fatal(err, f.Kind)
	}
	var h LinkHelloMsg
	json.Unmarshal(f.Payload, &h)
	if h.Token != "x" {
		t.Fatal(h)
	}
	f, _ = cb.Read()
	key, pts, au, err := ParseVideo(f.Payload)
	if err != nil || !key || pts != 42 || !bytes.Equal(au, []byte{0, 0, 0, 1, 0x65}) {
		t.Fatal(key, pts, au, err)
	}
}

func TestVersion(t *testing.T) {
	var c Control
	json.Unmarshal([]byte(`{"t":"hello","v":["mea-stream/2","mea-stream/1"],"future":1}`), &c)
	if v, ok := NegotiateVersion(c.Versions()); !ok || v != Version {
		t.Fatal(c.Versions())
	}
}

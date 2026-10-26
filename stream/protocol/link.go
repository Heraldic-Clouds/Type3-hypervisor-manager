package protocol

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// Agent link frame kinds (§6).
const (
	LinkHello   = 0x01
	LinkReady   = 0x02
	LinkVideo   = 0x10
	LinkAudio   = 0x11
	LinkInput   = 0x20
	LinkControl = 0x21
	LinkStats   = 0x30
	LinkBye     = 0x7F

	MaxFrame = 4 << 20
)

var ErrFrameTooLarge = errors.New("protocol: link frame too large")

// LinkHelloMsg is sent by the gateway when it attaches to an agent.
type LinkHelloMsg struct {
	Token   string   `json:"token"`
	Codecs  []string `json:"codecs"`
	Profile string   `json:"profile"`
	W       int      `json:"w"`
	H       int      `json:"h"`
	FPS     int      `json:"fps"`
	Kbps    int      `json:"kbps"`
}

// LinkReadyMsg is the agent's answer.
type LinkReadyMsg struct {
	Codec   string   `json:"codec"`
	Codecs  []string `json:"codecs"`
	W       int      `json:"w"`
	H       int      `json:"h"`
	FPS     int      `json:"fps"`
	Audio   bool     `json:"audio"`
	Encoder string   `json:"encoder"`
}

// LinkControlMsg carries gateway → agent commands.
type LinkControlMsg struct {
	T     string `json:"t"` // configure, keyframe, release, suspend, resume, bitrate, codec
	W     int    `json:"w,omitempty"`
	H     int    `json:"h,omitempty"`
	FPS   int    `json:"fps,omitempty"`
	Kbps  int    `json:"kbps,omitempty"`
	Codec string `json:"codec,omitempty"`
}

// LinkStatsMsg is agent → gateway encoder/capture telemetry.
type LinkStatsMsg struct {
	FPS       float64 `json:"fps"`
	Kbps      float64 `json:"kbps"`
	EncMs     float64 `json:"encMs"`
	CapMs     float64 `json:"capMs"`
	Idle      bool    `json:"idle"`
	Suspended bool    `json:"suspended"`
	Encoder   string  `json:"encoder"`
	Frames    uint64  `json:"frames"`
	Skipped   uint64  `json:"skipped"`
	Errors    uint64  `json:"errors"`
	W         int     `json:"w"`
	H         int     `json:"h"`
}

type ByeMsg struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

// Frame is one link frame.
type Frame struct {
	Kind    byte
	Payload []byte
}

// MediaHeader is the prefix of VIDEO (flags+pts) and AUDIO (pts) payloads.
func VideoPayload(key bool, ptsUS uint64, au []byte) []byte {
	b := make([]byte, 9+len(au))
	b[0] = b2u(key)
	binary.BigEndian.PutUint64(b[1:], ptsUS)
	copy(b[9:], au)
	return b
}

func ParseVideo(p []byte) (key bool, ptsUS uint64, au []byte, err error) {
	if len(p) < 9 {
		return false, 0, nil, ErrInput
	}
	return p[0]&1 != 0, binary.BigEndian.Uint64(p[1:]), p[9:], nil
}

func AudioPayload(ptsUS uint64, pkt []byte) []byte {
	b := make([]byte, 8+len(pkt))
	binary.BigEndian.PutUint64(b, ptsUS)
	copy(b[8:], pkt)
	return b
}

func ParseAudio(p []byte) (uint64, []byte, error) {
	if len(p) < 8 {
		return 0, nil, ErrInput
	}
	return binary.BigEndian.Uint64(p), p[8:], nil
}

// LinkConn frames messages over a stream. Writes are serialized.
type LinkConn struct {
	r  *bufio.Reader
	w  io.Writer
	mu sync.Mutex
}

func NewLinkConn(rw io.ReadWriter) *LinkConn {
	return &LinkConn{r: bufio.NewReaderSize(rw, 64<<10), w: rw}
}

func (c *LinkConn) Write(kind byte, payload []byte) error {
	if len(payload)+1 > MaxFrame {
		return ErrFrameTooLarge
	}
	var h [5]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(payload)+1))
	h[4] = kind
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.w.Write(h[:]); err != nil {
		return err
	}
	_, err := c.w.Write(payload)
	return err
}

func (c *LinkConn) WriteJSON(kind byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(kind, b)
}

// Read returns the next frame. Not safe for concurrent readers.
func (c *LinkConn) Read() (Frame, error) {
	var h [5]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(h[:4])
	if n == 0 || n > MaxFrame {
		return Frame{}, ErrFrameTooLarge
	}
	p := make([]byte, n-1)
	if _, err := io.ReadFull(c.r, p); err != nil {
		return Frame{}, err
	}
	return Frame{Kind: h[4], Payload: p}, nil
}

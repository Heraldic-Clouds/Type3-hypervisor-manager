// Package media produces encoded video access units and Opus packets for the
// agent. Two video backends exist:
//
//   - native: the mea-capture helper (NvFBC → NVENC, GPU-resident frames, live
//     bitrate/IDR control). This is the production path on the L4 host.
//   - ffmpeg: x11grab (or a test pattern) → h264_nvenc / libx264. Used when the
//     Capture SDK is not installed, and in CI. It costs a CPU copy per frame.
package media

import (
	"context"
	"time"
)

// Config describes the requested encode.
type Config struct {
	Codec   string // h264, h265, av1
	W, H    int
	FPS     int
	Kbps    int
	Display string // X display inside the container, e.g. ":0"
}

// AccessUnit is one encoded video frame.
type AccessUnit struct {
	Data []byte
	Key  bool
	PTS  time.Duration
	// Captured is when the frame left the capture stage (for latency stats).
	Captured time.Time
}

// Stats is the source's running telemetry.
type Stats struct {
	Encoder   string  `json:"encoder"`
	FPS       float64 `json:"fps"`
	Kbps      float64 `json:"kbps"`
	EncMs     float64 `json:"encMs"`
	CapMs     float64 `json:"capMs"`
	Idle      bool    `json:"idle"`
	Suspended bool    `json:"suspended"`
	Frames    uint64  `json:"frames"`
	Skipped   uint64  `json:"skipped"`
	Errors    uint64  `json:"errors"`
	Restarts  uint64  `json:"restarts"`
	W         int     `json:"w"`
	H         int     `json:"h"`
}

// VideoSource is implemented by the native helper and the ffmpeg backend.
type VideoSource interface {
	Name() string
	// Codecs lists encodable codecs on this machine.
	Codecs() []string
	// Start begins encoding; frames are delivered on Frames() until ctx ends
	// or Stop is called.
	Start(ctx context.Context, cfg Config) error
	Frames() <-chan AccessUnit
	SetBitrate(kbps int)
	SetFPS(fps int)
	ForceKeyframe()
	Suspend(bool)
	Stats() Stats
	Stop()
}

// AudioSource yields Opus packets (48 kHz, 10 ms).
type AudioSource interface {
	Start(ctx context.Context) error
	Packets() <-chan AccessUnit
	Stop()
}

// rateMeter tracks a 1 s sliding window of frames and bytes.
type rateMeter struct {
	start  time.Time
	frames int
	bytes  int
	fps    float64
	kbps   float64
}

func (r *rateMeter) add(n int, now time.Time) {
	if r.start.IsZero() {
		r.start = now
	}
	r.frames++
	r.bytes += n
	if d := now.Sub(r.start); d >= time.Second {
		r.fps = float64(r.frames) / d.Seconds()
		r.kbps = float64(r.bytes) * 8 / 1000 / d.Seconds()
		r.start, r.frames, r.bytes = now, 0, 0
	}
}

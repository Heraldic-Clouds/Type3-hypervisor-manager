package media

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

// Audio captures the container's own PulseAudio / pipewire-pulse monitor and
// encodes Opus (48 kHz stereo, 10 ms frames, low-delay mode). Each container
// runs its own audio server, so only that desktop's audio is captured.
type Audio struct {
	Bin         string // default "ffmpeg"
	Source      string // default "@DEFAULT_MONITOR@"
	Kbps        int    // default 96
	TestPattern bool
	Log         *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	out    chan AccessUnit
}

func (a *Audio) Args() []string {
	src, kbps := a.Source, a.Kbps
	if src == "" {
		src = "@DEFAULT_MONITOR@"
	}
	if kbps == 0 {
		kbps = 96
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-fflags", "nobuffer"}
	if a.TestPattern {
		args = append(args, "-re", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000")
	} else {
		args = append(args, "-f", "pulse", "-fragment_size", "1920", "-i", src)
	}
	return append(args, "-vn", "-ac", "2", "-ar", "48000", "-c:a", "libopus",
		"-application", "lowdelay", "-frame_duration", "10", "-b:a", fmt.Sprintf("%dk", kbps),
		"-f", "matroska", "-live", "1", "-cluster_size_limit", "1", "-cluster_time_limit", "0",
		"-flush_packets", "1", "pipe:1")
}

// Start launches capture. Failure to reach an audio server is reported to the
// caller, which continues video-only (PROTOCOL.md welcome.audio=false).
func (a *Audio) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	bin := a.Bin
	if bin == "" {
		bin = "ffmpeg"
	}
	if !a.TestPattern {
		if err := exec.CommandContext(ctx, "pactl", "info").Run(); err != nil {
			return ErrNoAudio
		}
	}
	pctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(pctx, bin, a.Args()...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	r, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return err
	}
	a.cancel = cancel
	a.out = make(chan AccessUnit, 32)
	out := a.out
	log := a.Log
	if log == nil {
		log = slog.Default()
	}
	go func() {
		defer close(out)
		mkv := NewMKVReader(r)
		for {
			p, err := mkv.Next()
			if err != nil {
				cmd.Wait()
				if pctx.Err() == nil {
					log.Warn("audio capture stopped", "err", err, "stderr", tail(stderr.String()))
				}
				return
			}
			if p.Video {
				continue
			}
			select {
			case out <- AccessUnit{Data: p.Data, PTS: p.PTS, Key: true, Captured: time.Now()}:
			default: // never block capture on a slow consumer
			}
		}
	}()
	return nil
}

func (a *Audio) Packets() <-chan AccessUnit { return a.out }

func (a *Audio) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
}

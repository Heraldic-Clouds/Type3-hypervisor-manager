package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// FFmpeg is the fallback video backend. Input is the container's own X
// display (x11grab) or, when TestPattern is set, a synthetic source.
type FFmpeg struct {
	Bin         string // default "ffmpeg"
	Encoder     string // "nvenc", "x264" or "" (auto: nvenc if it initialises)
	TestPattern bool
	Log         *slog.Logger

	mu        sync.Mutex
	cfg       Config
	ctx       context.Context
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	out       chan AccessUnit
	stats     Stats
	meter     rateMeter
	lastKey   time.Time
	lastStart time.Time
	suspended bool
	stopped   bool
	gen       int
	pending   *time.Timer
	codecs    []string
}

func (f *FFmpeg) bin() string {
	if f.Bin != "" {
		return f.Bin
	}
	return "ffmpeg"
}

func (f *FFmpeg) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.Default()
}

func (f *FFmpeg) Name() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "ffmpeg-" + f.encoder()
}

// probe runs a one-frame encode to find out whether an encoder really works
// (h264_nvenc is compiled into most ffmpeg builds even without a GPU).
func (f *FFmpeg) probe(enc string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.bin(), "-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "color=c=black:size=256x256:rate=30", "-frames:v", "2",
		"-c:v", enc, "-f", "null", "-")
	return cmd.Run() == nil
}

func (f *FFmpeg) encoder() string {
	if f.Encoder == "" {
		if f.probe("h264_nvenc") {
			f.Encoder = "nvenc"
		} else {
			f.Encoder = "x264"
		}
	}
	return f.Encoder
}

func (f *FFmpeg) Codecs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.codecs != nil {
		return f.codecs
	}
	if f.encoder() == "nvenc" {
		f.codecs = []string{"h264"}
		if f.probe("hevc_nvenc") {
			f.codecs = append(f.codecs, "h265")
		}
		if f.probe("av1_nvenc") {
			f.codecs = append(f.codecs, "av1")
		}
	} else {
		f.codecs = []string{"h264"}
	}
	return f.codecs
}

// Args builds the ffmpeg command line for cfg (exported for tests and docs).
func (f *FFmpeg) Args(cfg Config) []string {
	fps := strconv.Itoa(cfg.FPS)
	size := fmt.Sprintf("%dx%d", cfg.W, cfg.H)
	a := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-fflags", "nobuffer", "-flags", "low_delay"}
	if f.TestPattern {
		a = append(a, "-re", "-f", "lavfi", "-i", "testsrc2=size="+size+":rate="+fps)
	} else {
		a = append(a, "-f", "x11grab", "-draw_mouse", "1", "-framerate", fps, "-i", cfg.Display)
	}
	// Scale to the requested size and drop unchanged frames, keeping a
	// 5 fps keep-alive (PROTOCOL.md §5.1).
	maxDrop := max(1, cfg.FPS/5)
	vf := fmt.Sprintf("scale=%d:%d:flags=fast_bilinear,mpdecimate=max=%d,format=yuv420p", cfg.W, cfg.H, maxDrop)
	a = append(a, "-vf", vf, "-fps_mode", "vfr", "-an")
	k := strconv.Itoa(cfg.Kbps) + "k"
	vbv := strconv.Itoa(max(cfg.Kbps/max(cfg.FPS, 1), 50)) + "k" // ~one frame VBV
	gop := strconv.Itoa(cfg.FPS * 2)
	switch f.encoder() {
	case "nvenc":
		name := map[string]string{"h264": "h264_nvenc", "h265": "hevc_nvenc", "av1": "av1_nvenc"}[cfg.Codec]
		a = append(a, "-c:v", name, "-preset", "p1", "-tune", "ull", "-rc", "cbr",
			"-b:v", k, "-maxrate", k, "-bufsize", vbv, "-g", gop, "-bf", "0",
			"-delay", "0", "-zerolatency", "1", "-forced-idr", "1")
		if cfg.Codec == "h264" {
			a = append(a, "-profile:v", "baseline")
		}
	default:
		a = append(a, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-profile:v", "baseline", "-b:v", k, "-maxrate", k, "-bufsize", vbv,
			"-g", gop, "-bf", "0", "-threads", "2")
	}
	return append(a, "-f", "matroska", "-live", "1", "-cluster_size_limit", "1",
		"-cluster_time_limit", "0", "-flush_packets", "1", "pipe:1")
}

func (f *FFmpeg) Start(ctx context.Context, cfg Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cfg.Codec == "" {
		cfg.Codec = "h264"
	}
	f.cfg, f.ctx, f.stopped = cfg, ctx, false
	f.out = make(chan AccessUnit, 8)
	f.stats = Stats{Encoder: "ffmpeg-" + f.encoder(), W: cfg.W, H: cfg.H}
	return f.spawn()
}

// spawn starts ffmpeg for f.cfg; f.mu must be held.
func (f *FFmpeg) spawn() error {
	if f.cancel != nil {
		f.cancel()
	}
	if f.suspended || f.stopped {
		return nil
	}
	f.gen++
	gen := f.gen
	pctx, cancel := context.WithCancel(f.ctx)
	cmd := exec.CommandContext(pctx, f.bin(), f.Args(f.cfg)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("ffmpeg: %w", err)
	}
	f.cmd, f.cancel, f.lastStart = cmd, cancel, time.Now()
	f.stats.Restarts++
	go f.pump(gen, stdout, cmd, &stderr)
	return nil
}

func (f *FFmpeg) pump(gen int, r io.Reader, cmd *exec.Cmd, stderr *bytes.Buffer) {
	mkv := NewMKVReader(r)
	for {
		p, err := mkv.Next()
		if err != nil {
			cmd.Wait()
			f.mu.Lock()
			current := gen == f.gen && !f.stopped && !f.suspended
			if current {
				f.stats.Errors++
				f.log().Warn("ffmpeg exited", "err", err, "stderr", tail(stderr.String()))
				// restart after a crash, not after an intentional stop
				time.AfterFunc(time.Second, func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					if gen == f.gen && !f.stopped && !f.suspended {
						f.spawn()
					}
				})
			}
			f.mu.Unlock()
			return
		}
		if !p.Video {
			continue
		}
		now := time.Now()
		f.mu.Lock()
		if gen != f.gen {
			f.mu.Unlock()
			return
		}
		f.meter.add(len(p.Data), now)
		f.stats.Frames++
		f.stats.FPS, f.stats.Kbps = f.meter.fps, f.meter.kbps
		f.stats.Idle = f.meter.fps > 0 && f.meter.fps < 6 && f.cfg.FPS > 10
		if p.Key {
			f.lastKey = now
		}
		out := f.out
		f.mu.Unlock()
		select {
		case out <- AccessUnit{Data: p.Data, Key: p.Key, PTS: p.PTS, Captured: now}:
		default:
			f.mu.Lock()
			f.stats.Skipped++ // consumer is behind: drop rather than add latency
			f.mu.Unlock()
		}
	}
}

func tail(s string) string {
	if len(s) > 400 {
		return s[len(s)-400:]
	}
	return s
}

func (f *FFmpeg) Frames() <-chan AccessUnit { return f.out }

// restartSoon coalesces reconfiguration: ffmpeg cannot change bitrate live,
// so changes are applied by restarting at most every 3 s.
func (f *FFmpeg) restartSoon() {
	if f.pending != nil {
		return
	}
	wait := time.Until(f.lastStart.Add(3 * time.Second))
	if wait < 0 {
		wait = 0
	}
	f.pending = time.AfterFunc(wait, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.pending = nil
		f.spawn()
	})
}

func (f *FFmpeg) SetBitrate(kbps int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	old := f.cfg.Kbps
	if kbps <= 0 || (kbps*4 > old*3 && kbps*4 < old*5) {
		return // ignore changes under 25 %
	}
	f.cfg.Kbps = kbps
	f.restartSoon()
}

func (f *FFmpeg) SetFPS(fps int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fps <= 0 || fps == f.cfg.FPS {
		return
	}
	f.cfg.FPS = fps
	f.restartSoon()
}

func (f *FFmpeg) ForceKeyframe() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if time.Since(f.lastKey) < time.Second {
		return
	}
	f.restartSoon() // a fresh encoder starts with an IDR
}

func (f *FFmpeg) Suspend(s bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s == f.suspended {
		return
	}
	f.suspended = s
	f.stats.Suspended = s
	if s {
		if f.cancel != nil {
			f.cancel()
		}
		f.gen++
		return
	}
	f.spawn()
}

func (f *FFmpeg) Stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}

func (f *FFmpeg) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	f.gen++
	if f.pending != nil {
		f.pending.Stop()
		f.pending = nil
	}
	if f.cancel != nil {
		f.cancel()
	}
}

// ErrNoAudio means no PulseAudio/PipeWire server is reachable.
var ErrNoAudio = errors.New("media: no audio server")

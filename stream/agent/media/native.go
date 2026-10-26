package media

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Native drives the mea-capture helper (stream/capture), which captures with
// NVIDIA Capture SDK (NvFBC) into CUDA memory and encodes with NVENC without a
// CPU copy of the framebuffer.
//
// Helper protocol:
//
//	stdout: frames  len:u32be | type:u8 | flags:u8 | pts_us:u64be | capture_us:u32be | encode_us:u32be | data
//	        type 1 = video access unit (flags bit0 keyframe, bit1 unchanged-skip),
//	        type 2 = JSON status line (e.g. {"codecs":["h264","h265","av1"]}).
//	stdin:  JSON lines {"t":"bitrate","kbps":N} {"t":"fps","fps":N}
//	        {"t":"keyframe"} {"t":"suspend"} {"t":"resume"} {"t":"quit"}
type Native struct {
	Bin string // default "mea-capture"
	Log *slog.Logger

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	cancel  context.CancelFunc
	out     chan AccessUnit
	stats   Stats
	meter   rateMeter
	codecs  []string
	lastChg time.Time
}

func (n *Native) bin() string {
	if n.Bin != "" {
		return n.Bin
	}
	return "mea-capture"
}

func (n *Native) Name() string { return "nvfbc-nvenc" }

// Codecs asks the helper which codecs NVENC on this GPU supports.
func (n *Native) Codecs() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.codecs != nil {
		return n.codecs
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, n.bin(), "--probe").Output()
	if err != nil {
		return nil
	}
	var r struct {
		Codecs []string `json:"codecs"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &r) != nil {
		return nil
	}
	n.codecs = r.Codecs
	return n.codecs
}

// Available reports whether the helper is installed and can open NvFBC.
func (n *Native) Available() bool { return len(n.Codecs()) > 0 }

func (n *Native) Start(ctx context.Context, cfg Config) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if cfg.Codec == "" {
		cfg.Codec = "h264"
	}
	pctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(pctx, n.bin(),
		"--codec", cfg.Codec, "--width", strconv.Itoa(cfg.W), "--height", strconv.Itoa(cfg.H),
		"--fps", strconv.Itoa(cfg.FPS), "--kbps", strconv.Itoa(cfg.Kbps), "--display", cfg.Display)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("mea-capture: %w", err)
	}
	n.cmd, n.stdin, n.cancel = cmd, stdin, cancel
	n.out = make(chan AccessUnit, 8)
	n.stats = Stats{Encoder: n.Name(), W: cfg.W, H: cfg.H}
	go n.pump(stdout, cmd, &stderr, n.out)
	return nil
}

func (n *Native) pump(r io.Reader, cmd *exec.Cmd, stderr *bytes.Buffer, out chan AccessUnit) {
	br := bufio.NewReaderSize(r, 1<<20)
	log := n.Log
	if log == nil {
		log = slog.Default()
	}
	defer close(out)
	var hdr [22]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			cmd.Wait()
			n.mu.Lock()
			n.stats.Errors++
			n.mu.Unlock()
			log.Warn("mea-capture exited", "err", err, "stderr", tail(stderr.String()))
			return
		}
		l := binary.BigEndian.Uint32(hdr[0:])
		if l < 18 || l > 32<<20 {
			log.Error("mea-capture: bad frame length", "len", l)
			cmd.Process.Kill()
			continue
		}
		data := make([]byte, l-18)
		if _, err := io.ReadFull(br, data); err != nil {
			continue
		}
		typ, flags := hdr[4], hdr[5]
		pts := time.Duration(binary.BigEndian.Uint64(hdr[6:])) * time.Microsecond
		capUS, encUS := binary.BigEndian.Uint32(hdr[14:]), binary.BigEndian.Uint32(hdr[18:])
		if typ == 2 {
			log.Info("mea-capture", "status", strings.TrimSpace(string(data)))
			continue
		}
		now := time.Now()
		n.mu.Lock()
		if flags&2 != 0 {
			n.stats.Skipped++
			n.mu.Unlock()
			continue
		}
		n.meter.add(len(data), now)
		n.stats.Frames++
		n.stats.FPS, n.stats.Kbps = n.meter.fps, n.meter.kbps
		n.stats.CapMs, n.stats.EncMs = float64(capUS)/1000, float64(encUS)/1000
		n.stats.Idle = n.meter.fps > 0 && n.meter.fps < 6
		n.mu.Unlock()
		select {
		case out <- AccessUnit{Data: data, Key: flags&1 != 0, PTS: pts, Captured: now}:
		default:
			n.mu.Lock()
			n.stats.Skipped++
			n.mu.Unlock()
		}
	}
}

func (n *Native) send(v map[string]any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stdin == nil {
		return
	}
	b, _ := json.Marshal(v)
	n.stdin.Write(append(b, '\n'))
}

func (n *Native) Frames() <-chan AccessUnit { return n.out }
func (n *Native) SetBitrate(kbps int)       { n.send(map[string]any{"t": "bitrate", "kbps": kbps}) }
func (n *Native) SetFPS(fps int)            { n.send(map[string]any{"t": "fps", "fps": fps}) }
func (n *Native) ForceKeyframe()            { n.send(map[string]any{"t": "keyframe"}) }
func (n *Native) Suspend(s bool) {
	t := "resume"
	if s {
		t = "suspend"
	}
	n.mu.Lock()
	n.stats.Suspended = s
	n.mu.Unlock()
	n.send(map[string]any{"t": t})
}

func (n *Native) Stats() Stats {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stats
}

func (n *Native) Stop() {
	n.send(map[string]any{"t": "quit"})
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cancel != nil {
		c := n.cancel
		time.AfterFunc(500*time.Millisecond, c)
	}
	n.stdin = nil
}

var ErrNoNative = errors.New("media: mea-capture unavailable")

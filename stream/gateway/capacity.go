package gateway

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GPUSample is one nvidia-smi reading of the host GPU.
type GPUSample struct {
	At          time.Time `json:"at"`
	Name        string    `json:"name"`
	Driver      string    `json:"driver"`
	GPUUtil     float64   `json:"gpuUtil"`     // %
	EncUtil     float64   `json:"encoderUtil"` // %
	MemUsedMiB  float64   `json:"memUsedMiB"`
	MemTotalMiB float64   `json:"memTotalMiB"`
	EncSessions int       `json:"encoderSessions"`
	EncFPS      float64   `json:"encoderFps"`
	EncLatency  float64   `json:"encoderLatencyUs"`
	TempC       float64   `json:"tempC"`
	PowerW      float64   `json:"powerW"`
}

// Sampler polls nvidia-smi. Without a GPU it returns no samples and admission
// falls back to the static session limit.
type Sampler struct {
	Bin      string // default nvidia-smi
	Interval time.Duration

	mu   sync.Mutex
	last *GPUSample
	err  error
}

const smiQuery = "name,driver_version,utilization.gpu,utilization.encoder,memory.used,memory.total," +
	"encoder.stats.sessionCount,encoder.stats.averageFps,encoder.stats.averageLatency,temperature.gpu,power.draw"

// ParseSMI parses one CSV line of smiQuery output.
func ParseSMI(line string) (GPUSample, bool) {
	f := strings.Split(line, ",")
	if len(f) < 11 {
		return GPUSample{}, false
	}
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	num := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	return GPUSample{
		Name: f[0], Driver: f[1], GPUUtil: num(f[2]), EncUtil: num(f[3]),
		MemUsedMiB: num(f[4]), MemTotalMiB: num(f[5]), EncSessions: int(num(f[6])),
		EncFPS: num(f[7]), EncLatency: num(f[8]), TempC: num(f[9]), PowerW: num(f[10]),
	}, true
}

func (s *Sampler) Run(ctx context.Context) {
	bin := s.Bin
	if bin == "" {
		bin = "nvidia-smi"
	}
	iv := s.Interval
	if iv == 0 {
		iv = 2 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		out, err := exec.CommandContext(cctx, bin, "--query-gpu="+smiQuery, "--format=csv,noheader,nounits").Output()
		cancel()
		s.mu.Lock()
		s.err = err
		if err == nil {
			sc := bufio.NewScanner(bytes.NewReader(out))
			if sc.Scan() {
				if g, ok := ParseSMI(sc.Text()); ok {
					g.At = time.Now()
					s.last = &g
				}
			}
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Last returns the latest sample younger than 10 s.
func (s *Sampler) Last() *GPUSample {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil || time.Since(s.last.At) > 10*time.Second {
		return nil
	}
	c := *s.last
	return &c
}

// Limits decide admission. MaxSessions is the measured safe density for the
// host (Type3 updates it after benchmarks; it starts at 1 until the L4
// benchmark passes, see docs/BENCHMARK.md).
type Limits struct {
	MaxSessions   int     `json:"maxSessions"`
	MaxEncoderPct float64 `json:"maxEncoderPct"` // refuse new sessions above this NVENC load
	MaxGPUPct     float64 `json:"maxGpuPct"`
	MinFreeMemMiB float64 `json:"minFreeMemMiB"`
	Draining      bool    `json:"draining"`
}

// Admit reports whether one more session fits.
func Admit(l Limits, active int, g *GPUSample) (bool, string) {
	switch {
	case l.Draining:
		return false, "host draining"
	case l.MaxSessions > 0 && active >= l.MaxSessions:
		return false, "session limit reached"
	case g == nil:
		return true, ""
	case l.MaxEncoderPct > 0 && g.EncUtil >= l.MaxEncoderPct:
		return false, "encoder saturated"
	case l.MaxGPUPct > 0 && g.GPUUtil >= l.MaxGPUPct:
		return false, "gpu saturated"
	case l.MinFreeMemMiB > 0 && g.MemTotalMiB-g.MemUsedMiB < l.MinFreeMemMiB:
		return false, "gpu memory low"
	}
	return true, ""
}

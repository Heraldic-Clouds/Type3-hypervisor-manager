// mea-stream-bench measures how many concurrent Gaming Mode streams one GPU
// host sustains (Phase H). It opens 1..N real WebRTC sessions against the
// gateway, each to a different Eleet desktop, and records per step:
// received fps and bitrate, ping RTT, agent encode/capture time, and the
// host's GPU, NVENC and memory use from the gateway admin API.
//
// Every desktop must show a moving workload (a game, vkcube, a video) while
// it runs: an unchanged desktop is encoded at the 5 fps keep-alive only.
//
//	MEA_STREAM_PRIVATE_KEY=... MEA_ADMIN_TOKEN=... mea-stream-bench \
//	  -gateway https://stream.miseenaby.me -host gpu-1 \
//	  -desktops d1,d2,d3,d4 -steps 1,2,3,4 -duration 60s -out bench.json
//
// It reports measurements only; it never decides capacity on its own.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/gateway"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/internal/env"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/protocol"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/token"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

type opts struct {
	gateway, admin, adminToken, host, profile, codec, origin string
	desktops                                                 []string
	duration, warmup                                         time.Duration
}

// SessionResult is one stream's measurements over a step.
type SessionResult struct {
	Desktop   string  `json:"desktop"`
	Codec     string  `json:"codec"`
	W         int     `json:"w"`
	H         int     `json:"h"`
	TargetFPS int     `json:"targetFps"`
	FPS       float64 `json:"fps"`
	Kbps      float64 `json:"kbps"`
	RTTp50    float64 `json:"rttP50Ms"`
	RTTp95    float64 `json:"rttP95Ms"`
	EncMsP50  float64 `json:"encMsP50"`
	EncMsP95  float64 `json:"encMsP95"`
	CapMsP95  float64 `json:"capMsP95"`
	Error     string  `json:"error,omitempty"`
}

// StepResult is one concurrency level.
type StepResult struct {
	Sessions   int             `json:"sessions"`
	Streams    []SessionResult `json:"streams"`
	GPUUtilMax float64         `json:"gpuUtilMax"`
	EncUtilMax float64         `json:"encoderUtilMax"`
	EncUtilAvg float64         `json:"encoderUtilAvg"`
	MemUsedMax float64         `json:"memUsedMiBMax"`
	MemTotal   float64         `json:"memTotalMiB"`
	PowerMax   float64         `json:"powerWMax"`
	GPU        string          `json:"gpu"`
	Driver     string          `json:"driver"`
	Samples    int             `json:"gpuSamples"`
}

func main() {
	var o opts
	var desktops, steps string
	flag.StringVar(&o.gateway, "gateway", "", "gateway signalling URL")
	flag.StringVar(&o.admin, "admin", "http://127.0.0.1:47990", "gateway admin URL (GPU samples); empty to skip")
	flag.StringVar(&o.host, "host", "", "Type3 host id the gateway serves")
	flag.StringVar(&o.profile, "profile", "balanced", "economy | balanced | high")
	flag.StringVar(&o.codec, "codec", "h264", "h264 | h265 | av1")
	flag.StringVar(&o.origin, "origin", "https://miseenaby.me", "Origin header (must be allowed by the gateway)")
	flag.StringVar(&desktops, "desktops", "", "comma-separated desktop ids, one per concurrent stream")
	flag.StringVar(&steps, "steps", "1,2,3,4", "concurrency levels to measure")
	flag.DurationVar(&o.duration, "duration", 60*time.Second, "measurement time per step")
	flag.DurationVar(&o.warmup, "warmup", 10*time.Second, "time before measuring each step")
	out := flag.String("out", "", "write JSON results here")
	flag.Parse()
	o.adminToken = env.Secret("MEA_ADMIN_TOKEN")
	for _, d := range strings.Split(desktops, ",") {
		if d = strings.TrimSpace(d); d != "" {
			o.desktops = append(o.desktops, d)
		}
	}
	priv, err := token.ParsePrivateKey(env.Secret("MEA_STREAM_PRIVATE_KEY"))
	if err != nil || o.gateway == "" || o.host == "" || len(o.desktops) == 0 {
		fmt.Fprintln(os.Stderr, "need -gateway, -host, -desktops and MEA_STREAM_PRIVATE_KEY")
		flag.Usage()
		os.Exit(2)
	}
	var results []StepResult
	for _, s := range strings.Split(steps, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			continue
		}
		if n > len(o.desktops) {
			fmt.Fprintf(os.Stderr, "skipping %d streams: only %d desktops given\n", n, len(o.desktops))
			continue
		}
		fmt.Fprintf(os.Stderr, "measuring %d concurrent stream(s) for %s…\n", n, o.duration)
		r := step(o, priv, n)
		results = append(results, r)
		printStep(r)
		time.Sleep(5 * time.Second) // let sessions close and the GPU settle
	}
	if *out != "" {
		b, _ := json.MarshalIndent(map[string]any{
			"at": time.Now().UTC().Format(time.RFC3339), "gateway": o.gateway, "host": o.host,
			"profile": o.profile, "codec": o.codec, "duration": o.duration.String(), "steps": results,
		}, "", "  ")
		os.WriteFile(*out, b, 0o644)
	}
}

func step(o opts, priv []byte, n int) StepResult {
	ctx, cancel := context.WithTimeout(context.Background(), o.warmup+o.duration+30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	res := make([]SessionResult, n)
	measure := make(chan struct{})
	stop := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i] = runStream(ctx, o, priv, o.desktops[i], measure, stop)
		}(i)
	}
	time.Sleep(o.warmup)
	close(measure)
	samples := sampleGPU(o, o.duration)
	close(stop)
	wg.Wait()
	r := StepResult{Sessions: n, Streams: res, Samples: len(samples)}
	var encSum float64
	for _, g := range samples {
		r.GPUUtilMax = max(r.GPUUtilMax, g.GPUUtil)
		r.EncUtilMax = max(r.EncUtilMax, g.EncUtil)
		r.MemUsedMax = max(r.MemUsedMax, g.MemUsedMiB)
		r.PowerMax = max(r.PowerMax, g.PowerW)
		r.MemTotal, r.GPU, r.Driver = g.MemTotalMiB, g.Name, g.Driver
		encSum += g.EncUtil
	}
	if len(samples) > 0 {
		r.EncUtilAvg = encSum / float64(len(samples))
	}
	return r
}

func sampleGPU(o opts, d time.Duration) []gateway.GPUSample {
	var out []gateway.GPUSample
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if o.admin != "" {
			req, _ := http.NewRequest("GET", strings.TrimRight(o.admin, "/")+"/v1/metrics", nil)
			req.Header.Set("Authorization", "Bearer "+o.adminToken)
			if res, err := http.DefaultClient.Do(req); err == nil {
				var m gateway.HostMetrics
				if json.NewDecoder(res.Body).Decode(&m) == nil && m.GPU != nil {
					out = append(out, *m.GPU)
				}
				res.Body.Close()
			}
		}
		time.Sleep(time.Second)
	}
	return out
}

func runStream(ctx context.Context, o opts, priv []byte, desktop string, measure, stop <-chan struct{}) SessionResult {
	r := SessionResult{Desktop: desktop}
	now := time.Now()
	tok, _, err := token.Mint(priv, token.Claims{
		User: "bench", Desktop: desktop, Host: o.host, Session: token.NewID(),
		Entitlement: "admin", Profile: o.profile,
	}, now)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		r.Error = err.Error()
		return r
	}
	var se webrtc.SettingEngine
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer pc.Close()
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	control, _ := pc.CreateDataChannel(protocol.LabelControl, nil)
	pc.CreateDataChannel(protocol.LabelKey, nil)
	ordered, zero := false, uint16(0)
	pc.CreateDataChannel(protocol.LabelRT, &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &zero})

	var measuring atomic.Bool
	var frames, bytes atomic.Int64
	var mu sync.Mutex
	var rtts, enc, capt []float64
	t0 := time.Now()
	control.OnMessage(func(msg webrtc.DataChannelMessage) {
		var c protocol.Control
		if json.Unmarshal(msg.Data, &c) != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch c.T {
		case "welcome":
			r.Codec, r.W, r.H, r.TargetFPS = c.Codec, c.W, c.H, c.FPS
		case "configured":
			r.TargetFPS = c.FPS
		case "pong":
			if measuring.Load() {
				rtts = append(rtts, float64(time.Since(t0).Microseconds())/1000-c.TS)
			}
		case "sstats":
			if measuring.Load() && !c.Idle {
				enc = append(enc, c.EncMs)
				capt = append(capt, c.CapMs)
			}
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				p, _, err := tr.ReadRTP()
				if err != nil {
					return
				}
				if tr.Kind() == webrtc.RTPCodecTypeVideo && measuring.Load() {
					bytes.Add(int64(len(p.Payload)))
					if p.Marker {
						frames.Add(1)
					}
				}
			}
		}()
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	pc.SetLocalDescription(offer)
	<-gathered
	hello := protocol.Control{T: "hello", V: json.RawMessage(`["mea-stream/1"]`), Codecs: []string{o.codec}, Profile: o.profile}
	body, _ := json.Marshal(gateway.CreateRequest{Token: tok, SDP: pc.LocalDescription().SDP, Hello: &hello})
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(o.gateway, "/")+"/v1/sessions", bytesReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", o.origin)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		r.Error = fmt.Sprintf("HTTP %d: %s", res.StatusCode, b)
		return r
	}
	var cr gateway.CreateResponse
	if err := json.Unmarshal(b, &cr); err != nil {
		r.Error = err.Error()
		return r
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: cr.SDP}); err != nil {
		r.Error = err.Error()
		return r
	}
	defer func() {
		req, _ := http.NewRequest("DELETE", strings.TrimRight(o.gateway, "/")+"/v1/sessions/"+cr.SID, nil)
		req.Header.Set("Authorization", "Bearer "+cr.Secret)
		if res, err := http.DefaultClient.Do(req); err == nil {
			res.Body.Close()
		}
	}()

	ping := time.NewTicker(500 * time.Millisecond)
	defer ping.Stop()
	var start time.Time
	n := uint64(0)
	for {
		select {
		case <-ctx.Done():
			r.Error = "timed out"
			return r
		case <-measure:
			measure = nil
			start = time.Now()
			measuring.Store(true)
		case <-stop:
			el := time.Since(start).Seconds()
			measuring.Store(false)
			mu.Lock()
			defer mu.Unlock()
			if el > 0 {
				r.FPS = float64(frames.Load()) / el
				r.Kbps = float64(bytes.Load()) * 8 / 1000 / el
			}
			r.RTTp50, r.RTTp95 = pct(rtts, 50), pct(rtts, 95)
			r.EncMsP50, r.EncMsP95 = pct(enc, 50), pct(enc, 95)
			r.CapMsP95 = pct(capt, 95)
			return r
		case <-ping.C:
			n++
			msg, _ := json.Marshal(protocol.Control{T: "ping", N: n, TS: float64(time.Since(t0).Microseconds()) / 1000})
			control.SendText(string(msg))
		}
	}
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	sort.Float64s(s)
	i := int(float64(len(s)-1) * p / 100)
	return s[i]
}

func printStep(r StepResult) {
	fmt.Printf("\n%d stream(s)  GPU %s  driver %s  GPU max %.0f%%  NVENC avg %.0f%% max %.0f%%  mem %.0f/%.0f MiB  power %.0f W\n",
		r.Sessions, r.GPU, r.Driver, r.GPUUtilMax, r.EncUtilAvg, r.EncUtilMax, r.MemUsedMax, r.MemTotal, r.PowerMax)
	fmt.Println("| desktop | codec | size | fps (target) | Mbps | RTT p50/p95 ms | encode p50/p95 ms | capture p95 ms | error |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|")
	for _, s := range r.Streams {
		fmt.Printf("| %s | %s | %dx%d | %.1f (%d) | %.1f | %.1f / %.1f | %.2f / %.2f | %.2f | %s |\n",
			s.Desktop, s.Codec, s.W, s.H, s.FPS, s.TargetFPS, s.Kbps/1000, s.RTTp50, s.RTTp95, s.EncMsP50, s.EncMsP95, s.CapMsP95, s.Error)
	}
}

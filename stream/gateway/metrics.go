package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// SessionMetrics is the per-stream view Type3 reads.
type SessionMetrics struct {
	SID          string  `json:"sid"`
	User         string  `json:"user"`
	Desktop      string  `json:"desktop"`
	State        string  `json:"state"`
	Codec        string  `json:"codec"`
	Encoder      string  `json:"encoder"`
	W            int     `json:"w"`
	H            int     `json:"h"`
	Profile      string  `json:"profile"`
	TargetFPS    int     `json:"targetFps"`
	FPS          float64 `json:"fps"`
	Kbps         float64 `json:"kbps"`
	TargetKbps   int     `json:"targetKbps"`
	EstimateKbps int     `json:"estimateKbps"`
	EncMs        float64 `json:"encMs"`
	CapMs        float64 `json:"capMs"`
	Idle         bool    `json:"idle"`
	Suspended    bool    `json:"suspended"`
	ClientFPS    int     `json:"clientFps"`
	ClientRTTMs  float64 `json:"clientRttMs"`
	ClientLoss   float64 `json:"clientLossPct"`
	DecodeMs     float64 `json:"clientDecodeMs"`
	VideoBytes   uint64  `json:"videoBytes"`
	AudioBytes   uint64  `json:"audioBytes"`
	InputEvents  uint64  `json:"inputEvents"`
	DroppedInput uint64  `json:"droppedInput"`
	StartedAt    string  `json:"startedAt"`
	Seconds      int     `json:"seconds"`
}

type HostMetrics struct {
	Host        string           `json:"host"`
	At          string           `json:"at"`
	Active      int              `json:"activeSessions"`
	Streaming   int              `json:"streamingSessions"`
	Idle        int              `json:"idleSessions"`
	Limits      Limits           `json:"limits"`
	CanAdmit    bool             `json:"canAdmit"`
	AdmitReason string           `json:"admitReason,omitempty"`
	GPU         *GPUSample       `json:"gpu"`
	Admitted    uint64           `json:"admittedTotal"`
	Rejected    uint64           `json:"rejectedTotal"`
	Ended       uint64           `json:"endedTotal"`
	EgressBytes uint64           `json:"egressBytesTotal"`
	Sessions    []SessionMetrics `json:"sessions"`
}

func (g *Gateway) Metrics() HostMetrics {
	g.mu.Lock()
	ss := make([]*Session, 0, len(g.sessions))
	for _, s := range g.sessions {
		ss = append(ss, s)
	}
	limits := g.limits
	g.mu.Unlock()
	gpu := g.cfg.Sampler.Last()
	ok, why := Admit(limits, len(ss), gpu)
	hm := HostMetrics{
		Host: g.cfg.HostID, At: time.Now().UTC().Format(time.RFC3339), Active: len(ss),
		Limits: limits, CanAdmit: ok, AdmitReason: why, GPU: gpu,
		Admitted: g.admitted.Load(), Rejected: g.rejected.Load(), Ended: g.ended.Load(),
		EgressBytes: g.egressBytes.Load(), Sessions: []SessionMetrics{},
	}
	for _, s := range ss {
		est := 0
		if e := s.bwe.Load(); e != nil {
			est = (*e).GetTargetBitrate() / 1000
		}
		s.mu.Lock()
		m := SessionMetrics{
			SID: s.sid, User: s.claims.User, Desktop: s.claims.Desktop, State: s.state,
			Codec: s.ready.Codec, Encoder: s.agentStats.Encoder, W: s.agentStats.W, H: s.agentStats.H,
			Profile: s.profile.Name, TargetFPS: s.fps, FPS: s.agentStats.FPS, Kbps: s.agentStats.Kbps,
			TargetKbps: s.targetKbps, EstimateKbps: est, EncMs: s.agentStats.EncMs, CapMs: s.agentStats.CapMs,
			Idle: s.agentStats.Idle, Suspended: s.suspended, ClientFPS: s.client.FPS,
			ClientRTTMs: s.client.RTTMs, ClientLoss: s.client.LossPct, DecodeMs: s.client.DecodeMs,
			VideoBytes: s.videoBytes.Load(), AudioBytes: s.audioBytes.Load(),
			InputEvents: s.inputEvents.Load(), DroppedInput: s.droppedInput.Load(),
			StartedAt: s.started.UTC().Format(time.RFC3339), Seconds: int(time.Since(s.started).Seconds()),
		}
		s.mu.Unlock()
		if m.State == "streaming" {
			hm.Streaming++
		}
		if m.Idle || m.Suspended {
			hm.Idle++
		}
		hm.Sessions = append(hm.Sessions, m)
	}
	sort.Slice(hm.Sessions, func(i, j int) bool { return hm.Sessions[i].StartedAt < hm.Sessions[j].StartedAt })
	return hm
}

func (g *Gateway) metricsJSON(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, g.Metrics())
}

// metricsProm serves Prometheus text format for CloudWatch agent / scraping.
func (g *Gateway) metricsProm(w http.ResponseWriter, _ *http.Request) {
	m := g.Metrics()
	var b strings.Builder
	p := func(name string, v any, labels ...string) {
		l := ""
		if len(labels) > 0 {
			l = "{" + strings.Join(labels, ",") + "}"
		}
		fmt.Fprintf(&b, "mea_stream_%s%s %v\n", name, l, v)
	}
	host := fmt.Sprintf("host=%q", m.Host)
	p("sessions_active", m.Active, host)
	p("sessions_streaming", m.Streaming, host)
	p("sessions_idle", m.Idle, host)
	p("sessions_limit", m.Limits.MaxSessions, host)
	p("admitted_total", m.Admitted, host)
	p("rejected_total", m.Rejected, host)
	p("egress_bytes_total", m.EgressBytes, host)
	if m.GPU != nil {
		p("gpu_util_percent", m.GPU.GPUUtil, host)
		p("nvenc_util_percent", m.GPU.EncUtil, host)
		p("gpu_mem_used_mib", m.GPU.MemUsedMiB, host)
		p("nvenc_sessions", m.GPU.EncSessions, host)
	}
	for _, s := range m.Sessions {
		l := []string{host, fmt.Sprintf("sid=%q", s.SID), fmt.Sprintf("desktop=%q", s.Desktop)}
		p("session_fps", s.FPS, l...)
		p("session_kbps", s.Kbps, l...)
		p("session_target_kbps", s.TargetKbps, l...)
		p("session_rtt_ms", s.ClientRTTMs, l...)
		p("session_encode_ms", s.EncMs, l...)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Write([]byte(b.String()))
}

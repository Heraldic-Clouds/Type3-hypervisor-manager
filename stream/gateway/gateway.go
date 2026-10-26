// Package gateway is the MEA Stream gateway that runs once per GPU host. It
// verifies stream tokens, enforces host capacity, terminates WebRTC for every
// session on one muxed UDP port, and relays media and input between browsers
// and the agent inside exactly one LXD container per session.
package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/protocol"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/token"
)

type Config struct {
	HostID    string
	PublicKey ed25519.PublicKey

	// MediaPort is the single UDP (and ICE-TCP) port shared by all sessions.
	MediaPort int
	// PublicIPs are advertised as host candidates (the EC2 public/Elastic IP;
	// the instance only sees its private address).
	PublicIPs []string
	// ICETCP enables passive ICE-TCP on MediaPort for UDP-blocked networks.
	ICETCP bool
	// Loopback allows 127.0.0.1 candidates (tests and local development).
	Loopback bool

	// AllowedOrigins are the MEA web origins allowed to call the signalling API.
	AllowedOrigins []string
	// AdminToken protects the Type3 metrics/admin API.
	AdminToken string

	Registry Registry
	Limits   Limits
	Sampler  *Sampler

	// AgentDialTimeout and ReadyTimeout bound the agent link setup.
	AgentDialTimeout time.Duration
	ReadyTimeout     time.Duration

	Log *slog.Logger
	Now func() time.Time
}

type Gateway struct {
	cfg      Config
	log      *slog.Logger
	verifier *token.Verifier
	udpMux   ice.UDPMux
	tcpMux   ice.TCPMux
	closers  []io.Closer

	mu        sync.Mutex
	limits    Limits
	sessions  map[string]*Session // by sid
	byDesktop map[string]*Session

	admitted, rejected, ended atomic.Uint64
	egressBytes               atomic.Uint64
}

func New(cfg Config) (*Gateway, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.AgentDialTimeout == 0 {
		cfg.AgentDialTimeout = 3 * time.Second
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 10 * time.Second
	}
	if cfg.HostID == "" || cfg.PublicKey == nil || cfg.Registry == nil {
		return nil, errors.New("gateway: HostID, PublicKey and Registry are required")
	}
	g := &Gateway{
		cfg: cfg, log: cfg.Log, limits: cfg.Limits,
		verifier:  &token.Verifier{Public: cfg.PublicKey, Host: cfg.HostID, Now: cfg.Now},
		sessions:  map[string]*Session{},
		byDesktop: map[string]*Session{},
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{Port: cfg.MediaPort})
	if err != nil {
		return nil, fmt.Errorf("gateway: media port: %w", err)
	}
	g.closers = append(g.closers, udp)
	g.udpMux = webrtc.NewICEUDPMux(nil, udp)
	if cfg.ICETCP {
		tcp, err := net.ListenTCP("tcp4", &net.TCPAddr{Port: udp.LocalAddr().(*net.UDPAddr).Port})
		if err != nil {
			udp.Close()
			return nil, fmt.Errorf("gateway: ICE-TCP port: %w", err)
		}
		g.closers = append(g.closers, tcp)
		g.tcpMux = webrtc.NewICETCPMux(nil, tcp, 8)
	}
	return g, nil
}

// MediaAddr returns the bound media UDP address.
func (g *Gateway) MediaAddr() net.Addr { return g.closers[0].(*net.UDPConn).LocalAddr() }

func (g *Gateway) Close() {
	g.mu.Lock()
	ss := make([]*Session, 0, len(g.sessions))
	for _, s := range g.sessions {
		ss = append(ss, s)
	}
	g.mu.Unlock()
	for _, s := range ss {
		s.Close(protocol.CodeNormal, "gateway shutdown")
	}
	for _, c := range g.closers {
		c.Close()
	}
}

func (g *Gateway) settingEngine() webrtc.SettingEngine {
	var se webrtc.SettingEngine
	se.SetICEUDPMux(g.udpMux)
	types := []webrtc.NetworkType{webrtc.NetworkTypeUDP4}
	if g.tcpMux != nil {
		se.SetICETCPMux(g.tcpMux)
		types = append(types, webrtc.NetworkTypeTCP4)
	}
	se.SetNetworkTypes(types)
	if len(g.cfg.PublicIPs) > 0 {
		se.SetNAT1To1IPs(g.cfg.PublicIPs, webrtc.ICECandidateTypeHost)
		// The gateway has a public address, so it does not need to gather or
		// check candidates itself; browsers (and their TURN relays) do.
		se.SetLite(true)
	}
	se.SetIncludeLoopbackCandidate(g.cfg.Loopback)
	se.SetICETimeouts(2*time.Second, 10*time.Second, 500*time.Millisecond)
	return se
}

// Handler is the public signalling API (HTTPS on the GPU host).
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sessions", g.createSession)
	mux.HandleFunc("DELETE /v1/sessions/{sid}", g.deleteSession)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return g.cors(mux)
}

// AdminHandler is the Type3-facing API. Bind it to a private address.
func (g *Gateway) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/metrics", g.metricsJSON)
	mux.HandleFunc("GET /metrics", g.metricsProm)
	mux.HandleFunc("PUT /v1/limits", g.putLimits)
	mux.HandleFunc("DELETE /v1/sessions/{sid}", g.adminDelete)
	return g.admin(mux)
}

func (g *Gateway) admin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Bearer " + g.cfg.AdminToken
		if g.cfg.AdminToken == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (g *Gateway) cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			if !slices.Contains(g.cfg.AllowedOrigins, o) {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Allow-Methods", "POST, DELETE")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

type apiError struct {
	Code  int    `json:"code"`
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, apiError{Code: code, Error: msg})
}

// CreateRequest is the WHIP-style offer (PROTOCOL.md §2).
type CreateRequest struct {
	Token string            `json:"token"`
	SDP   string            `json:"sdp"`
	Hello *protocol.Control `json:"hello"`
}

type CreateResponse struct {
	SID     string           `json:"sid"`
	SDP     string           `json:"sdp"`
	Secret  string           `json:"secret"`
	Welcome protocol.Control `json:"welcome"`
}

func tokenError(err error) (int, int) {
	switch {
	case errors.Is(err, token.ErrWrongHost), errors.Is(err, token.ErrEntitlement):
		return http.StatusForbidden, protocol.CodeForbidden
	}
	return http.StatusUnauthorized, protocol.CodeBadToken
}

func (g *Gateway) createSession(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&req); err != nil || req.SDP == "" {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, "malformed request")
		return
	}
	hello := protocol.Control{T: "hello", V: protocol.VersionJSON(protocol.Version), Profile: "balanced"}
	if req.Hello != nil {
		hello = *req.Hello
	}
	if _, ok := protocol.NegotiateVersion(hello.Versions()); !ok {
		fail(w, http.StatusBadRequest, protocol.CodeVersion, "unsupported protocol version")
		return
	}
	// Verify consumes the jti only after the signature and claims check out.
	claims, err := g.verifier.Verify(req.Token)
	if err != nil {
		status, code := tokenError(err)
		g.rejected.Add(1)
		fail(w, status, code, err.Error())
		return
	}
	log := g.log.With("sid", claims.Session, "user", claims.User, "desktop", claims.Desktop)

	// An unknown desktop is a 404 even when the host is full.
	addr, err := g.cfg.Registry.Lookup(claims.Desktop)
	if err != nil {
		g.rejected.Add(1)
		log.Warn("agent lookup", "err", err)
		fail(w, http.StatusNotFound, protocol.CodeAgentDown, "desktop is not running on this host")
		return
	}
	g.mu.Lock()
	prev := g.byDesktop[claims.Desktop]
	active := len(g.sessions)
	if prev != nil {
		active-- // a reconnect replaces the existing session
	}
	ok, why := Admit(g.limits, active, g.cfg.Sampler.Last())
	g.mu.Unlock()
	if !ok {
		g.rejected.Add(1)
		log.Warn("rejected: capacity", "reason", why)
		fail(w, http.StatusServiceUnavailable, protocol.CodeCapacity, why)
		return
	}
	if prev != nil {
		code := protocol.CodeTakenOver
		if prev.claims.User == claims.User {
			code = protocol.CodeNormal // the same user reconnecting
		}
		prev.Close(code, "replaced by a newer connection")
	}

	offerCodecs := codecsInSDP(req.SDP)
	clientCodecs := offerCodecs
	if len(hello.Codecs) > 0 {
		clientCodecs = intersect(hello.Codecs, offerCodecs)
	}
	if len(clientCodecs) == 0 {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, "offer has no supported video codec")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), g.cfg.AgentDialTimeout+g.cfg.ReadyTimeout+5*time.Second)
	defer cancel()
	s, answer, err := g.startSession(ctx, claims, req.Token, hello, clientCodecs, addr, req.SDP, log)
	if err != nil {
		g.rejected.Add(1)
		var ce codeError
		if errors.As(err, &ce) {
			fail(w, http.StatusBadGateway, ce.code, ce.msg)
		} else {
			fail(w, http.StatusBadGateway, protocol.CodeAgentDown, err.Error())
		}
		log.Warn("session start failed", "err", err)
		return
	}
	g.admitted.Add(1)
	writeJSON(w, http.StatusCreated, CreateResponse{SID: s.sid, SDP: answer, Secret: s.secret, Welcome: s.welcome()})
}

func (g *Gateway) register(s *Session) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessions[s.sid] = s
	g.byDesktop[s.claims.Desktop] = s
}

func (g *Gateway) unregister(s *Session) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sessions[s.sid] == s {
		delete(g.sessions, s.sid)
		g.ended.Add(1)
	}
	if g.byDesktop[s.claims.Desktop] == s {
		delete(g.byDesktop, s.claims.Desktop)
	}
}

func (g *Gateway) deleteSession(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	s := g.sessions[r.PathValue("sid")]
	g.mu.Unlock()
	secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s == nil || subtle.ConstantTimeCompare([]byte(secret), []byte(s.secret)) != 1 {
		fail(w, http.StatusNotFound, protocol.CodeForbidden, "no such session")
		return
	}
	s.Close(protocol.CodeNormal, "client disconnect")
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) adminDelete(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	s := g.sessions[r.PathValue("sid")]
	g.mu.Unlock()
	if s == nil {
		http.NotFound(w, r)
		return
	}
	s.Close(protocol.CodeNormal, "stopped by administrator")
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) putLimits(w http.ResponseWriter, r *http.Request) {
	var l Limits
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&l); err != nil || l.MaxSessions < 0 {
		http.Error(w, "bad limits", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.limits = l
	g.mu.Unlock()
	g.log.Info("limits updated", "limits", l)
	writeJSON(w, http.StatusOK, l)
}

// codecsInSDP lists the MEA codec names present in an SDP offer's rtpmaps.
func codecsInSDP(sdp string) []string {
	var out []string
	add := func(c string) {
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	for _, line := range strings.Split(sdp, "\n") {
		if !strings.HasPrefix(line, "a=rtpmap:") {
			continue
		}
		l := strings.ToUpper(line)
		switch {
		case strings.Contains(l, " H264/"):
			add(protocol.H264)
		case strings.Contains(l, " H265/"):
			add(protocol.H265)
		case strings.Contains(l, " AV1/"):
			add(protocol.AV1)
		}
	}
	return out
}

func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		if slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

type codeError struct {
	code int
	msg  string
}

func (e codeError) Error() string { return fmt.Sprintf("%d: %s", e.code, e.msg) }

// Package hosts implements the multi-host registry: dynamic registration,
// persistent host records, mTLS connections, health tracking with
// disconnect/reconnect handling, resource discovery and basic placement.
package hosts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/canonical/lxd/shared/api"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/gpu"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/lxdclient"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/secrets"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/validate"
)

// HostRegistry is the contract the API and container manager use.
type HostRegistry interface {
	Register(ctx context.Context, req RegisterRequest) (model.Host, error)
	List(ctx context.Context) ([]model.Host, error)
	Get(ctx context.Context, id string) (model.Host, error)
	Unregister(ctx context.Context, id string, force bool) (int, error)
	Client(ctx context.Context, id string) (lxdclient.Server, model.Host, error)
	Health(ctx context.Context, id string) (HealthReport, error)
	Resources(ctx context.Context, id string) (HostResources, error)
	Select(ctx context.Context, req PlacementRequest) (model.Host, error)
	CheckPlacement(ctx context.Context, hostID string, req PlacementRequest) (model.Host, error)
}

// ResourceMonitor reports host health and resource inventory.
type ResourceMonitor interface {
	Health(ctx context.Context, id string) (HealthReport, error)
	Resources(ctx context.Context, id string) (HostResources, error)
}

// StorageManager exposes storage pools and networks of a host.
type StorageManager interface {
	StoragePools(ctx context.Context, id string) ([]PoolInfo, error)
	Networks(ctx context.Context, id string) ([]NetworkInfo, error)
}

// Options configures the registry.
type Options struct {
	ClientCertRef        string
	ClientKeyRef         string
	CACertRef            string
	RequestTimeout       time.Duration
	FailureThreshold     int
	AllowPublicEndpoints bool
	AllowUnixSocket      bool
	DefaultNetwork       string
	DefaultStoragePool   string
	CPUOvercommit        float64
	MemoryOvercommit     float64
	MaxContainersPerGPU  int
	UserAgent            string
	// LookupIP resolves endpoint hostnames for the public-endpoint check.
	LookupIP func(ctx context.Context, host string) ([]net.IP, error)
	Now      func() time.Time
}

// RegisterRequest is the input for registering a host.
type RegisterRequest struct {
	Name               string            `json:"name"`
	Endpoint           string            `json:"endpoint"`
	Project            string            `json:"project"`
	ServerCert         string            `json:"server_cert"`
	ClientCertRef      string            `json:"client_cert_ref"`
	ClientKeyRef       string            `json:"client_key_ref"`
	Provider           string            `json:"provider"`
	ProviderRef        string            `json:"provider_ref"`
	Labels             map[string]string `json:"labels"`
	DefaultNetwork     string            `json:"default_network"`
	DefaultStoragePool string            `json:"default_storage_pool"`
	Schedulable        *bool             `json:"schedulable"`
	// AllowOffline registers the host even if it cannot be reached now
	// (e.g. a machine that is still booting).
	AllowOffline bool `json:"allow_offline"`
}

// Registry implements HostRegistry, ResourceMonitor and StorageManager.
type Registry struct {
	st        store.Store
	conn      lxdclient.Connector
	sec       *secrets.Resolver
	opt       Options
	log       *slog.Logger
	providers map[string]HostProvider

	mu      sync.Mutex
	clients map[string]lxdclient.Server

	writeMu sync.Mutex

	cbMu        sync.Mutex
	onReconnect []func(hostID string)
}

// NewRegistry builds a registry.
func NewRegistry(st store.Store, conn lxdclient.Connector, sec *secrets.Resolver, opt Options, log *slog.Logger) *Registry {
	if opt.RequestTimeout <= 0 {
		opt.RequestTimeout = 30 * time.Second
	}
	if opt.FailureThreshold < 1 {
		opt.FailureThreshold = 3
	}
	if opt.CPUOvercommit <= 0 {
		opt.CPUOvercommit = 1
	}
	if opt.MemoryOvercommit <= 0 {
		opt.MemoryOvercommit = 1
	}
	if opt.MaxContainersPerGPU <= 0 {
		opt.MaxContainersPerGPU = 4
	}
	if opt.LookupIP == nil {
		opt.LookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if sec == nil {
		sec = &secrets.Resolver{}
	}
	r := &Registry{
		st: st, conn: conn, sec: sec, opt: opt, log: log,
		providers: map[string]HostProvider{},
		clients:   map[string]lxdclient.Server{},
	}
	r.AddProvider(StaticProvider{})
	return r
}

// AddProvider registers a HostProvider (Milestone 2 adds EC2).
func (r *Registry) AddProvider(p HostProvider) { r.providers[p.Name()] = p }

// Provider returns the provider for a host.
func (r *Registry) Provider(h model.Host) (HostProvider, bool) {
	p, ok := r.providers[h.Provider]
	return p, ok
}

// OnReconnect registers fn to run (in its own goroutine) when a host comes
// back online after being offline or unknown.
func (r *Registry) OnReconnect(fn func(hostID string)) {
	r.cbMu.Lock()
	defer r.cbMu.Unlock()
	r.onReconnect = append(r.onReconnect, fn)
}

func (r *Registry) now() time.Time { return r.opt.Now().UTC() }

// Register validates, probes and persists a new host.
func (r *Registry) Register(ctx context.Context, req RegisterRequest) (model.Host, error) {
	if err := validate.Name("name", req.Name); err != nil {
		return model.Host{}, err
	}
	if err := validate.Labels(req.Labels); err != nil {
		return model.Host{}, err
	}
	if req.Provider == "" {
		req.Provider = "static"
	}
	if _, ok := r.providers[req.Provider]; !ok {
		return model.Host{}, errs.Invalid("unknown_provider", "provider %q is not available", req.Provider)
	}
	if req.Project == "" {
		req.Project = "default"
	}
	if err := validate.Name("project", req.Project); err != nil && req.Project != "default" {
		return model.Host{}, err
	}
	if err := r.checkEndpoint(ctx, req.Endpoint, req.AllowOffline); err != nil {
		return model.Host{}, err
	}
	scheme, _, _ := lxdclient.ParseEndpoint(req.Endpoint)
	if (req.ClientCertRef == "") != (req.ClientKeyRef == "") {
		return model.Host{}, errs.Invalid("invalid_client_identity", "client_cert_ref and client_key_ref must be set together")
	}
	for _, ref := range []string{req.ClientCertRef, req.ClientKeyRef} {
		if strings.Contains(ref, "PRIVATE KEY") || strings.Contains(ref, "BEGIN CERTIFICATE") {
			return model.Host{}, errs.Invalid("inline_secret_rejected", "client_cert_ref/client_key_ref must be references (path, file://, env://, s3://), not PEM contents")
		}
	}

	h := model.Host{
		ID: req.Name, Name: req.Name, Endpoint: strings.TrimSuffix(req.Endpoint, "/"), Project: req.Project,
		ClientCertRef: req.ClientCertRef, ClientKeyRef: req.ClientKeyRef,
		Provider: req.Provider, ProviderRef: req.ProviderRef, Labels: req.Labels,
		DefaultNetwork:     firstNonEmpty(req.DefaultNetwork, r.opt.DefaultNetwork),
		DefaultStoragePool: firstNonEmpty(req.DefaultStoragePool, r.opt.DefaultStoragePool),
		Schedulable:        req.Schedulable == nil || *req.Schedulable,
		Status:             model.HostUnknown,
	}
	if scheme == "https" {
		if strings.TrimSpace(req.ServerCert) == "" && r.opt.CACertRef == "" {
			return model.Host{}, errs.Invalid("server_cert_required", "server_cert (the LXD server certificate PEM) is required unless a CA is configured")
		}
		if req.ServerCert != "" {
			fp, err := lxdclient.CertFingerprint(req.ServerCert)
			if err != nil {
				return model.Host{}, errs.Invalid("invalid_server_cert", "server_cert: %v", err)
			}
			h.ServerCertPEM = strings.TrimSpace(req.ServerCert) + "\n"
			h.ServerCertFingerprint = fp
		}
	}

	r.writeMu.Lock()
	existing, err := r.st.ListHosts(ctx)
	if err != nil {
		r.writeMu.Unlock()
		return model.Host{}, err
	}
	for _, e := range existing {
		if e.ID == h.ID {
			r.writeMu.Unlock()
			return model.Host{}, errs.Conflict("host_exists", "host %q is already registered", h.ID)
		}
		if e.Endpoint == h.Endpoint && e.Project == h.Project {
			r.writeMu.Unlock()
			return model.Host{}, errs.Conflict("endpoint_registered", "endpoint is already registered as host %q", e.ID)
		}
	}
	now := r.now()
	h.CreatedAt, h.UpdatedAt = now, now
	r.writeMu.Unlock()

	// Probe before persisting so a bad certificate or endpoint fails fast.
	report, perr := r.probe(ctx, &h)
	if perr != nil && !req.AllowOffline {
		r.dropClient(h.ID)
		return model.Host{}, errs.Wrap(perr, errs.KindUnavailable, "host_unreachable", "cannot reach LXD at %s", h.Endpoint).
			WithDetails(map[string]any{"checks": report.Checks})
	}
	if perr == nil {
		if res, err := r.discover(ctx, h); err == nil {
			h.Capacity = res
		} else {
			r.log.Warn("resource discovery failed", "host", h.ID, "err", err)
		}
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if _, err := r.st.GetHost(ctx, h.ID); err == nil {
		// Lost a race with a concurrent registration of the same name; keep
		// the winner's connection.
		return model.Host{}, errs.Conflict("host_exists", "host %q is already registered", h.ID)
	}
	if err := r.st.PutHost(ctx, h); err != nil {
		return model.Host{}, err
	}
	r.log.Info("host registered", "host", h.ID, "endpoint", h.Endpoint, "status", h.Status, "provider", h.Provider)
	return h, nil
}

// checkEndpoint enforces that LXD management endpoints are private.
func (r *Registry) checkEndpoint(ctx context.Context, endpoint string, allowOffline bool) error {
	scheme, _, err := lxdclient.ParseEndpoint(endpoint)
	if err != nil {
		return errs.Invalid("invalid_endpoint", "%v", err)
	}
	if scheme == "unix" {
		if !r.opt.AllowUnixSocket {
			return errs.Invalid("unix_socket_disabled", "unix socket endpoints are disabled (TYPE3_ALLOW_UNIX_SOCKET=false)")
		}
		return nil
	}
	if r.opt.AllowPublicEndpoints {
		return nil
	}
	u, _ := url.Parse(endpoint)
	host := u.Hostname()
	ips := []net.IP{}
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	} else {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		resolved, err := r.opt.LookupIP(lctx, host)
		if err != nil || len(resolved) == 0 {
			if allowOffline {
				return nil
			}
			return errs.Invalid("endpoint_unresolvable", "cannot resolve %q to verify it is a private address", host)
		}
		ips = resolved
	}
	for _, ip := range ips {
		if !IsPrivateIP(ip) {
			return errs.Invalid("public_endpoint_rejected",
				"LXD endpoint %s resolves to a public address; LXD management must stay on a private network (set TYPE3_ALLOW_PUBLIC_LXD_ENDPOINTS=true only behind a firewall/VPN)", host)
		}
	}
	return nil
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// IsPrivateIP reports whether ip is loopback, RFC1918/ULA, link-local or CGNAT.
func IsPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}

// List returns all hosts.
func (r *Registry) List(ctx context.Context) ([]model.Host, error) { return r.st.ListHosts(ctx) }

// Get returns one host.
func (r *Registry) Get(ctx context.Context, id string) (model.Host, error) {
	return r.st.GetHost(ctx, id)
}

// Unregister removes a host. It refuses while Type3 manages containers on the
// host unless force is set; force only forgets Type3's records and never
// deletes LXD instances. It returns how many container records were dropped.
func (r *Registry) Unregister(ctx context.Context, id string, force bool) (int, error) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if _, err := r.st.GetHost(ctx, id); err != nil {
		return 0, err
	}
	cs, err := r.st.ListContainers(ctx, store.ContainerFilter{HostID: id})
	if err != nil {
		return 0, err
	}
	if len(cs) > 0 && !force {
		return 0, errs.Conflict("host_has_containers", "host %q still has %d managed containers; delete them first or pass force=true to forget them", id, len(cs)).
			WithDetails(map[string]any{"containers": len(cs)})
	}
	for _, c := range cs {
		if err := r.st.DeleteContainer(ctx, c.ID); err != nil {
			return 0, err
		}
	}
	if err := r.st.DeleteHost(ctx, id); err != nil {
		return 0, err
	}
	r.dropClient(id)
	r.log.Info("host unregistered", "host", id, "forgotten_containers", len(cs))
	return len(cs), nil
}

func (r *Registry) target(ctx context.Context, h model.Host) (lxdclient.Target, error) {
	t := lxdclient.Target{Endpoint: h.Endpoint, Project: h.Project, ServerCertPEM: h.ServerCertPEM, UserAgent: r.opt.UserAgent}
	scheme, _, err := lxdclient.ParseEndpoint(h.Endpoint)
	if err != nil {
		return t, err
	}
	if scheme == "unix" {
		return t, nil
	}
	certRef, keyRef := h.ClientCertRef, h.ClientKeyRef
	if certRef == "" {
		certRef, keyRef = r.opt.ClientCertRef, r.opt.ClientKeyRef
	}
	if certRef == "" {
		return t, errors.New("no LXD client certificate configured (TYPE3_LXD_CLIENT_CERT/TYPE3_LXD_CLIENT_KEY)")
	}
	if t.ClientCertPEM, err = r.sec.Resolve(ctx, certRef); err != nil {
		return t, fmt.Errorf("client certificate: %w", err)
	}
	if t.ClientKeyPEM, err = r.sec.Resolve(ctx, keyRef); err != nil {
		return t, fmt.Errorf("client key: %w", err)
	}
	if t.CACertPEM, err = r.sec.Resolve(ctx, r.opt.CACertRef); err != nil {
		return t, fmt.Errorf("CA certificate: %w", err)
	}
	return t, nil
}

func (r *Registry) cachedClient(id string) lxdclient.Server {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clients[id]
}

func (r *Registry) connect(ctx context.Context, h model.Host) (lxdclient.Server, error) {
	if c := r.cachedClient(h.ID); c != nil {
		return c, nil
	}
	t, err := r.target(ctx, h)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, r.opt.RequestTimeout)
	defer cancel()
	c, err := r.conn.Connect(cctx, t)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing := r.clients[h.ID]; existing != nil {
		c.Disconnect()
		return existing, nil
	}
	r.clients[h.ID] = c
	return c, nil
}

func (r *Registry) dropClient(id string) {
	r.mu.Lock()
	c := r.clients[id]
	delete(r.clients, id)
	r.mu.Unlock()
	if c != nil {
		c.Disconnect()
	}
}

// Client returns a connection to a host for LXD calls. Offline hosts get one
// reconnect attempt; on failure an unavailable error is returned.
func (r *Registry) Client(ctx context.Context, id string) (lxdclient.Server, model.Host, error) {
	h, err := r.st.GetHost(ctx, id)
	if err != nil {
		return nil, h, err
	}
	c, err := r.connect(ctx, h)
	if err != nil {
		r.recordFailure(ctx, id, err)
		return nil, h, errs.Wrap(err, errs.KindUnavailable, "host_unavailable", "host %q is unavailable", id)
	}
	return c, h, nil
}

// ReportFailure lets callers report an LXD transport failure on a host so the
// connection is re-established next time.
func (r *Registry) ReportFailure(ctx context.Context, id string, err error) {
	r.recordFailure(ctx, id, err)
}

// HealthCheck is one item of a host health report.
type HealthCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// HealthReport is the result of probing a host.
type HealthReport struct {
	HostID              string           `json:"host_id"`
	Status              model.HostStatus `json:"status"`
	Reason              string           `json:"reason,omitempty"`
	LatencyMillis       int64            `json:"latency_ms"`
	CheckedAt           time.Time        `json:"checked_at"`
	LastSeenAt          *time.Time       `json:"last_seen_at,omitempty"`
	ConsecutiveFailures int              `json:"consecutive_failures"`
	ServerVersion       string           `json:"server_version,omitempty"`
	Clustered           bool             `json:"clustered"`
	Checks              []HealthCheck    `json:"checks"`
	Error               string           `json:"error,omitempty"`
}

// probe contacts the host and updates h's health fields in place. It does
// not persist; callers do.
func (r *Registry) probe(ctx context.Context, h *model.Host) (HealthReport, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opt.RequestTimeout)
	defer cancel()
	start := r.opt.Now()
	now := r.now()
	rep := HealthReport{HostID: h.ID, CheckedAt: now}
	h.LastCheckAt = &now

	c, err := r.connect(ctx, *h)
	var info *api.Server
	if err == nil {
		info, err = c.GetServer(ctx)
	}
	rep.LatencyMillis = r.opt.Now().Sub(start).Milliseconds()
	if err != nil {
		r.dropClient(h.ID)
		h.ConsecutiveFailures++
		h.LastError = sanitize(err)
		if h.ConsecutiveFailures >= r.opt.FailureThreshold || h.Status == model.HostUnknown || h.Status == model.HostOffline {
			h.Status = model.HostOffline
			h.StatusReason = "unreachable"
		} else {
			h.Status = model.HostDegraded
			h.StatusReason = fmt.Sprintf("%d consecutive failed checks", h.ConsecutiveFailures)
		}
		rep.Checks = append(rep.Checks, HealthCheck{Name: "lxd_api", OK: false, Detail: h.LastError})
		rep.Status, rep.Reason, rep.Error = h.Status, h.StatusReason, h.LastError
		rep.ConsecutiveFailures, rep.LastSeenAt = h.ConsecutiveFailures, h.LastSeenAt
		return rep, err
	}

	rep.Checks = append(rep.Checks, HealthCheck{Name: "lxd_api", OK: true, Detail: "LXD " + info.Environment.ServerVersion})
	h.ServerName = info.Environment.ServerName
	h.ServerVersion = info.Environment.ServerVersion
	h.Clustered = info.Environment.ServerClustered
	if h.Clustered {
		if members, err := c.GetClusterMembers(ctx); err == nil {
			h.ClusterMembers = h.ClusterMembers[:0]
			for _, m := range members {
				h.ClusterMembers = append(h.ClusterMembers, m.ServerName)
			}
		}
	}

	var problems []string
	if nets, err := c.GetNetworks(ctx); err != nil {
		rep.Checks = append(rep.Checks, HealthCheck{Name: "networks", OK: false, Detail: sanitize(err)})
		problems = append(problems, "networks unavailable")
	} else {
		ok := slices.ContainsFunc(nets, func(n api.Network) bool { return n.Name == h.DefaultNetwork })
		rep.Checks = append(rep.Checks, HealthCheck{Name: "network:" + h.DefaultNetwork, OK: ok})
		if !ok {
			problems = append(problems, "default network "+h.DefaultNetwork+" missing")
		}
	}
	if pools, err := c.GetStoragePools(ctx); err != nil {
		rep.Checks = append(rep.Checks, HealthCheck{Name: "storage_pools", OK: false, Detail: sanitize(err)})
		problems = append(problems, "storage pools unavailable")
	} else {
		ok := slices.ContainsFunc(pools, func(p api.StoragePool) bool { return p.Name == h.DefaultStoragePool })
		rep.Checks = append(rep.Checks, HealthCheck{Name: "storage_pool:" + h.DefaultStoragePool, OK: ok})
		if !ok {
			problems = append(problems, "default storage pool "+h.DefaultStoragePool+" missing")
		}
	}

	h.ConsecutiveFailures = 0
	h.LastError = ""
	h.LastSeenAt = &now
	h.LatencyMillis = rep.LatencyMillis
	if len(problems) > 0 {
		h.Status = model.HostDegraded
		h.StatusReason = strings.Join(problems, "; ")
	} else {
		h.Status = model.HostOnline
		h.StatusReason = ""
	}
	rep.Status, rep.Reason = h.Status, h.StatusReason
	rep.LastSeenAt, rep.ServerVersion, rep.Clustered = h.LastSeenAt, h.ServerVersion, h.Clustered
	return rep, nil
}

func sanitize(err error) string {
	s := err.Error()
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// Health probes a host now and persists the result.
func (r *Registry) Health(ctx context.Context, id string) (HealthReport, error) {
	h, err := r.st.GetHost(ctx, id)
	if err != nil {
		return HealthReport{}, err
	}
	prev := h.Status
	rep, _ := r.probe(ctx, &h)
	if err := r.saveHealth(ctx, h); err != nil {
		return rep, err
	}
	r.afterProbe(ctx, h, prev)
	return rep, nil
}

func (r *Registry) afterProbe(ctx context.Context, h model.Host, prev model.HostStatus) {
	if prev == h.Status {
		return
	}
	r.log.Info("host status changed", "host", h.ID, "from", prev, "to", h.Status, "reason", h.StatusReason)
	recovered := (prev == model.HostOffline || prev == model.HostUnknown) &&
		(h.Status == model.HostOnline || h.Status == model.HostDegraded)
	if !recovered {
		return
	}
	if res, err := r.discover(ctx, h); err == nil {
		r.updateHost(ctx, h.ID, func(hh *model.Host) { hh.Capacity = res })
	}
	r.cbMu.Lock()
	cbs := slices.Clone(r.onReconnect)
	r.cbMu.Unlock()
	for _, fn := range cbs {
		go fn(h.ID)
	}
}

// saveHealth writes only the health-related fields so concurrent edits to
// other fields are not lost.
func (r *Registry) saveHealth(ctx context.Context, h model.Host) error {
	return r.updateHost(ctx, h.ID, func(cur *model.Host) {
		cur.Status, cur.StatusReason, cur.LastError = h.Status, h.StatusReason, h.LastError
		cur.LastSeenAt, cur.LastCheckAt = h.LastSeenAt, h.LastCheckAt
		cur.ConsecutiveFailures, cur.LatencyMillis = h.ConsecutiveFailures, h.LatencyMillis
		cur.ServerName, cur.ServerVersion = h.ServerName, h.ServerVersion
		cur.Clustered, cur.ClusterMembers = h.Clustered, h.ClusterMembers
	})
}

func (r *Registry) updateHost(ctx context.Context, id string, fn func(*model.Host)) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	cur, err := r.st.GetHost(ctx, id)
	if err != nil {
		return err
	}
	fn(&cur)
	cur.UpdatedAt = r.now()
	return r.st.PutHost(ctx, cur)
}

func (r *Registry) recordFailure(ctx context.Context, id string, cause error) {
	r.dropClient(id)
	var prev model.HostStatus
	var next model.Host
	err := r.updateHost(ctx, id, func(h *model.Host) {
		prev = h.Status
		h.ConsecutiveFailures++
		h.LastError = sanitize(cause)
		if h.ConsecutiveFailures >= r.opt.FailureThreshold {
			h.Status, h.StatusReason = model.HostOffline, "unreachable"
		} else if h.Status == model.HostOnline {
			h.Status, h.StatusReason = model.HostDegraded, "request failed"
		}
		next = *h
	})
	if err == nil && prev != next.Status {
		r.log.Warn("host status changed", "host", id, "from", prev, "to", next.Status, "err", next.LastError)
	}
}

// CheckAll probes every host concurrently (bounded) and persists results.
func (r *Registry) CheckAll(ctx context.Context) {
	hs, err := r.st.ListHosts(ctx)
	if err != nil {
		r.log.Error("list hosts for health check", "err", err)
		return
	}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, h := range hs {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := r.Health(ctx, id); err != nil && !errs.Is(err, errs.KindNotFound) {
				r.log.Warn("health check", "host", id, "err", err)
			}
		}(h.ID)
	}
	wg.Wait()
}

// Run probes all hosts every interval until ctx ends.
func (r *Registry) Run(ctx context.Context, interval time.Duration) {
	r.CheckAll(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.CheckAll(ctx)
		}
	}
}

// Close drops all cached connections.
func (r *Registry) Close() {
	r.mu.Lock()
	cs := r.clients
	r.clients = map[string]lxdclient.Server{}
	r.mu.Unlock()
	for _, c := range cs {
		c.Disconnect()
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// discover fetches capacity totals for placement.
func (r *Registry) discover(ctx context.Context, h model.Host) (model.HostCapacity, error) {
	c, err := r.connect(ctx, h)
	if err != nil {
		return model.HostCapacity{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.opt.RequestTimeout)
	defer cancel()
	res, err := c.GetServerResources(ctx)
	if err != nil {
		return model.HostCapacity{}, err
	}
	inv := gpu.FromResources(res)
	nv := inv.Nvidia
	if !inv.NvidiaDriverReady {
		nv = 0
	}
	return model.HostCapacity{
		CPUThreads: res.CPU.Total, MemoryBytes: res.Memory.Total,
		GPUs: inv.Total, NvidiaGPUs: nv, DiscoveredAt: r.now(),
	}, nil
}

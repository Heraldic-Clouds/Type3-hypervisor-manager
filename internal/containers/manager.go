// Package containers implements the container lifecycle on top of the host
// registry: idempotent creation from tier profiles, start/stop/restart,
// deletion, resource changes, snapshots and reconciliation of Type3's
// records with the real LXD state.
package containers

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/canonical/lxd/shared/api"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/gpu"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/hosts"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/images"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/lxdclient"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/operations"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/profiles"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/validate"
)

// ContainerManager is the contract used by the API.
type ContainerManager interface {
	Create(ctx context.Context, req CreateRequest, actor string) (CreateResult, error)
	List(ctx context.Context, f store.ContainerFilter) ([]model.Container, error)
	Get(ctx context.Context, id string, live bool) (View, error)
	Start(ctx context.Context, id, actor, idemKey string) (model.Operation, error)
	Stop(ctx context.Context, id string, req StopRequest, actor, idemKey string) (model.Operation, error)
	Restart(ctx context.Context, id string, req StopRequest, actor, idemKey string) (model.Operation, error)
	Delete(ctx context.Context, id string, req DeleteRequest, actor, idemKey string) (model.Operation, error)
	UpdateResources(ctx context.Context, id string, req profiles.ResourceRequest, actor, idemKey string) (model.Operation, error)
	ListSnapshots(ctx context.Context, id string) ([]Snapshot, error)
	CreateSnapshot(ctx context.Context, id string, req SnapshotRequest, actor, idemKey string) (model.Operation, error)
	RestoreSnapshot(ctx context.Context, id, snapshot, actor, idemKey string) (model.Operation, error)
	DeleteSnapshot(ctx context.Context, id, snapshot, actor string) (model.Operation, error)
	Reconcile(ctx context.Context, hostID string) (ReconcileReport, error)
}

// Options configures the Manager.
type Options struct {
	GracefulStopTimeout time.Duration
	OperationTimeout    time.Duration
	IdmapIsolated       bool
	PortIsolation       bool
	Now                 func() time.Time
}

// Manager implements ContainerManager.
type Manager struct {
	st      store.Store
	hosts   hosts.HostRegistry
	images  images.ImageManager
	ops     operations.OperationManager
	catalog *profiles.Catalog
	opt     Options
	log     *slog.Logger

	locks sync.Map // container name -> *sync.Mutex
}

// NewManager builds a container manager.
func NewManager(st store.Store, h hosts.HostRegistry, im images.ImageManager, ops operations.OperationManager,
	catalog *profiles.Catalog, opt Options, log *slog.Logger) *Manager {
	if opt.GracefulStopTimeout <= 0 {
		opt.GracefulStopTimeout = 60 * time.Second
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Manager{st: st, hosts: h, images: im, ops: ops, catalog: catalog, opt: opt, log: log}
}

func (m *Manager) now() time.Time { return m.opt.Now().UTC() }

func (m *Manager) lock(name string) func() {
	v, _ := m.locks.LoadOrStore(name, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// View is a container record plus live/derived fields.
type View struct {
	model.Container
	// Stale is set when the host could not be queried and the state shown
	// is the last recorded one.
	Stale           bool             `json:"stale,omitempty"`
	ActiveOperation *model.Operation `json:"active_operation,omitempty"`
}

// CreateRequest creates a container from a tier profile.
type CreateRequest struct {
	Name    string `json:"name"`
	OwnerID string `json:"owner_id"`
	Tier    string `json:"tier"`
	// HostID selects a host explicitly; empty means automatic placement.
	HostID      string                   `json:"host_id"`
	Resources   profiles.ResourceRequest `json:"resources"`
	Image       *model.ImageRef          `json:"image"`
	Network     string                   `json:"network"`
	StoragePool string                   `json:"storage_pool"`
	// GPU may be omitted (tier default). true on a non-GPU tier is refused.
	GPU            *bool    `json:"gpu"`
	SSHKeys        []string `json:"ssh_authorized_keys"`
	Start          *bool    `json:"start"`
	IdempotencyKey string   `json:"-"`
}

// CreateResult is returned by Create.
type CreateResult struct {
	Container model.Container `json:"container"`
	Operation model.Operation `json:"operation"`
	// Existing is true when the request matched an earlier identical
	// request and nothing new was started.
	Existing bool `json:"existing"`
}

const resourceType = "container"

func (m *Manager) validateCreate(req *CreateRequest) (profiles.Tier, model.Resources, error) {
	if err := validate.Name("name", req.Name); err != nil {
		return profiles.Tier{}, model.Resources{}, err
	}
	if err := validate.OwnerID(req.OwnerID); err != nil {
		return profiles.Tier{}, model.Resources{}, err
	}
	if err := validate.IdempotencyKey(req.IdempotencyKey); err != nil {
		return profiles.Tier{}, model.Resources{}, err
	}
	if err := validate.SSHKeys(req.SSHKeys); err != nil {
		return profiles.Tier{}, model.Resources{}, err
	}
	for field, v := range map[string]string{"network": req.Network, "storage_pool": req.StoragePool, "host_id": req.HostID} {
		if v != "" {
			if err := validate.Name(field, v); err != nil {
				return profiles.Tier{}, model.Resources{}, err
			}
		}
	}
	tier, err := m.catalog.Get(req.Tier)
	if err != nil {
		return tier, model.Resources{}, err
	}
	if req.GPU != nil && *req.GPU && !tier.GPU {
		return tier, model.Resources{}, errs.New(errs.KindForbidden, "gpu_not_allowed",
			"tier %s does not include GPU access", tier.Name).WithDetails(map[string]any{"tier": tier.Name})
	}
	if req.GPU != nil && !*req.GPU && tier.GPU {
		return tier, model.Resources{}, errs.Invalid("gpu_required", "tier %s always includes a GPU", tier.Name)
	}
	res, err := tier.Resolve(req.Resources)
	if err != nil {
		return tier, res, err
	}
	return tier, res, nil
}

// Create validates the request against the tier profile, places the
// container and starts creation in the background. Identical repeated
// requests (same name and spec, or same Idempotency-Key) return the existing
// container and operation instead of creating a duplicate.
func (m *Manager) Create(ctx context.Context, req CreateRequest, actor string) (CreateResult, error) {
	tier, res, err := m.validateCreate(&req)
	if err != nil {
		return CreateResult{}, err
	}
	img := tier.Image
	if req.Image != nil {
		img = *req.Image
	}
	source, err := m.images.ResolveSource(img)
	if err != nil {
		return CreateResult{}, err
	}
	spec := profiles.SpecInput{
		OwnerID: req.OwnerID, Tier: tier.Name, Resources: res, Image: img,
		Network: req.Network, StoragePool: req.StoragePool, HostID: req.HostID, SSHKeys: req.SSHKeys,
	}
	hash := profiles.SpecHash(spec)

	unlock := m.lock(req.Name)
	defer unlock()

	if req.IdempotencyKey != "" {
		if c, err := m.st.FindContainerByIdempotencyKey(ctx, req.IdempotencyKey); err == nil {
			if c.ID != req.Name || c.SpecHash != hash {
				return CreateResult{}, errs.Conflict("idempotency_key_reused", "Idempotency-Key was already used for a different container request")
			}
			return m.existing(ctx, c)
		}
	}
	if c, err := m.st.GetContainer(ctx, req.Name); err == nil {
		if c.SpecHash == hash && c.Status != model.ContainerError && c.Status != model.ContainerMissing {
			return m.existing(ctx, c)
		}
		return CreateResult{}, errs.Conflict("container_exists", "container %q already exists", req.Name).
			WithDetails(map[string]any{"status": c.Status, "host_id": c.HostID})
	} else if !errs.Is(err, errs.KindNotFound) {
		return CreateResult{}, err
	}

	place := hosts.PlacementRequest{Resources: res, GPU: tier.GPU}
	var host model.Host
	if req.HostID != "" {
		host, err = m.hosts.CheckPlacement(ctx, req.HostID, place)
	} else {
		host, err = m.hosts.Select(ctx, place)
	}
	if err != nil {
		return CreateResult{}, err
	}
	network := firstNonEmpty(req.Network, tier.Network, host.DefaultNetwork)
	pool := firstNonEmpty(req.StoragePool, tier.StoragePool, host.DefaultStoragePool)

	now := m.now()
	rec := model.Container{
		ID: req.Name, HostID: host.ID, OwnerID: req.OwnerID, Tier: tier.Name,
		Image: img, Resources: res, GPU: tier.GPU, RootAccess: tier.RootAccess, Ephemeral: tier.Ephemeral,
		Network: network, StoragePool: pool, Profiles: slices.Clone(tier.LXDProfiles),
		Status: model.ContainerPending, SpecHash: hash, IdempotencyKey: req.IdempotencyKey,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := m.st.PutContainer(ctx, rec); err != nil {
		return CreateResult{}, err
	}

	start := req.Start == nil || *req.Start
	inst := profiles.BuildInstance(profiles.InstanceSpec{
		Name: rec.ID, OwnerID: rec.OwnerID, Tier: tier, Resources: res, Source: source,
		Network: network, StoragePool: pool, SSHKeys: req.SSHKeys,
		IdmapIsolated: m.opt.IdmapIsolated, PortIsolation: m.opt.PortIsolation, SpecHash: hash,
	})

	op, _, err := m.ops.Submit(operations.Spec{
		Type: "container.create", ResourceType: resourceType, ResourceID: rec.ID, HostID: host.ID,
		Actor: actor, IdempotencyKey: req.IdempotencyKey, Timeout: m.opt.OperationTimeout,
	}, m.guard(host.ID, func(ctx context.Context, p *operations.Progress) (map[string]any, error) {
		return m.runCreate(ctx, p, rec, inst, start)
	}))
	if err != nil {
		_ = m.st.DeleteContainer(ctx, rec.ID)
		return CreateResult{}, err
	}
	rec.LastOperationID = op.ID
	_ = m.patch(ctx, rec.ID, func(c *model.Container) { c.LastOperationID = op.ID })
	m.log.Info("container create accepted", "container", rec.ID, "host", host.ID, "tier", tier.Name, "owner", rec.OwnerID, "op", op.ID)
	return CreateResult{Container: rec, Operation: op}, nil
}

func (m *Manager) existing(ctx context.Context, c model.Container) (CreateResult, error) {
	res := CreateResult{Container: c, Existing: true}
	if op, ok := m.ops.Active(resourceType, c.ID); ok {
		res.Operation = op
	} else if c.LastOperationID != "" {
		if op, err := m.ops.Get(ctx, c.LastOperationID); err == nil {
			res.Operation = op
		}
	}
	return res, nil
}

// patch applies fn to the stored record under the container lock-free path
// (the store itself is atomic per write).
func (m *Manager) patch(ctx context.Context, id string, fn func(*model.Container)) error {
	c, err := m.st.GetContainer(ctx, id)
	if err != nil {
		return err
	}
	fn(&c)
	c.UpdatedAt = m.now()
	return m.st.PutContainer(ctx, c)
}

func (m *Manager) setStatus(ctx context.Context, id string, st model.ContainerStatus, errMsg string) {
	_ = m.patch(ctx, id, func(c *model.Container) {
		c.Status = st
		c.Error = errMsg
	})
}

func (m *Manager) runCreate(ctx context.Context, p *operations.Progress, rec model.Container, inst api.InstancesPost, start bool) (map[string]any, error) {
	fail := func(err error) (map[string]any, error) {
		m.setStatus(context.Background(), rec.ID, model.ContainerError, err.Error())
		return nil, err
	}
	m.setStatus(ctx, rec.ID, model.ContainerCreating, "")
	p.Stage("connecting to host", 5)
	c, host, err := m.hosts.Client(ctx, rec.HostID)
	if err != nil {
		return fail(err)
	}
	if rec.GPU {
		res, err := c.GetServerResources(ctx)
		if err != nil {
			return fail(errs.Wrap(err, errs.KindUnavailable, "host_unavailable", "read GPU inventory"))
		}
		if err := gpu.CheckEligibility(true, true, gpu.FromResources(res)); err != nil {
			return fail(errs.Wrap(err, errs.KindPrecondition, "gpu_unavailable", "host %q cannot serve GPU containers", host.ID))
		}
	}

	// Retry safety: an instance with this name may exist from an earlier
	// attempt that failed after LXD accepted it.
	existing, _, err := c.GetInstance(ctx, rec.ID)
	switch {
	case err == nil:
		if existing.Config[profiles.KeyManaged] != "true" || existing.Config[profiles.KeySpecHash] != rec.SpecHash {
			return fail(errs.Conflict("instance_name_taken", "an instance named %q that Type3 did not create already exists on host %q", rec.ID, host.ID))
		}
		p.Stage("adopting instance from earlier attempt", 60)
	case lxdclient.IsNotFound(err):
		p.Stage("creating instance", 10)
		op, err := c.CreateInstance(ctx, inst)
		if err != nil {
			if lxdclient.IsConflict(err) {
				return fail(errs.Conflict("instance_name_taken", "instance %q already exists on host %q", rec.ID, host.ID))
			}
			return fail(errs.Wrap(err, errs.KindUnavailable, "lxd_create_failed", "create instance"))
		}
		if err := op.Wait(ctx, func(o api.Operation) { p.LXD(op.ID(), images.ProgressDetail(o)) }); err != nil {
			return fail(fmt.Errorf("create instance: %w", err))
		}
	default:
		return fail(errs.Wrap(err, errs.KindUnavailable, "host_unavailable", "check for existing instance"))
	}

	if start {
		p.Stage("starting instance", 70)
		if err := m.changeState(ctx, c, rec.ID, api.InstanceStatePut{Action: "start", Timeout: -1}, p); err != nil && !alreadyInState(err) {
			// The instance exists; record it as stopped with the error so a
			// retry of start (or reconciliation) can continue from here.
			m.setStatus(context.Background(), rec.ID, model.ContainerStopped, "start failed: "+err.Error())
			return nil, fmt.Errorf("start instance: %w", err)
		}
	}
	p.Stage("reading state", 95)
	v := m.observe(ctx, c, rec.ID)
	return map[string]any{"container_id": rec.ID, "host_id": rec.HostID, "status": v.Status, "ipv4": v.IPv4}, nil
}

func (m *Manager) changeState(ctx context.Context, c lxdclient.Server, name string, put api.InstanceStatePut, p *operations.Progress) error {
	op, err := c.UpdateInstanceState(ctx, name, put)
	if err != nil {
		return err
	}
	if p != nil {
		p.LXD(op.ID(), "")
	}
	return op.Wait(ctx, nil)
}

// alreadyInState matches LXD's errors for start-when-running and
// stop-when-stopped so repeated requests are harmless.
func alreadyInState(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range []string{"already running", "already stopped", "is not running", "isn't running"} {
		if containsFold(msg, s) {
			return true
		}
	}
	return false
}

// observe refreshes status and IP addresses from LXD and persists them.
func (m *Manager) observe(ctx context.Context, c lxdclient.Server, id string) model.Container {
	rec, _ := m.tryObserve(ctx, c, id)
	return rec
}

// tryObserve is observe that also returns the LXD error, if any.
func (m *Manager) tryObserve(ctx context.Context, c lxdclient.Server, id string) (model.Container, error) {
	rec, err := m.st.GetContainer(ctx, id)
	if err != nil {
		return rec, err
	}
	st, err := c.GetInstanceState(ctx, id)
	if lxdclient.IsNotFound(err) {
		if rec.Ephemeral {
			// LXD removes ephemeral containers when they stop.
			_ = m.st.DeleteContainer(ctx, id)
			rec.Status = model.ContainerMissing
			return rec, nil
		}
		rec.Status = model.ContainerMissing
		rec.IPv4, rec.IPv6 = nil, nil
	} else if err == nil {
		rec.Status = MapStatus(st.Status)
		rec.LXDStatus = st.Status
		rec.IPv4, rec.IPv6 = Addresses(st)
	} else {
		return rec, err
	}
	if inst, _, err := c.GetInstance(ctx, id); err == nil {
		rec.Location = inst.Location
	}
	now := m.now()
	rec.ObservedAt = &now
	rec.UpdatedAt = now
	if rec.Status != model.ContainerError {
		rec.Error = ""
	}
	_ = m.st.PutContainer(ctx, rec)
	return rec, nil
}

// MapStatus converts an LXD status string to a Type3 status.
func MapStatus(s string) model.ContainerStatus {
	switch s {
	case "Running":
		return model.ContainerRunning
	case "Stopped":
		return model.ContainerStopped
	case "Frozen":
		return model.ContainerFrozen
	case "Starting":
		return model.ContainerStarting
	case "Stopping":
		return model.ContainerStopping
	case "Error":
		return model.ContainerError
	default:
		return model.ContainerUnknown
	}
}

// Addresses returns the global IPv4/IPv6 addresses of the container's
// non-loopback interfaces.
func Addresses(st *api.InstanceState) (v4, v6 []string) {
	names := make([]string, 0, len(st.Network))
	for n := range st.Network {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if n == "lo" {
			continue
		}
		for _, a := range st.Network[n].Addresses {
			if a.Scope != "global" || a.Address == "" {
				continue
			}
			switch a.Family {
			case "inet":
				v4 = append(v4, a.Address)
			case "inet6":
				v6 = append(v6, a.Address)
			}
		}
	}
	return v4, v6
}

// List returns container records.
func (m *Manager) List(ctx context.Context, f store.ContainerFilter) ([]model.Container, error) {
	return m.st.ListContainers(ctx, f)
}

// Get returns a container; with live=true its state is refreshed from LXD
// (falling back to the recorded state, marked stale, if the host is down).
func (m *Manager) Get(ctx context.Context, id string, live bool) (View, error) {
	rec, err := m.st.GetContainer(ctx, id)
	if err != nil {
		return View{}, err
	}
	v := View{Container: rec}
	if op, ok := m.ops.Active(resourceType, id); ok {
		v.ActiveOperation = &op
	}
	if !live || v.ActiveOperation != nil {
		return v, nil
	}
	c, _, err := m.hosts.Client(ctx, rec.HostID)
	if err != nil {
		v.Stale = true
		return v, nil
	}
	cur, err := m.tryObserve(ctx, c, id)
	if err != nil {
		_ = m.hostErr(ctx, rec.HostID, err)
		v.Stale = true
		return v, nil
	}
	v.Container = cur
	return v, nil
}

func (m *Manager) record(ctx context.Context, id string) (model.Container, error) {
	if err := validate.Name("container_id", id); err != nil {
		return model.Container{}, errs.NotFound("container_not_found", "container %q not found", id)
	}
	rec, err := m.st.GetContainer(ctx, id)
	if err != nil {
		return rec, err
	}
	switch rec.Status {
	case model.ContainerPending, model.ContainerCreating:
		if _, busy := m.ops.Active(resourceType, id); busy {
			return rec, errs.Conflict("container_not_ready", "container %q is still being created", id)
		}
	case model.ContainerDeleting:
		if _, busy := m.ops.Active(resourceType, id); busy {
			return rec, errs.Conflict("container_deleting", "container %q is being deleted", id)
		}
	}
	return rec, nil
}

// submit runs a lifecycle task. A repeated request while the same kind of
// operation is already running returns that operation.
func (m *Manager) submit(rec model.Container, typ, actor, idemKey string, task operations.Task) (model.Operation, error) {
	if op, ok := m.ops.Active(resourceType, rec.ID); ok && op.Type == typ {
		return op, nil
	}
	op, _, err := m.ops.Submit(operations.Spec{
		Type: typ, ResourceType: resourceType, ResourceID: rec.ID, HostID: rec.HostID,
		Actor: actor, IdempotencyKey: idemKey, Timeout: m.opt.OperationTimeout,
	}, m.guard(rec.HostID, task))
	if err == nil {
		_ = m.patch(context.Background(), rec.ID, func(c *model.Container) { c.LastOperationID = op.ID })
	}
	return op, err
}

// guard converts transport failures into host_unavailable errors and tells
// the registry, so a dead connection is dropped and the host re-probed.
func (m *Manager) guard(hostID string, task operations.Task) operations.Task {
	return func(ctx context.Context, p *operations.Progress) (map[string]any, error) {
		res, err := task(ctx, p)
		return res, m.hostErr(ctx, hostID, err)
	}
}

func (m *Manager) hostErr(ctx context.Context, hostID string, err error) error {
	if !lxdclient.IsTransport(err) {
		return err
	}
	m.hosts.ReportFailure(context.WithoutCancel(ctx), hostID, err)
	return errs.Wrap(err, errs.KindUnavailable, "host_unavailable", "host %q is unreachable", hostID)
}

// Start starts a container. Starting a running container succeeds.
func (m *Manager) Start(ctx context.Context, id, actor, idemKey string) (model.Operation, error) {
	rec, err := m.record(ctx, id)
	if err != nil {
		return model.Operation{}, err
	}
	if rec.Status == model.ContainerMissing {
		return model.Operation{}, errs.Conflict("container_missing", "container %q no longer exists in LXD; delete it", id)
	}
	return m.submit(rec, "container.start", actor, idemKey, func(ctx context.Context, p *operations.Progress) (map[string]any, error) {
		c, _, err := m.hosts.Client(ctx, rec.HostID)
		if err != nil {
			return nil, err
		}
		m.setStatus(ctx, id, model.ContainerStarting, "")
		p.Stage("starting", 20)
		err = m.changeState(ctx, c, id, api.InstanceStatePut{Action: "start", Timeout: -1}, p)
		if err != nil && !alreadyInState(err) {
			m.observe(context.Background(), c, id)
			return nil, err
		}
		v := m.observe(ctx, c, id)
		return map[string]any{"status": v.Status, "ipv4": v.IPv4}, nil
	})
}

// StopRequest controls stop/restart.
type StopRequest struct {
	// Force kills the container if a graceful shutdown does not finish.
	Force bool `json:"force"`
	// Reason is required with Force and is written to the audit log.
	Reason string `json:"reason"`
	// TimeoutSeconds bounds the graceful shutdown (default from config).
	TimeoutSeconds int `json:"timeout_seconds"`
}

func (m *Manager) stopTimeout(req StopRequest) (int, error) {
	if req.Force {
		if err := validate.Reason(req.Reason); err != nil {
			return 0, err
		}
	}
	t := int(m.opt.GracefulStopTimeout / time.Second)
	if req.TimeoutSeconds != 0 {
		if err := validate.Range("timeout_seconds", req.TimeoutSeconds, 1, 600); err != nil {
			return 0, err
		}
		t = req.TimeoutSeconds
	}
	return t, nil
}

// stopInstance tries a graceful stop and, only if allowed, a forced one.
func (m *Manager) stopInstance(ctx context.Context, c lxdclient.Server, id string, timeout int, force bool, p *operations.Progress) (forced bool, err error) {
	p.Stage("graceful shutdown", 20)
	err = m.changeState(ctx, c, id, api.InstanceStatePut{Action: "stop", Timeout: timeout}, p)
	if err == nil || alreadyInState(err) || lxdclient.IsNotFound(err) {
		return false, nil
	}
	if !force {
		return false, errs.Wrap(err, errs.KindTimeout, "graceful_stop_failed", "container did not shut down gracefully; retry with force=true and a reason")
	}
	m.log.Warn("graceful stop failed, forcing", "container", id, "err", err)
	p.Stage("forced stop", 60)
	err = m.changeState(ctx, c, id, api.InstanceStatePut{Action: "stop", Force: true, Timeout: -1}, p)
	if err != nil && !alreadyInState(err) && !lxdclient.IsNotFound(err) {
		return true, err
	}
	return true, nil
}

// Stop stops a container gracefully, forcing only when requested.
func (m *Manager) Stop(ctx context.Context, id string, req StopRequest, actor, idemKey string) (model.Operation, error) {
	rec, err := m.record(ctx, id)
	if err != nil {
		return model.Operation{}, err
	}
	timeout, err := m.stopTimeout(req)
	if err != nil {
		return model.Operation{}, err
	}
	return m.submit(rec, "container.stop", actor, idemKey, func(ctx context.Context, p *operations.Progress) (map[string]any, error) {
		c, _, err := m.hosts.Client(ctx, rec.HostID)
		if err != nil {
			return nil, err
		}
		m.setStatus(ctx, id, model.ContainerStopping, "")
		forced, err := m.stopInstance(ctx, c, id, timeout, req.Force, p)
		v := m.observe(context.Background(), c, id)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"status": v.Status, "forced": forced}
		if rec.Ephemeral && v.Status == model.ContainerMissing {
			out["deleted"] = true
		}
		return out, nil
	})
}

// Restart restarts a running container, or starts a stopped one.
func (m *Manager) Restart(ctx context.Context, id string, req StopRequest, actor, idemKey string) (model.Operation, error) {
	rec, err := m.record(ctx, id)
	if err != nil {
		return model.Operation{}, err
	}
	timeout, err := m.stopTimeout(req)
	if err != nil {
		return model.Operation{}, err
	}
	return m.submit(rec, "container.restart", actor, idemKey, func(ctx context.Context, p *operations.Progress) (map[string]any, error) {
		c, _, err := m.hosts.Client(ctx, rec.HostID)
		if err != nil {
			return nil, err
		}
		st, err := c.GetInstanceState(ctx, id)
		if err != nil {
			return nil, err
		}
		m.setStatus(ctx, id, model.ContainerRestarting, "")
		forced := false
		if st.Status == "Running" {
			p.Stage("restarting", 30)
			err = m.changeState(ctx, c, id, api.InstanceStatePut{Action: "restart", Timeout: timeout}, p)
			if err != nil {
				if !req.Force {
					m.observe(context.Background(), c, id)
					return nil, errs.Wrap(err, errs.KindTimeout, "graceful_restart_failed", "restart failed; retry with force=true and a reason")
				}
				forced = true
				p.Stage("forced restart", 50)
				if err := m.changeState(ctx, c, id, api.InstanceStatePut{Action: "restart", Force: true, Timeout: -1}, p); err != nil {
					m.observe(context.Background(), c, id)
					return nil, err
				}
			}
		} else {
			p.Stage("starting", 50)
			if err := m.changeState(ctx, c, id, api.InstanceStatePut{Action: "start", Timeout: -1}, p); err != nil && !alreadyInState(err) {
				m.observe(context.Background(), c, id)
				return nil, err
			}
		}
		v := m.observe(ctx, c, id)
		return map[string]any{"status": v.Status, "forced": forced}, nil
	})
}

// DeleteRequest controls deletion.
type DeleteRequest struct {
	// Force stops a running container before deleting it.
	Force  bool   `json:"force"`
	Reason string `json:"reason"`
}

// Delete removes a container and its snapshots. A running container needs
// force=true. Deleting an instance LXD no longer has just removes the record.
func (m *Manager) Delete(ctx context.Context, id string, req DeleteRequest, actor, idemKey string) (model.Operation, error) {
	rec, err := m.st.GetContainer(ctx, id)
	if err != nil {
		return model.Operation{}, err
	}
	if req.Force {
		if err := validate.Reason(req.Reason); err != nil {
			return model.Operation{}, err
		}
	}
	if op, ok := m.ops.Active(resourceType, id); ok && op.Type != "container.delete" {
		return model.Operation{}, errs.Conflict("operation_in_progress", "container %q has a %s operation in progress", id, op.Type).
			WithDetails(map[string]any{"operation_id": op.ID})
	}
	if rec.Status == model.ContainerRunning && !req.Force {
		return model.Operation{}, errs.Conflict("container_running", "container %q is running; stop it first or pass force=true with a reason", id)
	}
	return m.submit(rec, "container.delete", actor, idemKey, func(ctx context.Context, p *operations.Progress) (map[string]any, error) {
		c, _, err := m.hosts.Client(ctx, rec.HostID)
		if err != nil {
			return nil, err
		}
		prev := rec.Status
		m.setStatus(ctx, id, model.ContainerDeleting, "")
		st, err := c.GetInstanceState(ctx, id)
		if lxdclient.IsNotFound(err) {
			if err := m.st.DeleteContainer(ctx, id); err != nil {
				return nil, err
			}
			return map[string]any{"deleted": true, "instance_was_missing": true}, nil
		}
		if err != nil {
			m.setStatus(context.Background(), id, prev, "")
			return nil, err
		}
		if st.Status == "Running" || st.Status == "Frozen" {
			if !req.Force {
				m.setStatus(context.Background(), id, model.ContainerRunning, "")
				return nil, errs.Conflict("container_running", "container %q is running; stop it first or pass force=true with a reason", id)
			}
			if _, err := m.stopInstance(ctx, c, id, int(m.opt.GracefulStopTimeout/time.Second), true, p); err != nil {
				m.observe(context.Background(), c, id)
				return nil, err
			}
		}
		p.Stage("deleting instance", 70)
		op, err := c.DeleteInstance(ctx, id)
		if err != nil && !lxdclient.IsNotFound(err) {
			m.observe(context.Background(), c, id)
			return nil, err
		}
		if err == nil {
			p.LXD(op.ID(), "")
			if err := op.Wait(ctx, nil); err != nil {
				m.observe(context.Background(), c, id)
				return nil, err
			}
		}
		if err := m.st.DeleteContainer(ctx, id); err != nil {
			return nil, err
		}
		return map[string]any{"deleted": true}, nil
	})
}

// UpdateResources changes CPU, memory and disk within the tier bounds.
// CPU and memory apply live; the root disk can only grow.
func (m *Manager) UpdateResources(ctx context.Context, id string, req profiles.ResourceRequest, actor, idemKey string) (model.Operation, error) {
	rec, err := m.record(ctx, id)
	if err != nil {
		return model.Operation{}, err
	}
	if req.CPU == nil && req.MemoryMiB == nil && req.DiskGiB == nil {
		return model.Operation{}, errs.Invalid("empty_update", "set at least one of cpu, memory_mib or disk_gib")
	}
	tier, err := m.catalog.Get(rec.Tier)
	if err != nil {
		return model.Operation{}, err
	}
	next, err := tier.Apply(rec.Resources, req)
	if err != nil {
		return model.Operation{}, err
	}
	if next.DiskGiB < rec.Resources.DiskGiB {
		return model.Operation{}, errs.Invalid("disk_shrink_unsupported", "root disk cannot shrink (current %d GiB)", rec.Resources.DiskGiB)
	}
	delta := hosts.PlacementRequest{Resources: model.Resources{
		CPU: max(0, next.CPU-rec.Resources.CPU), MemoryMiB: max(0, next.MemoryMiB-rec.Resources.MemoryMiB),
	}}
	if _, err := m.hosts.CheckPlacement(ctx, rec.HostID, delta); err != nil {
		return model.Operation{}, err
	}
	return m.submit(rec, "container.update_resources", actor, idemKey, func(ctx context.Context, p *operations.Progress) (map[string]any, error) {
		c, _, err := m.hosts.Client(ctx, rec.HostID)
		if err != nil {
			return nil, err
		}
		p.Stage("updating limits", 30)
		inst, etag, err := c.GetInstance(ctx, id)
		if err != nil {
			return nil, err
		}
		put := inst.Writable()
		if put.Config == nil {
			put.Config = map[string]string{}
		}
		for k, v := range profiles.ResourceConfig(next) {
			put.Config[k] = v
		}
		if root, ok := put.Devices[profiles.RootDevice]; ok {
			root["size"] = fmt.Sprintf("%dGiB", next.DiskGiB)
		} else {
			if put.Devices == nil {
				put.Devices = map[string]map[string]string{}
			}
			put.Devices[profiles.RootDevice] = profiles.RootDisk(rec.StoragePool, next.DiskGiB)
		}
		op, err := c.UpdateInstance(ctx, id, put, etag)
		if err != nil {
			return nil, err
		}
		p.LXD(op.ID(), "")
		if err := op.Wait(ctx, nil); err != nil {
			return nil, err
		}
		if err := m.patch(ctx, id, func(c *model.Container) { c.Resources = next }); err != nil {
			return nil, err
		}
		return map[string]any{"resources": next}, nil
	})
}

// Snapshot describes an LXD snapshot.
type Snapshot struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Stateful  bool      `json:"stateful"`
	SizeBytes int64     `json:"size_bytes"`
}

// SnapshotRequest creates a snapshot.
type SnapshotRequest struct {
	Name string `json:"name"`
}

// ListSnapshots lists a container's snapshots.
func (m *Manager) ListSnapshots(ctx context.Context, id string) ([]Snapshot, error) {
	rec, err := m.st.GetContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	c, _, err := m.hosts.Client(ctx, rec.HostID)
	if err != nil {
		return nil, err
	}
	snaps, err := c.GetInstanceSnapshots(ctx, id)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindUnavailable, "host_unavailable", "list snapshots")
	}
	out := make([]Snapshot, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, Snapshot{Name: s.Name, CreatedAt: s.CreatedAt, Stateful: s.Stateful, SizeBytes: s.Size})
	}
	return out, nil
}

// CreateSnapshot takes a (stateless) snapshot.
func (m *Manager) CreateSnapshot(ctx context.Context, id string, req SnapshotRequest, actor, idemKey string) (model.Operation, error) {
	rec, err := m.record(ctx, id)
	if err != nil {
		return model.Operation{}, err
	}
	if req.Name == "" {
		req.Name = "snap-" + m.now().Format("20060102-150405")
	}
	if err := validate.SnapshotName(req.Name); err != nil {
		return model.Operation{}, err
	}
	return m.submit(rec, "container.snapshot", actor, idemKey, func(ctx context.Context, p *operations.Progress) (map[string]any, error) {
		c, _, err := m.hosts.Client(ctx, rec.HostID)
		if err != nil {
			return nil, err
		}
		p.Stage("creating snapshot", 30)
		op, err := c.CreateInstanceSnapshot(ctx, id, api.InstanceSnapshotsPost{Name: req.Name})
		if lxdclient.IsConflict(err) {
			return map[string]any{"snapshot": req.Name, "already_exists": true}, nil
		}
		if err != nil {
			return nil, err
		}
		p.LXD(op.ID(), "")
		if err := op.Wait(ctx, nil); err != nil {
			return nil, err
		}
		return map[string]any{"snapshot": req.Name}, nil
	})
}

// RestoreSnapshot restores a snapshot. Resource limits revert to the values
// captured in the snapshot and the record is updated to match.
func (m *Manager) RestoreSnapshot(ctx context.Context, id, snapshot, actor, idemKey string) (model.Operation, error) {
	rec, err := m.record(ctx, id)
	if err != nil {
		return model.Operation{}, err
	}
	if err := validate.SnapshotName(snapshot); err != nil {
		return model.Operation{}, err
	}
	return m.submit(rec, "container.restore", actor, idemKey, func(ctx context.Context, p *operations.Progress) (map[string]any, error) {
		c, _, err := m.hosts.Client(ctx, rec.HostID)
		if err != nil {
			return nil, err
		}
		snaps, err := c.GetInstanceSnapshots(ctx, id)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(snaps, func(s api.InstanceSnapshot) bool { return s.Name == snapshot }) {
			return nil, errs.NotFound("snapshot_not_found", "snapshot %q not found", snapshot)
		}
		p.Stage("restoring snapshot", 30)
		op, err := c.UpdateInstance(ctx, id, api.InstancePut{Restore: snapshot}, "")
		if err != nil {
			if lxdclient.IsNotFound(err) {
				return nil, errs.NotFound("snapshot_not_found", "snapshot %q not found", snapshot)
			}
			return nil, err
		}
		p.LXD(op.ID(), "")
		if err := op.Wait(ctx, nil); err != nil {
			return nil, err
		}
		if inst, _, err := c.GetInstance(ctx, id); err == nil {
			if r, ok := ResourcesFromInstance(inst); ok {
				_ = m.patch(ctx, id, func(c *model.Container) { c.Resources = r })
			}
		}
		v := m.observe(ctx, c, id)
		return map[string]any{"snapshot": snapshot, "status": v.Status, "resources": v.Resources}, nil
	})
}

// DeleteSnapshot deletes a snapshot.
func (m *Manager) DeleteSnapshot(ctx context.Context, id, snapshot, actor string) (model.Operation, error) {
	rec, err := m.record(ctx, id)
	if err != nil {
		return model.Operation{}, err
	}
	if err := validate.SnapshotName(snapshot); err != nil {
		return model.Operation{}, err
	}
	return m.submit(rec, "container.snapshot_delete", actor, "", func(ctx context.Context, p *operations.Progress) (map[string]any, error) {
		c, _, err := m.hosts.Client(ctx, rec.HostID)
		if err != nil {
			return nil, err
		}
		op, err := c.DeleteInstanceSnapshot(ctx, id, snapshot)
		if lxdclient.IsNotFound(err) {
			return map[string]any{"snapshot": snapshot, "already_deleted": true}, nil
		}
		if err != nil {
			return nil, err
		}
		if err := op.Wait(ctx, nil); err != nil {
			return nil, err
		}
		return map[string]any{"snapshot": snapshot, "deleted": true}, nil
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

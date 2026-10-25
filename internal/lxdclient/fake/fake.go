// Package fake is an in-memory LXD daemon implementing lxdclient.Server. It
// models instance state transitions, snapshots, images and failure injection
// so higher layers can be unit tested without a real LXD host.
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/canonical/lxd/shared/api"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/lxdclient"
)

// ErrUnreachable simulates a host that cannot be contacted.
var ErrUnreachable = errors.New("fake lxd: connection refused")

// Server is a fake LXD daemon. Exported fields may be changed by tests while
// holding no lock only before the server is shared; use the setters after.
type Server struct {
	mu sync.Mutex

	Name       string
	Version    string
	Clustered  bool
	Members    []string
	Resources  api.Resources
	Pools      map[string]api.ResourcesStoragePool
	Networks   []string
	Profiles   []string
	instances  map[string]*instance
	images     map[string]api.Image
	aliases    map[string]string
	down       bool
	failNext   map[string]error
	opDelay    time.Duration
	blockOps   chan struct{}
	opCounter  int
	Calls      []string
	nextIPByte int
}

type instance struct {
	inst      api.Instance
	snapshots map[string]api.InstanceSnapshot
	snapState map[string]api.InstancePut
}

// New returns a fake host with 8 CPU threads, 32 GiB RAM, a 500 GiB
// "default" pool, an lxdbr0 bridge and an ubuntu/24.04 image.
func New(name string) *Server {
	s := &Server{
		Name:    name,
		Version: "6.5",
		Resources: api.Resources{
			CPU:    api.ResourcesCPU{Architecture: "x86_64", Total: 8},
			Memory: api.ResourcesMemory{Total: 32 << 30, Used: 2 << 30},
		},
		Pools:      map[string]api.ResourcesStoragePool{"default": {Space: api.ResourcesStoragePoolSpace{Total: 500 << 30, Used: 10 << 30}}},
		Networks:   []string{"lxdbr0"},
		Profiles:   []string{"default"},
		instances:  map[string]*instance{},
		images:     map[string]api.Image{},
		aliases:    map[string]string{},
		failNext:   map[string]error{},
		nextIPByte: 10,
	}
	s.AddImage(api.Image{
		Fingerprint:  strings.Repeat("ab", 32),
		Architecture: "x86_64",
		Type:         "container",
		Size:         400 << 20,
		Properties:   map[string]string{"os": "ubuntu", "release": "noble", "version": "24.04"},
		Aliases:      []api.ImageAlias{{Name: "ubuntu/24.04"}},
	})
	return s
}

// AddNvidiaGPU adds an NVIDIA card to the host resources.
func (s *Server) AddNvidiaGPU(model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.Resources.GPU.Cards)
	s.Resources.GPU.Cards = append(s.Resources.GPU.Cards, api.ResourcesGPUCard{
		Driver: "nvidia", DriverVersion: "550.54", Vendor: "NVIDIA Corporation", VendorID: "10de",
		Product: model, ProductID: "2204", PCIAddress: fmt.Sprintf("0000:0%d:00.0", n+1),
		DRM:    &api.ResourcesGPUCardDRM{ID: uint64(n), CardName: fmt.Sprintf("card%d", n)},
		Nvidia: &api.ResourcesGPUCardNvidia{Model: model, Brand: "GeForce", UUID: fmt.Sprintf("GPU-fake-%d", n), CUDAVersion: "12.4", NVRMVersion: "550.54"},
	})
	s.Resources.GPU.Total = uint64(len(s.Resources.GPU.Cards))
}

// AddImage registers an image (and its aliases).
func (s *Server) AddImage(img api.Image) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.images[img.Fingerprint] = img
	for _, a := range img.Aliases {
		s.aliases[a.Name] = img.Fingerprint
	}
}

// SetDown makes every call fail as if the host were unreachable.
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// FailNext makes the next call to method return err.
func (s *Server) FailNext(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[method] = err
}

// SetOpDelay makes operations take d to complete.
func (s *Server) SetOpDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opDelay = d
}

// BlockOps makes operations hang until UnblockOps is called.
func (s *Server) BlockOps() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockOps = make(chan struct{})
}

// UnblockOps releases blocked operations.
func (s *Server) UnblockOps() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blockOps != nil {
		close(s.blockOps)
		s.blockOps = nil
	}
}

// InjectInstance adds an instance directly, bypassing the API (e.g. to
// simulate something created outside Type3).
func (s *Server) InjectInstance(inst api.Instance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst.Status == "" {
		inst.Status = "Stopped"
		inst.StatusCode = api.Stopped
	}
	s.instances[inst.Name] = &instance{inst: inst, snapshots: map[string]api.InstanceSnapshot{}, snapState: map[string]api.InstancePut{}}
}

// RemoveInstance deletes an instance directly, bypassing the API.
func (s *Server) RemoveInstance(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.instances, name)
}

// Instance returns a copy of an instance.
func (s *Server) Instance(name string) (api.Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instances[name]
	if !ok {
		return api.Instance{}, false
	}
	return cloneInstance(in.inst), true
}

// InstanceCount returns the number of instances.
func (s *Server) InstanceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.instances)
}

// CallCount counts calls to method.
func (s *Server) CallCount(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.Calls {
		if c == method {
			n++
		}
	}
	return n
}

func (s *Server) enter(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, method)
	if s.down {
		return ErrUnreachable
	}
	if err, ok := s.failNext[method]; ok {
		delete(s.failNext, method)
		return err
	}
	return nil
}

func notFound(what string) error {
	return api.StatusErrorf(http.StatusNotFound, "%s not found", what)
}

func cloneInstance(in api.Instance) api.Instance {
	in.Config = maps.Clone(in.Config)
	in.Devices = cloneDevices(in.Devices)
	in.Profiles = slices.Clone(in.Profiles)
	return in
}

func cloneDevices(d map[string]map[string]string) map[string]map[string]string {
	if d == nil {
		return nil
	}
	out := make(map[string]map[string]string, len(d))
	for k, v := range d {
		out[k] = maps.Clone(v)
	}
	return out
}

// newOp runs apply after the configured delay (or block) and reports it as
// an LXD operation.
func (s *Server) newOp(apply func() error) lxdclient.Op {
	s.mu.Lock()
	s.opCounter++
	o := &fakeOp{id: fmt.Sprintf("op-%d", s.opCounter), done: make(chan struct{})}
	delay, block := s.opDelay, s.blockOps
	s.mu.Unlock()

	go func() {
		if block != nil {
			<-block
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		o.mu.Lock()
		cancelled := o.cancelled
		o.mu.Unlock()
		var err error
		if cancelled {
			err = errors.New("operation cancelled")
		} else {
			err = apply()
		}
		o.mu.Lock()
		o.err = err
		o.mu.Unlock()
		close(o.done)
	}()
	return o
}

type fakeOp struct {
	id        string
	done      chan struct{}
	mu        sync.Mutex
	err       error
	cancelled bool
}

func (o *fakeOp) ID() string { return o.id }

func (o *fakeOp) Cancel() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cancelled = true
	return nil
}

func (o *fakeOp) Wait(ctx context.Context, progress func(api.Operation)) error {
	if progress != nil {
		progress(api.Operation{ID: o.id, Status: "Running", StatusCode: api.Running,
			Metadata: map[string]any{"download_progress": "rootfs: 50% (10MB/s)"}})
	}
	select {
	case <-o.done:
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return lxdclient.ErrOperationTimeout
		}
		return ctx.Err()
	}
}

// Connector serves fake servers by endpoint.
type Connector struct {
	mu      sync.Mutex
	servers map[string]*Server
	// Connects counts successful Connect calls per endpoint.
	Connects map[string]int
}

// NewConnector returns an empty Connector.
func NewConnector() *Connector {
	return &Connector{servers: map[string]*Server{}, Connects: map[string]int{}}
}

// Add serves s at endpoint.
func (c *Connector) Add(endpoint string, s *Server) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.servers[endpoint] = s
}

// ConnectCount returns how many successful connections endpoint received.
func (c *Connector) ConnectCount(endpoint string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Connects[endpoint]
}

// Connect implements lxdclient.Connector.
func (c *Connector) Connect(_ context.Context, t lxdclient.Target) (lxdclient.Server, error) {
	c.mu.Lock()
	s, ok := c.servers[t.Endpoint]
	c.mu.Unlock()
	if !ok {
		return nil, ErrUnreachable
	}
	if err := s.enter("Connect"); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.Connects[t.Endpoint]++
	c.mu.Unlock()
	return s, nil
}

// --- lxdclient.Server implementation ---

func (s *Server) GetServer(_ context.Context) (*api.Server, error) {
	if err := s.enter("GetServer"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return &api.Server{Environment: api.ServerEnvironment{
		ServerName: s.Name, ServerVersion: s.Version, ServerClustered: s.Clustered,
		Kernel: "Linux", KernelVersion: "6.8.0", OSName: "Ubuntu", OSVersion: "24.04", Driver: "lxc",
	}}, nil
}

func (s *Server) GetServerResources(_ context.Context) (*api.Resources, error) {
	if err := s.enter("GetServerResources"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.Resources
	r.GPU.Cards = slices.Clone(r.GPU.Cards)
	return &r, nil
}

func (s *Server) GetStoragePools(_ context.Context) ([]api.StoragePool, error) {
	if err := s.enter("GetStoragePools"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []api.StoragePool
	for name := range s.Pools {
		out = append(out, api.StoragePool{Name: name, Driver: "zfs", Status: "Created"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) GetStoragePoolResources(_ context.Context, pool string) (*api.ResourcesStoragePool, error) {
	if err := s.enter("GetStoragePoolResources"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.Pools[pool]
	if !ok {
		return nil, notFound("storage pool")
	}
	return &r, nil
}

func (s *Server) GetNetworks(_ context.Context) ([]api.Network, error) {
	if err := s.enter("GetNetworks"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []api.Network
	for _, n := range s.Networks {
		out = append(out, api.Network{Name: n, Type: "bridge", Managed: true, Status: "Created"})
	}
	return out, nil
}

func (s *Server) GetProfileNames(_ context.Context) ([]string, error) {
	if err := s.enter("GetProfileNames"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.Profiles), nil
}

func (s *Server) GetClusterMembers(_ context.Context) ([]api.ClusterMember, error) {
	if err := s.enter("GetClusterMembers"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []api.ClusterMember
	for _, m := range s.Members {
		out = append(out, api.ClusterMember{ServerName: m, Status: "Online"})
	}
	return out, nil
}

func (s *Server) GetInstances(_ context.Context) ([]api.Instance, error) {
	if err := s.enter("GetInstances"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []api.Instance{}
	for _, in := range s.instances {
		out = append(out, cloneInstance(in.inst))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) GetInstance(_ context.Context, name string) (*api.Instance, string, error) {
	if err := s.enter("GetInstance"); err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instances[name]
	if !ok {
		return nil, "", notFound("instance")
	}
	c := cloneInstance(in.inst)
	return &c, "etag-" + name, nil
}

func (s *Server) GetInstanceState(_ context.Context, name string) (*api.InstanceState, error) {
	if err := s.enter("GetInstanceState"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instances[name]
	if !ok {
		return nil, notFound("instance")
	}
	st := &api.InstanceState{Status: in.inst.Status, StatusCode: in.inst.StatusCode, Network: map[string]api.InstanceStateNetwork{}}
	if in.inst.StatusCode == api.Running {
		st.Network["lo"] = api.InstanceStateNetwork{Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Address: "127.0.0.1", Scope: "local"}}}
		ip4 := in.inst.Config["volatile.fake.ipv4"]
		st.Network["eth0"] = api.InstanceStateNetwork{Addresses: []api.InstanceStateNetworkAddress{
			{Family: "inet", Address: ip4, Scope: "global"},
			{Family: "inet6", Address: "fd42::" + strings.TrimPrefix(ip4, "10.0.0."), Scope: "global"},
			{Family: "inet6", Address: "fe80::1", Scope: "link"},
		}}
	}
	return st, nil
}

func (s *Server) CreateInstance(_ context.Context, req api.InstancesPost) (lxdclient.Op, error) {
	if err := s.enter("CreateInstance"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if _, exists := s.instances[req.Name]; exists {
		s.mu.Unlock()
		return nil, api.StatusErrorf(http.StatusConflict, "Instance %q already exists", req.Name)
	}
	if req.Type != "" && req.Type != api.InstanceTypeContainer {
		s.mu.Unlock()
		return nil, api.StatusErrorf(http.StatusBadRequest, "fake only supports containers")
	}
	if req.Source.Server == "" {
		fp := req.Source.Fingerprint
		if fp == "" {
			fp = s.aliases[req.Source.Alias]
		}
		if _, ok := s.images[fp]; !ok {
			s.mu.Unlock()
			return nil, api.StatusErrorf(http.StatusNotFound, "Image not found")
		}
	}
	s.mu.Unlock()

	return s.newOp(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, exists := s.instances[req.Name]; exists {
			return fmt.Errorf("instance %q already exists", req.Name)
		}
		cfg := maps.Clone(req.Config)
		if cfg == nil {
			cfg = map[string]string{}
		}
		s.nextIPByte++
		cfg["volatile.fake.ipv4"] = fmt.Sprintf("10.0.0.%d", s.nextIPByte)
		inst := api.Instance{
			Name: req.Name, Type: string(api.InstanceTypeContainer), Status: "Stopped", StatusCode: api.Stopped,
			Architecture: "x86_64", Ephemeral: req.Ephemeral, Profiles: slices.Clone(req.Profiles),
			Config: cfg, Devices: cloneDevices(req.Devices), CreatedAt: time.Now().UTC(), Location: "none",
		}
		if req.Start {
			inst.Status, inst.StatusCode = "Running", api.Running
		}
		s.instances[req.Name] = &instance{inst: inst, snapshots: map[string]api.InstanceSnapshot{}, snapState: map[string]api.InstancePut{}}
		return nil
	}), nil
}

func (s *Server) UpdateInstance(_ context.Context, name string, put api.InstancePut, _ string) (lxdclient.Op, error) {
	if err := s.enter("UpdateInstance"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	_, ok := s.instances[name]
	s.mu.Unlock()
	if !ok {
		return nil, notFound("instance")
	}
	return s.newOp(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		in, ok := s.instances[name]
		if !ok {
			return errors.New("instance vanished")
		}
		if put.Restore != "" {
			saved, ok := in.snapState[put.Restore]
			if !ok {
				return fmt.Errorf("snapshot %q not found", put.Restore)
			}
			ip := in.inst.Config["volatile.fake.ipv4"]
			in.inst.Config = maps.Clone(saved.Config)
			in.inst.Config["volatile.fake.ipv4"] = ip
			in.inst.Devices = cloneDevices(saved.Devices)
			return nil
		}
		ip := in.inst.Config["volatile.fake.ipv4"]
		in.inst.Config = maps.Clone(put.Config)
		if in.inst.Config == nil {
			in.inst.Config = map[string]string{}
		}
		in.inst.Config["volatile.fake.ipv4"] = ip
		in.inst.Devices = cloneDevices(put.Devices)
		in.inst.Profiles = slices.Clone(put.Profiles)
		return nil
	}), nil
}

func (s *Server) UpdateInstanceState(_ context.Context, name string, put api.InstanceStatePut) (lxdclient.Op, error) {
	if err := s.enter("UpdateInstanceState"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	_, ok := s.instances[name]
	s.mu.Unlock()
	if !ok {
		return nil, notFound("instance")
	}
	return s.newOp(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		in, ok := s.instances[name]
		if !ok {
			return errors.New("instance vanished")
		}
		switch put.Action {
		case "start":
			if in.inst.StatusCode == api.Running {
				return errors.New("The instance is already running")
			}
			in.inst.Status, in.inst.StatusCode = "Running", api.Running
		case "stop":
			if in.inst.StatusCode != api.Running && in.inst.StatusCode != api.Frozen {
				return errors.New("The instance is already stopped")
			}
			if in.inst.Config["volatile.fake.ignore_sigpwr"] == "true" && !put.Force {
				return errors.New("Failed shutting down instance: timeout")
			}
			in.inst.Status, in.inst.StatusCode = "Stopped", api.Stopped
			if in.inst.Ephemeral {
				delete(s.instances, name)
			}
		case "restart":
			if in.inst.StatusCode != api.Running {
				return errors.New("The instance isn't running")
			}
		case "freeze":
			in.inst.Status, in.inst.StatusCode = "Frozen", api.Frozen
		case "unfreeze":
			in.inst.Status, in.inst.StatusCode = "Running", api.Running
		default:
			return fmt.Errorf("unknown action %q", put.Action)
		}
		return nil
	}), nil
}

func (s *Server) DeleteInstance(_ context.Context, name string) (lxdclient.Op, error) {
	if err := s.enter("DeleteInstance"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	in, ok := s.instances[name]
	running := ok && in.inst.StatusCode == api.Running
	s.mu.Unlock()
	if !ok {
		return nil, notFound("instance")
	}
	if running {
		return nil, api.StatusErrorf(http.StatusBadRequest, "Instance is running")
	}
	return s.newOp(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.instances, name)
		return nil
	}), nil
}

func (s *Server) GetInstanceSnapshots(_ context.Context, name string) ([]api.InstanceSnapshot, error) {
	if err := s.enter("GetInstanceSnapshots"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instances[name]
	if !ok {
		return nil, notFound("instance")
	}
	out := []api.InstanceSnapshot{}
	for _, sn := range in.snapshots {
		out = append(out, sn)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) CreateInstanceSnapshot(_ context.Context, name string, req api.InstanceSnapshotsPost) (lxdclient.Op, error) {
	if err := s.enter("CreateInstanceSnapshot"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	in, ok := s.instances[name]
	if ok {
		if _, dup := in.snapshots[req.Name]; dup {
			s.mu.Unlock()
			return nil, api.StatusErrorf(http.StatusConflict, "Snapshot %q already exists", req.Name)
		}
	}
	s.mu.Unlock()
	if !ok {
		return nil, notFound("instance")
	}
	return s.newOp(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		in := s.instances[name]
		in.snapshots[req.Name] = api.InstanceSnapshot{Name: req.Name, CreatedAt: time.Now().UTC(), Stateful: req.Stateful}
		in.snapState[req.Name] = api.InstancePut{Config: maps.Clone(in.inst.Config), Devices: cloneDevices(in.inst.Devices)}
		return nil
	}), nil
}

func (s *Server) DeleteInstanceSnapshot(_ context.Context, name, snapshot string) (lxdclient.Op, error) {
	if err := s.enter("DeleteInstanceSnapshot"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	in, ok := s.instances[name]
	exists := ok && in.snapshots[snapshot].Name != ""
	s.mu.Unlock()
	if !exists {
		return nil, notFound("snapshot")
	}
	return s.newOp(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.instances[name].snapshots, snapshot)
		delete(s.instances[name].snapState, snapshot)
		return nil
	}), nil
}

func (s *Server) GetImages(_ context.Context) ([]api.Image, error) {
	if err := s.enter("GetImages"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []api.Image{}
	for _, img := range s.images {
		out = append(out, img)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	return out, nil
}

func (s *Server) GetImage(_ context.Context, fingerprint string) (*api.Image, error) {
	if err := s.enter("GetImage"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var matches []api.Image
	for fp, img := range s.images {
		if strings.HasPrefix(fp, fingerprint) {
			matches = append(matches, img)
		}
	}
	if len(matches) != 1 {
		return nil, notFound("image")
	}
	return &matches[0], nil
}

func (s *Server) GetImageAlias(_ context.Context, name string) (*api.ImageAliasesEntry, error) {
	if err := s.enter("GetImageAlias"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fp, ok := s.aliases[name]
	if !ok {
		return nil, notFound("alias")
	}
	return &api.ImageAliasesEntry{Name: name, Target: fp, Type: "container"}, nil
}

// RemoteFingerprint is the fingerprint the fake assigns to an image imported
// from server+alias.
func RemoteFingerprint(server, alias string) string {
	sum := sha256.Sum256([]byte(server + "|" + alias))
	return hex.EncodeToString(sum[:])
}

func (s *Server) CreateImage(_ context.Context, req api.ImagesPost) (lxdclient.Op, error) {
	if err := s.enter("CreateImage"); err != nil {
		return nil, err
	}
	if req.Source == nil {
		return nil, api.StatusErrorf(http.StatusBadRequest, "upload not supported by fake")
	}
	return s.newOp(func() error {
		var fp string
		switch req.Source.Type {
		case api.SourceTypeImage:
			if req.Source.Fingerprint != "" {
				fp = req.Source.Fingerprint
			} else {
				fp = RemoteFingerprint(req.Source.Server, req.Source.Alias)
			}
		case "url":
			fp = RemoteFingerprint(req.Source.URL, "")
		default:
			return fmt.Errorf("unsupported source type %q", req.Source.Type)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		img := s.images[fp]
		img.Fingerprint = fp
		img.Architecture = "x86_64"
		img.Type = "container"
		img.Size = 380 << 20
		img.Properties = map[string]string{"os": "ubuntu", "release": "noble"}
		for _, a := range req.Aliases {
			img.Aliases = append(img.Aliases, a)
			s.aliases[a.Name] = fp
		}
		s.images[fp] = img
		return nil
	}), nil
}

func (s *Server) DeleteImage(_ context.Context, fingerprint string) (lxdclient.Op, error) {
	if err := s.enter("DeleteImage"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	_, ok := s.images[fingerprint]
	s.mu.Unlock()
	if !ok {
		return nil, notFound("image")
	}
	return s.newOp(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		for alias, fp := range s.aliases {
			if fp == fingerprint {
				delete(s.aliases, alias)
			}
		}
		delete(s.images, fingerprint)
		return nil
	}), nil
}

func (s *Server) Disconnect() {}

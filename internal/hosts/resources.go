package hosts

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/gpu"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
)

// CPUInfo summarises host CPUs.
type CPUInfo struct {
	Architecture string   `json:"architecture"`
	Threads      uint64   `json:"threads"`
	Sockets      int      `json:"sockets"`
	Models       []string `json:"models"`
}

// MemoryInfo summarises host memory.
type MemoryInfo struct {
	TotalBytes uint64 `json:"total_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
}

// PoolInfo describes a storage pool.
type PoolInfo struct {
	Name       string `json:"name"`
	Driver     string `json:"driver"`
	Status     string `json:"status"`
	TotalBytes uint64 `json:"total_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
}

// NetworkInfo describes an LXD network.
type NetworkInfo struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Managed bool   `json:"managed"`
	Status  string `json:"status"`
}

// Allocation is what Type3-managed containers have reserved on a host.
type Allocation struct {
	Containers    int `json:"containers"`
	CPU           int `json:"cpu"`
	MemoryMiB     int `json:"memory_mib"`
	DiskGiB       int `json:"disk_gib"`
	GPUContainers int `json:"gpu_containers"`
}

// Allocatable is the capacity available to Type3 after overcommit ratios.
type Allocatable struct {
	CPU           float64 `json:"cpu"`
	MemoryMiB     int     `json:"memory_mib"`
	GPUContainers int     `json:"gpu_containers"`
}

// HostResources is the full resource inventory of a host.
type HostResources struct {
	HostID      string        `json:"host_id"`
	CPU         CPUInfo       `json:"cpu"`
	Memory      MemoryInfo    `json:"memory"`
	Storage     []PoolInfo    `json:"storage"`
	Networks    []NetworkInfo `json:"networks"`
	GPU         gpu.Inventory `json:"gpu"`
	GPUCapable  bool          `json:"gpu_capable"`
	Allocated   Allocation    `json:"allocated"`
	Allocatable Allocatable   `json:"allocatable"`
	CollectedAt time.Time     `json:"collected_at"`
}

// Resources collects CPU, memory, storage, network and GPU inventory live
// from the host and refreshes the cached capacity used for placement.
func (r *Registry) Resources(ctx context.Context, id string) (HostResources, error) {
	c, h, err := r.Client(ctx, id)
	if err != nil {
		return HostResources{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.opt.RequestTimeout)
	defer cancel()
	res, err := c.GetServerResources(ctx)
	if err != nil {
		r.recordFailure(ctx, id, err)
		return HostResources{}, errs.Wrap(err, errs.KindUnavailable, "host_unavailable", "read resources of host %q", id)
	}
	out := HostResources{HostID: id, CollectedAt: r.now()}
	out.CPU = CPUInfo{Architecture: res.CPU.Architecture, Threads: res.CPU.Total, Sockets: len(res.CPU.Sockets), Models: []string{}}
	for _, s := range res.CPU.Sockets {
		if s.Name != "" {
			out.CPU.Models = append(out.CPU.Models, s.Name)
		}
	}
	out.Memory = MemoryInfo{TotalBytes: res.Memory.Total, UsedBytes: res.Memory.Used}
	out.GPU = gpu.FromResources(res)
	out.GPUCapable = out.GPU.GPUCapable()

	if out.Storage, err = r.StoragePools(ctx, id); err != nil {
		return HostResources{}, err
	}
	if out.Networks, err = r.Networks(ctx, id); err != nil {
		return HostResources{}, err
	}

	alloc, err := r.Allocation(ctx, id)
	if err != nil {
		return HostResources{}, err
	}
	out.Allocated = alloc

	nv := out.GPU.Nvidia
	if !out.GPU.NvidiaDriverReady {
		nv = 0
	}
	capacity := model.HostCapacity{CPUThreads: res.CPU.Total, MemoryBytes: res.Memory.Total, GPUs: out.GPU.Total, NvidiaGPUs: nv, DiscoveredAt: out.CollectedAt}
	out.Allocatable = r.allocatable(capacity)
	_ = r.updateHost(ctx, h.ID, func(hh *model.Host) { hh.Capacity = capacity })
	return out, nil
}

func (r *Registry) allocatable(c model.HostCapacity) Allocatable {
	return Allocatable{
		CPU:           float64(c.CPUThreads) * r.opt.CPUOvercommit,
		MemoryMiB:     int(math.Floor(float64(c.MemoryBytes>>20) * r.opt.MemoryOvercommit)),
		GPUContainers: c.NvidiaGPUs * r.opt.MaxContainersPerGPU,
	}
}

// StoragePools lists pools with usage.
func (r *Registry) StoragePools(ctx context.Context, id string) ([]PoolInfo, error) {
	c, _, err := r.Client(ctx, id)
	if err != nil {
		return nil, err
	}
	pools, err := c.GetStoragePools(ctx)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindUnavailable, "host_unavailable", "list storage pools of host %q", id)
	}
	out := make([]PoolInfo, 0, len(pools))
	for _, p := range pools {
		pi := PoolInfo{Name: p.Name, Driver: p.Driver, Status: p.Status}
		if pr, err := c.GetStoragePoolResources(ctx, p.Name); err == nil {
			pi.TotalBytes, pi.UsedBytes = pr.Space.Total, pr.Space.Used
		}
		out = append(out, pi)
	}
	return out, nil
}

// Networks lists networks.
func (r *Registry) Networks(ctx context.Context, id string) ([]NetworkInfo, error) {
	c, _, err := r.Client(ctx, id)
	if err != nil {
		return nil, err
	}
	nets, err := c.GetNetworks(ctx)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindUnavailable, "host_unavailable", "list networks of host %q", id)
	}
	out := make([]NetworkInfo, 0, len(nets))
	for _, n := range nets {
		out = append(out, NetworkInfo{Name: n.Name, Type: n.Type, Managed: n.Managed, Status: n.Status})
	}
	return out, nil
}

// Allocation sums the resources reserved by Type3 containers on a host.
func (r *Registry) Allocation(ctx context.Context, id string) (Allocation, error) {
	cs, err := r.st.ListContainers(ctx, store.ContainerFilter{HostID: id})
	if err != nil {
		return Allocation{}, err
	}
	var a Allocation
	for _, c := range cs {
		if c.Status == model.ContainerMissing {
			continue
		}
		a.Containers++
		a.CPU += c.Resources.CPU
		a.MemoryMiB += c.Resources.MemoryMiB
		a.DiskGiB += c.Resources.DiskGiB
		if c.GPU {
			a.GPUContainers++
		}
	}
	return a, nil
}

// PlacementRequest describes what a new container needs.
type PlacementRequest struct {
	Resources model.Resources
	GPU       bool
}

// CheckPlacement verifies an explicitly chosen host can take the container.
func (r *Registry) CheckPlacement(ctx context.Context, hostID string, req PlacementRequest) (model.Host, error) {
	h, err := r.st.GetHost(ctx, hostID)
	if err != nil {
		return h, err
	}
	if h.Status == model.HostOffline {
		return h, errs.Unavailable("host_unavailable", "host %q is offline", hostID)
	}
	if err := r.fits(ctx, h, req); err != nil {
		return h, err
	}
	return h, nil
}

func (r *Registry) fits(ctx context.Context, h model.Host, req PlacementRequest) error {
	if req.GPU && !h.GPUCapable() {
		return errs.New(errs.KindPrecondition, "gpu_unavailable", "host %q has no usable NVIDIA GPU", h.ID)
	}
	if h.Capacity.DiscoveredAt.IsZero() {
		// Capacity unknown (host registered offline): allow explicit
		// placement, the LXD call will fail if it really does not fit.
		return nil
	}
	alloc, err := r.Allocation(ctx, h.ID)
	if err != nil {
		return err
	}
	limit := r.allocatable(h.Capacity)
	if req.GPU && alloc.GPUContainers+1 > limit.GPUContainers {
		return errs.Conflict("insufficient_gpu_capacity", "host %q already shares its GPUs with %d containers", h.ID, alloc.GPUContainers)
	}
	if float64(alloc.CPU+req.Resources.CPU) > limit.CPU {
		return errs.Conflict("insufficient_cpu", "host %q has no CPU capacity left", h.ID).
			WithDetails(map[string]any{"allocated": alloc.CPU, "allocatable": limit.CPU})
	}
	if alloc.MemoryMiB+req.Resources.MemoryMiB > limit.MemoryMiB {
		return errs.Conflict("insufficient_memory", "host %q has no memory capacity left", h.ID).
			WithDetails(map[string]any{"allocated_mib": alloc.MemoryMiB, "allocatable_mib": limit.MemoryMiB})
	}
	return nil
}

// Select picks a host automatically: online, schedulable hosts that fit,
// preferring CPU-only hosts for non-GPU work (keeping GPU hosts free) and
// then the most free memory. Sophisticated scheduling is Milestone 6.
func (r *Registry) Select(ctx context.Context, req PlacementRequest) (model.Host, error) {
	hs, err := r.st.ListHosts(ctx)
	if err != nil {
		return model.Host{}, err
	}
	type cand struct {
		h       model.Host
		freeMem int
	}
	var cands []cand
	for _, h := range hs {
		if !h.Schedulable || h.Status != model.HostOnline || h.Capacity.DiscoveredAt.IsZero() {
			continue
		}
		if err := r.fits(ctx, h, req); err != nil {
			continue
		}
		alloc, _ := r.Allocation(ctx, h.ID)
		cands = append(cands, cand{h: h, freeMem: r.allocatable(h.Capacity).MemoryMiB - alloc.MemoryMiB})
	}
	if len(cands) == 0 {
		return model.Host{}, errs.Conflict("no_host_available", "no online host has capacity for this container").
			WithDetails(map[string]any{"gpu": req.GPU, "cpu": req.Resources.CPU, "memory_mib": req.Resources.MemoryMiB})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if !req.GPU && a.h.GPUCapable() != b.h.GPUCapable() {
			return !a.h.GPUCapable()
		}
		if a.freeMem != b.freeMem {
			return a.freeMem > b.freeMem
		}
		return a.h.ID < b.h.ID
	})
	return cands[0].h, nil
}

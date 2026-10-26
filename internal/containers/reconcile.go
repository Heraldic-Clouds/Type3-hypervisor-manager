package containers

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/canonical/lxd/shared/api"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/gpu"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/profiles"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/validate"
)

// ReconcileReport summarises one reconciliation pass over a host.
type ReconcileReport struct {
	HostID    string   `json:"host_id"`
	Checked   int      `json:"checked"`
	Updated   []string `json:"updated"`
	Missing   []string `json:"missing"`
	Removed   []string `json:"removed"`
	Adopted   []string `json:"adopted"`
	Conflicts []string `json:"conflicts"`
	Skipped   []string `json:"skipped_busy"`
}

// Reconcile compares Type3's records for a host with the instances LXD
// actually has, and repairs the records:
//
//   - a record whose instance disappeared is marked missing (or dropped if
//     it was being deleted or was ephemeral);
//   - a record's status and addresses are refreshed from LXD;
//   - an instance tagged user.mea.managed=true with no record (for example
//     after a crash between LXD accepting a create and Type3 saving) is
//     adopted as an orphaned record.
//
// Containers with an operation in flight are left alone.
func (m *Manager) Reconcile(ctx context.Context, hostID string) (ReconcileReport, error) {
	rep := ReconcileReport{HostID: hostID, Updated: []string{}, Missing: []string{}, Removed: []string{},
		Adopted: []string{}, Conflicts: []string{}, Skipped: []string{}}
	c, _, err := m.hosts.Client(ctx, hostID)
	if err != nil {
		return rep, err
	}
	insts, err := c.GetInstances(ctx)
	if err != nil {
		return rep, errs.Wrap(err, errs.KindUnavailable, "host_unavailable", "list instances on host %q", hostID)
	}
	byName := make(map[string]api.Instance, len(insts))
	for _, in := range insts {
		byName[in.Name] = in
	}
	recs, err := m.st.ListContainers(ctx, store.ContainerFilter{HostID: hostID})
	if err != nil {
		return rep, err
	}
	known := map[string]bool{}
	for _, rec := range recs {
		known[rec.ID] = true
		rep.Checked++
		if _, busy := m.ops.Active(resourceType, rec.ID); busy {
			rep.Skipped = append(rep.Skipped, rec.ID)
			continue
		}
		unlock := m.lock(rec.ID)
		inst, present := byName[rec.ID]
		switch {
		case !present && (rec.Status == model.ContainerDeleting || rec.Ephemeral):
			if err := m.st.DeleteContainer(ctx, rec.ID); err == nil {
				rep.Removed = append(rep.Removed, rec.ID)
			}
		case !present && (rec.Status == model.ContainerPending || rec.Status == model.ContainerCreating):
			// The create was interrupted before LXD made the instance.
			m.setStatus(ctx, rec.ID, model.ContainerError, "creation interrupted before the instance existed; delete and recreate")
			rep.Missing = append(rep.Missing, rec.ID)
		case !present:
			if rec.Status != model.ContainerMissing {
				m.setStatus(ctx, rec.ID, model.ContainerMissing, "instance not found in LXD during reconciliation")
				rep.Missing = append(rep.Missing, rec.ID)
			}
		default:
			before := rec.Status
			v := m.observe(ctx, c, rec.ID)
			if before == model.ContainerPending || before == model.ContainerCreating {
				// The create finished in LXD but Type3 never recorded it.
				if r, ok := ResourcesFromInstance(&inst); ok {
					_ = m.patch(ctx, rec.ID, func(cc *model.Container) { cc.Resources = r })
				}
			}
			if v.Status != before {
				rep.Updated = append(rep.Updated, rec.ID)
			}
		}
		unlock()
	}

	for _, in := range insts {
		if known[in.Name] || in.Config[profiles.KeyManaged] != "true" {
			continue
		}
		if other, err := m.st.GetContainer(ctx, in.Name); err == nil {
			// Same name managed on another host: never merge silently.
			m.log.Warn("managed instance name collides with a record on another host",
				"container", in.Name, "host", hostID, "recorded_host", other.HostID)
			rep.Conflicts = append(rep.Conflicts, in.Name)
			continue
		}
		if validate.Name("name", in.Name) != nil {
			continue
		}
		rec := adoptRecord(hostID, in, m.now())
		if err := m.st.PutContainer(ctx, rec); err != nil {
			return rep, err
		}
		m.observe(ctx, c, rec.ID)
		m.log.Warn("adopted orphaned managed instance", "container", rec.ID, "host", hostID)
		rep.Adopted = append(rep.Adopted, rec.ID)
	}
	return rep, nil
}

func adoptRecord(hostID string, in api.Instance, now time.Time) model.Container {
	rec := model.Container{
		ID: in.Name, HostID: hostID, OwnerID: in.Config[profiles.KeyOwner], Tier: in.Config[profiles.KeyTier],
		SpecHash: in.Config[profiles.KeySpecHash], Ephemeral: in.Ephemeral, Profiles: in.Profiles,
		Status: MapStatus(in.Status), LXDStatus: in.Status, Orphaned: true, CreatedAt: now, UpdatedAt: now,
	}
	if r, ok := ResourcesFromInstance(&in); ok {
		rec.Resources = r
	}
	if nic, ok := in.Devices[profiles.NICDevice]; ok {
		rec.Network = nic["network"]
	}
	if root, ok := in.Devices[profiles.RootDevice]; ok {
		rec.StoragePool = root["pool"]
	}
	for _, d := range in.Devices {
		if gpu.IsGPUDevice(d) {
			rec.GPU = true
		}
	}
	return rec
}

// ReconcileAll reconciles every host that is not offline.
func (m *Manager) ReconcileAll(ctx context.Context) []ReconcileReport {
	hs, err := m.hosts.List(ctx)
	if err != nil {
		m.log.Error("reconcile: list hosts", "err", err)
		return nil
	}
	var out []ReconcileReport
	for _, h := range hs {
		if h.Status == model.HostOffline {
			continue
		}
		rep, err := m.Reconcile(ctx, h.ID)
		if err != nil {
			m.log.Warn("reconcile host", "host", h.ID, "err", err)
			continue
		}
		if len(rep.Updated)+len(rep.Missing)+len(rep.Removed)+len(rep.Adopted)+len(rep.Conflicts) > 0 {
			m.log.Info("reconciled host", "host", h.ID, "updated", rep.Updated, "missing", rep.Missing,
				"removed", rep.Removed, "adopted", rep.Adopted, "conflicts", rep.Conflicts)
		}
		out = append(out, rep)
	}
	return out
}

// Run reconciles all hosts every interval until ctx ends.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.ReconcileAll(ctx)
		}
	}
}

// ResourcesFromInstance reads CPU, memory and disk limits back from LXD config.
func ResourcesFromInstance(in *api.Instance) (model.Resources, bool) {
	var r model.Resources
	cpu, err := strconv.Atoi(in.Config[profiles.KeyCPU])
	if err != nil {
		return r, false
	}
	mem, ok := ParseMiB(in.Config[profiles.KeyMemory])
	if !ok {
		return r, false
	}
	r.CPU, r.MemoryMiB = cpu, mem
	if root, ok := in.Devices[profiles.RootDevice]; ok {
		if mib, ok := ParseMiB(root["size"]); ok {
			r.DiskGiB = mib / 1024
		}
	}
	return r, true
}

// ParseMiB parses an LXD size ("6144MiB", "6GiB", "512MB", "1073741824") into MiB.
func ParseMiB(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	units := []struct {
		suffix string
		bytes  float64
	}{
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"B", 1},
	}
	mult := 1.0
	num := s
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.bytes
			num = strings.TrimSuffix(s, u.suffix)
			break
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return int(f * mult / (1 << 20)), true
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

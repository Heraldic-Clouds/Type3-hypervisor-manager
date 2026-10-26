package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/gpu"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
)

// setCredentials sets the desktop account password. The body is never
// logged or audited; only the fact that credentials changed is.
func (a *API) setCredentials(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Password string `json:"password"`
	}
	if err := decode(r, &req, false); err != nil {
		writeError(w, r, err)
		return
	}
	err := a.d.Containers.SetPassword(r.Context(), id, req.Password)
	a.audit(r, "container.credentials", "container", id, "", nil, err, nil)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "credentials": "updated"})
}

func (a *API) containerLogs(w http.ResponseWriter, r *http.Request) {
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	out, err := a.d.Containers.Logs(r.Context(), r.PathValue("id"), lines)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "log": out})
}

// containerUsage is one container's live resource use.
type containerUsage struct {
	ID               string `json:"id"`
	OwnerID          string `json:"owner_id"`
	Tier             string `json:"tier"`
	Status           string `json:"status"`
	CPUSeconds       int64  `json:"cpu_seconds"`
	MemoryUsageBytes int64  `json:"memory_usage_bytes"`
	RootUsageBytes   int64  `json:"root_usage_bytes"`
	HomeUsageBytes   int64  `json:"home_usage_bytes,omitempty"`
	Processes        int64  `json:"processes"`
	GPU              bool   `json:"gpu"`
}

// hostTelemetry samples GPU (nvidia-smi), host memory and per-container use.
func (a *API) hostTelemetry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	body := map[string]any{"host_id": id, "sampled_at": time.Now().UTC()}
	if res, err := a.d.Hosts.Resources(ctx, id); err == nil {
		body["resources"] = res
	} else {
		body["resources_error"] = errMessage(err)
	}
	if a.d.Agent != nil {
		if g, err := a.d.Agent.GPUs(ctx); err == nil {
			body["gpus"] = g
		} else {
			body["gpus"] = []gpu.Utilization{}
			body["gpu_error"] = "nvidia-smi unavailable"
		}
	}
	recs, err := a.d.Containers.List(ctx, store.ContainerFilter{HostID: id})
	if err != nil {
		writeError(w, r, err)
		return
	}
	usage := []containerUsage{}
	c, _, cerr := a.d.Hosts.Client(ctx, id)
	homes := r.URL.Query().Get("storage") == "true"
	for _, rec := range recs {
		u := containerUsage{ID: rec.ID, OwnerID: rec.OwnerID, Tier: rec.Tier, Status: string(rec.Status), GPU: rec.GPU}
		if cerr == nil {
			if st, err := c.GetInstanceState(ctx, rec.ID); err == nil {
				u.Status = st.Status
				u.CPUSeconds = st.CPU.Usage / 1e9
				u.MemoryUsageBytes = st.Memory.Usage
				u.Processes = st.Processes
				if d, ok := st.Disk["root"]; ok {
					u.RootUsageBytes = d.Usage
				}
			}
		}
		if homes && rec.PersistentHome && a.d.Agent != nil {
			u.HomeUsageBytes, _ = a.d.Agent.Usage(ctx, rec.OwnerID)
		}
		usage = append(usage, u)
	}
	body["containers"] = usage
	writeJSON(w, http.StatusOK, body)
}

func errMessage(err error) string {
	if e, ok := errs.As(err); ok {
		return e.Message
	}
	return "unavailable"
}

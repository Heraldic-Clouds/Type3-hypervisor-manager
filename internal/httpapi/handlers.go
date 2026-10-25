package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/audit"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/auth"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/containers"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/hosts"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/images"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/operations"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/profiles"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/validate"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/version"
)

// audit records a privileged action. err nil means accepted/succeeded.
func (a *API) audit(r *http.Request, action, resType, resID, reason string, op *model.Operation, err error, details map[string]any) {
	p := principal(r)
	e := audit.Event{
		Actor: p.Name, Role: p.Role.String(), Action: action, ResourceType: resType, ResourceID: resID,
		Outcome: "accepted", RequestID: requestIDFrom(r.Context()), RemoteAddr: remoteIP(r), Reason: reason, Details: details,
	}
	if op != nil {
		e.OperationID = op.ID
	}
	if err != nil {
		e.Outcome = "rejected"
		if ae, ok := errs.As(err); ok {
			e.Error = ae.Code
		} else {
			e.Error = "internal_error"
		}
	}
	a.d.Audit.Record(r.Context(), e)
}

func idemKey(r *http.Request) (string, error) {
	k := r.Header.Get("Idempotency-Key")
	return k, validate.IdempotencyKey(k)
}

func accepted(w http.ResponseWriter, op model.Operation, extra map[string]any) {
	body := map[string]any{"operation": op}
	for k, v := range extra {
		body[k] = v
	}
	w.Header().Set("Location", "/api/v1/operations/"+op.ID)
	writeJSON(w, http.StatusAccepted, body)
}

// --- monitoring ---

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	status, code := "ok", http.StatusOK
	checks := map[string]string{"store": "ok"}
	if err := a.d.Store.Ping(ctx); err != nil {
		status, code = "unhealthy", http.StatusServiceUnavailable
		checks["store"] = "unavailable"
	}
	counts := map[model.HostStatus]int{}
	if hs, err := a.d.Hosts.List(ctx); err == nil {
		for _, h := range hs {
			counts[h.Status]++
		}
	}
	writeJSON(w, code, map[string]any{
		"status": status, "version": version.Version, "checks": checks,
		"hosts": counts, "time": time.Now().UTC(),
	})
}

func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_ = a.d.Metrics.Write(w)
}

func (a *API) listOperations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ops := a.d.Ops.List(r.Context(), operations.Filter{
		ResourceType: q.Get("resource_type"), ResourceID: q.Get("resource_id"), Status: model.OperationStatus(q.Get("status")),
	})
	if len(ops) > 200 {
		ops = ops[:200]
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": ops})
}

func (a *API) getOperation(w http.ResponseWriter, r *http.Request) {
	op, err := a.d.Ops.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (a *API) cancelOperation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	op, err := a.d.Ops.Cancel(r.Context(), id)
	a.audit(r, "operation.cancel", "operation", id, "", &op, err, nil)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op)
}

func (a *API) listProfiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"profiles": a.d.Catalog.List()})
}

// --- hosts ---

func (a *API) registerHost(w http.ResponseWriter, r *http.Request) {
	var req hosts.RegisterRequest
	if err := decode(r, &req, false); err != nil {
		writeError(w, r, err)
		return
	}
	h, err := a.d.Hosts.Register(r.Context(), req)
	a.audit(r, "host.register", "host", req.Name, "", nil, err, map[string]any{"endpoint": req.Endpoint})
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/hosts/"+h.ID)
	writeJSON(w, http.StatusCreated, h)
}

func (a *API) listHosts(w http.ResponseWriter, r *http.Request) {
	hs, err := a.d.Hosts.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hs})
}

func (a *API) getHost(w http.ResponseWriter, r *http.Request) {
	h, err := a.d.Hosts.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (a *API) hostResources(w http.ResponseWriter, r *http.Request) {
	res, err := a.d.Hosts.Resources(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) hostHealth(w http.ResponseWriter, r *http.Request) {
	rep, err := a.d.Hosts.Health(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	code := http.StatusOK
	if rep.Status == model.HostOffline {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, rep)
}

func (a *API) hostContainers(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	recs, err := a.d.Containers.List(r.Context(), store.ContainerFilter{HostID: id})
	if err != nil {
		writeError(w, r, err)
		return
	}
	if _, err := a.d.Hosts.Get(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	body := map[string]any{"host_id": id, "managed": recs}
	c, _, err := a.d.Hosts.Client(r.Context(), id)
	if err != nil {
		body["stale"] = true
		writeJSON(w, http.StatusOK, body)
		return
	}
	insts, err := c.GetInstances(r.Context())
	if err != nil {
		a.d.Hosts.ReportFailure(r.Context(), id, err)
		body["stale"] = true
		writeJSON(w, http.StatusOK, body)
		return
	}
	unmanaged := []map[string]string{}
	for _, in := range insts {
		if in.Config[profiles.KeyManaged] != "true" {
			unmanaged = append(unmanaged, map[string]string{"name": in.Name, "status": in.Status})
		}
	}
	body["unmanaged"] = unmanaged
	writeJSON(w, http.StatusOK, body)
}

func (a *API) reconcileHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rep, err := a.d.Containers.Reconcile(r.Context(), id)
	a.audit(r, "host.reconcile", "host", id, "", nil, err, nil)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (a *API) unregisterHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	force, err := boolQuery(r, "force", false)
	if err != nil {
		writeError(w, r, err)
		return
	}
	n, err := a.d.Hosts.Unregister(r.Context(), id, force)
	a.audit(r, "host.unregister", "host", id, "", nil, err, map[string]any{"force": force, "forgotten_containers": n})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "unregistered": true, "forgotten_containers": n})
}

// --- containers ---

func (a *API) createContainer(w http.ResponseWriter, r *http.Request) {
	var req containers.CreateRequest
	if err := decode(r, &req, false); err != nil {
		writeError(w, r, err)
		return
	}
	key, err := idemKey(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	req.IdempotencyKey = key
	if req.Image != nil {
		// Overriding the tier image is an administrative decision.
		if err := requireRole(r, auth.RoleAdmin, "choosing a non-default image"); err != nil {
			writeError(w, r, err)
			return
		}
	}
	res, err := a.d.Containers.Create(r.Context(), req, principal(r).Name)
	if err != nil {
		a.audit(r, "container.create", "container", req.Name, "", nil, err, map[string]any{"tier": req.Tier, "owner_id": req.OwnerID})
		writeError(w, r, err)
		return
	}
	if !res.Existing {
		a.audit(r, "container.create", "container", req.Name, "", &res.Operation, nil,
			map[string]any{"tier": res.Container.Tier, "owner_id": res.Container.OwnerID, "host_id": res.Container.HostID})
	}
	w.Header().Set("Location", "/api/v1/containers/"+res.Container.ID)
	status := http.StatusAccepted
	if res.Existing {
		status = http.StatusOK
	}
	writeJSON(w, status, res)
}

func (a *API) listContainers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cs, err := a.d.Containers.List(r.Context(), store.ContainerFilter{HostID: q.Get("host_id"), OwnerID: q.Get("owner_id"), Tier: q.Get("tier")})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"containers": cs})
}

func (a *API) getContainer(w http.ResponseWriter, r *http.Request) {
	live, err := boolQuery(r, "live", true)
	if err != nil {
		writeError(w, r, err)
		return
	}
	v, err := a.d.Containers.Get(r.Context(), r.PathValue("id"), live)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) startContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key, err := idemKey(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	op, err := a.d.Containers.Start(r.Context(), id, principal(r).Name, key)
	if err != nil {
		writeError(w, r, err)
		return
	}
	accepted(w, op, nil)
}

func (a *API) stopRequest(r *http.Request, action string) (containers.StopRequest, string, error) {
	var req containers.StopRequest
	if err := decode(r, &req, true); err != nil {
		return req, "", err
	}
	if req.Force {
		if err := requireRole(r, auth.RoleAdmin, "a forced "+action); err != nil {
			return req, "", err
		}
	}
	key, err := idemKey(r)
	return req, key, err
}

func (a *API) stopContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	req, key, err := a.stopRequest(r, "stop")
	if err != nil {
		a.audit(r, "container.stop", "container", id, req.Reason, nil, err, map[string]any{"force": req.Force})
		writeError(w, r, err)
		return
	}
	op, err := a.d.Containers.Stop(r.Context(), id, req, principal(r).Name, key)
	a.audit(r, "container.stop", "container", id, req.Reason, &op, err, map[string]any{"force": req.Force})
	if err != nil {
		writeError(w, r, err)
		return
	}
	accepted(w, op, nil)
}

func (a *API) restartContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	req, key, err := a.stopRequest(r, "restart")
	if err != nil {
		a.audit(r, "container.restart", "container", id, req.Reason, nil, err, map[string]any{"force": req.Force})
		writeError(w, r, err)
		return
	}
	op, err := a.d.Containers.Restart(r.Context(), id, req, principal(r).Name, key)
	a.audit(r, "container.restart", "container", id, req.Reason, &op, err, map[string]any{"force": req.Force})
	if err != nil {
		writeError(w, r, err)
		return
	}
	accepted(w, op, nil)
}

func (a *API) deleteContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req containers.DeleteRequest
	if err := decode(r, &req, true); err != nil {
		writeError(w, r, err)
		return
	}
	if f, err := boolQuery(r, "force", false); err != nil {
		writeError(w, r, err)
		return
	} else if f {
		req.Force = true
	}
	if req.Reason == "" {
		req.Reason = r.URL.Query().Get("reason")
	}
	if req.Force {
		if err := requireRole(r, auth.RoleAdmin, "a forced delete"); err != nil {
			a.audit(r, "container.delete", "container", id, req.Reason, nil, err, map[string]any{"force": true})
			writeError(w, r, err)
			return
		}
	}
	key, err := idemKey(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	op, err := a.d.Containers.Delete(r.Context(), id, req, principal(r).Name, key)
	a.audit(r, "container.delete", "container", id, req.Reason, &op, err, map[string]any{"force": req.Force})
	if err != nil {
		writeError(w, r, err)
		return
	}
	accepted(w, op, nil)
}

func (a *API) updateResources(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req profiles.ResourceRequest
	if err := decode(r, &req, false); err != nil {
		writeError(w, r, err)
		return
	}
	key, err := idemKey(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	op, err := a.d.Containers.UpdateResources(r.Context(), id, req, principal(r).Name, key)
	a.audit(r, "container.update_resources", "container", id, "", &op, err, map[string]any{"request": req})
	if err != nil {
		writeError(w, r, err)
		return
	}
	accepted(w, op, nil)
}

func (a *API) listSnapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := a.d.Containers.ListSnapshots(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snaps})
}

func (a *API) createSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req containers.SnapshotRequest
	if err := decode(r, &req, true); err != nil {
		writeError(w, r, err)
		return
	}
	key, err := idemKey(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	op, err := a.d.Containers.CreateSnapshot(r.Context(), id, req, principal(r).Name, key)
	if err != nil {
		writeError(w, r, err)
		return
	}
	accepted(w, op, nil)
}

func (a *API) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	id, snap := r.PathValue("id"), r.PathValue("snapshot")
	key, err := idemKey(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	op, err := a.d.Containers.RestoreSnapshot(r.Context(), id, snap, principal(r).Name, key)
	a.audit(r, "container.snapshot_restore", "container", id, "", &op, err, map[string]any{"snapshot": snap})
	if err != nil {
		writeError(w, r, err)
		return
	}
	accepted(w, op, nil)
}

func (a *API) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id, snap := r.PathValue("id"), r.PathValue("snapshot")
	op, err := a.d.Containers.DeleteSnapshot(r.Context(), id, snap, principal(r).Name)
	a.audit(r, "container.snapshot_delete", "container", id, "", &op, err, map[string]any{"snapshot": snap})
	if err != nil {
		writeError(w, r, err)
		return
	}
	accepted(w, op, nil)
}

// --- images ---

func (a *API) listImages(w http.ResponseWriter, r *http.Request) {
	res, err := a.d.Images.ListLocal(r.Context(), r.URL.Query().Get("host_id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) listRemoteImages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	imgs, err := a.d.Images.ListRemote(r.Context(), q.Get("server"), q.Get("q"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": imgs, "servers": a.d.Images.Servers()})
}

func (a *API) importImage(w http.ResponseWriter, r *http.Request) {
	var req images.ImportRequest
	if err := decode(r, &req, false); err != nil {
		writeError(w, r, err)
		return
	}
	key, err := idemKey(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	op, existed, err := a.d.Images.Import(r.Context(), req, principal(r).Name, key)
	a.audit(r, "image.import", "image", req.HostID+":"+req.Alias+req.Fingerprint, "", &op, err,
		map[string]any{"server": req.Server, "expected_fingerprint": req.ExpectedFingerprint})
	if err != nil {
		writeError(w, r, err)
		return
	}
	if existed {
		writeJSON(w, http.StatusOK, map[string]any{"operation": op, "existing": true})
		return
	}
	accepted(w, op, nil)
}

func (a *API) deleteImage(w http.ResponseWriter, r *http.Request) {
	fp, hostID := r.PathValue("id"), r.URL.Query().Get("host_id")
	force, err := boolQuery(r, "force", false)
	if err != nil {
		writeError(w, r, err)
		return
	}
	op, err := a.d.Images.Delete(r.Context(), hostID, fp, force, principal(r).Name)
	a.audit(r, "image.delete", "image", hostID+":"+fp, "", &op, err, map[string]any{"force": force})
	if err != nil {
		writeError(w, r, err)
		return
	}
	accepted(w, op, nil)
}

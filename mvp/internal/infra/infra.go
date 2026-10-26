// Package infra is the administrator's Infrastructure Management API. It is
// mounted under /api/admin/infra by the API, which has already enforced the
// administrator role, CSRF and (for destructive actions) step-up. Type3 is
// never exposed: this package calls it from inside the VPC.
package infra

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/paid"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/type3"
)

type Type3 interface {
	paid.Type3
	Resize(ctx context.Context, id string, r type3.Resources) (type3.Operation, error)
	Logs(ctx context.Context, id string) (string, error)
	Snapshots(ctx context.Context, id string) (json.RawMessage, error)
	Snapshot(ctx context.Context, id, name string) (type3.Operation, error)
}

type Service struct {
	Store  *store.Store
	Config paid.Config
	Type3  func(ctx context.Context, h paid.Host) (Type3, error)
}

type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return e.msg }

// Status maps errors for the API's error writer.
func Status(e error) (int, string, bool) {
	var he httpError
	if errors.As(e, &he) {
		return he.code, he.msg, true
	}
	return 0, "", false
}

func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var id = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, actor, path string) error {
	ctx := r.Context()
	p := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/hosts" && r.Method == "GET":
		return s.hosts(w, r)
	case path == "/containers" && r.Method == "GET":
		return s.containers(w, r)
	case path == "/failures" && r.Method == "GET":
		return s.failures(w, r)
	case (path == "/host/start" || path == "/host/stop") && r.Method == "POST":
		start := path == "/host/start"
		var e error
		if start {
			// Holds the host up for an hour even with no sessions.
			_, e = s.Store.DB.Exec(ctx, `UPDATE mea.gpu_hosts SET desired='running',hold_until=now()+interval '1 hour',last_busy_at=now() WHERE slot=1`)
		} else {
			// Stop only when nothing needs the host; never interrupts sessions.
			var busy bool
			if e = s.Store.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mea.desktops WHERE tier IN ('power','eleet') AND state IN ('waiting','queued','provisioning','ready','terminating')) OR EXISTS(SELECT 1 FROM mea.containers WHERE state IN ('creating','starting','running','stopping','deleting'))`).Scan(&busy); e != nil {
				return e
			}
			if busy {
				return httpError{409, "Paid desktops are active or containers are running. Stop them first."}
			}
			_, e = s.Store.DB.Exec(ctx, `UPDATE mea.gpu_hosts SET desired='stopped',hold_until=NULL,last_busy_at=now()-interval '1 day' WHERE slot=1`)
		}
		if e != nil {
			return e
		}
		if e = beta.Audit(ctx, s.Store.DB, actor, "infra.host."+map[bool]string{true: "start", false: "stop"}[start], "gpu-1", nil); e != nil {
			return e
		}
		write(w, 202, map[string]string{"desired": map[bool]string{true: "running", false: "stopped"}[start]})
		return nil
	case len(p) == 3 && p[0] == "containers" && id.MatchString(p[1]):
		return s.containerAction(w, r, actor, p[1], p[2])
	}
	return httpError{404, "Not found"}
}

func (s *Service) hosts(w http.ResponseWriter, r *http.Request) error {
	h, e := paid.GetHost(r.Context(), s.Store.DB)
	if e != nil {
		return e
	}
	var active, sessions int
	if e = s.Store.DB.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM mea.desktops WHERE tier IN ('power','eleet') AND state IN ('waiting','queued','provisioning','ready')),
 (SELECT count(*) FROM mea.desktop_sessions ds JOIN mea.desktops d ON d.id=ds.desktop_id WHERE d.tier IN ('power','eleet') AND ds.closed_at IS NULL AND ds.connected_at IS NOT NULL)`).Scan(&active, &sessions); e != nil {
		return e
	}
	seconds := h.RunningSec
	var uptime int64
	if h.StartedAt != nil {
		uptime = int64(time.Since(*h.StartedAt).Seconds())
		seconds += uptime
	}
	write(w, 200, map[string]any{"hosts": []map[string]any{{
		"slot": 1, "host": h, "activeDesktops": active, "connectedSessions": sessions, "uptimeSeconds": uptime,
		"instanceType": "g6.4xlarge", "hourlyCost": s.Config.HourlyCost,
		"estimatedRuntimeCost": float64(seconds) / 3600 * s.Config.HourlyCost,
		"limits": map[string]any{"hosts": 1, "cpu": s.Config.HostCPU, "memoryMiB": s.Config.HostMemoryMiB, "eleet": s.Config.EleetLimit},
	}}})
	return nil
}

func (s *Service) containers(w http.ResponseWriter, r *http.Request) error {
	rows, e := s.Store.DB.Query(r.Context(), `SELECT c.id::text,c.name,u.username,c.tier,c.state,c.desired,c.cpu,c.memory_mib,c.disk_gib,c.gpu,c.rdp_port,c.storage_bytes,c.error,c.updated_at,
 h.private_ip,(SELECT d.state FROM mea.desktops d WHERE d.container_id=c.id ORDER BY d.created_at DESC LIMIT 1)
 FROM mea.containers c JOIN mea.users u ON u.id=c.user_id LEFT JOIN mea.gpu_hosts h ON h.slot=c.host_slot WHERE c.state<>'deleted' ORDER BY c.created_at`)
	if e != nil {
		return e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var cid, name, user, tier, state, desired string
		var cpu, mem, disk int
		var gpu bool
		var port *int
		var storage *int64
		var errText, ip, desktop *string
		var updated time.Time
		if e = rows.Scan(&cid, &name, &user, &tier, &state, &desired, &cpu, &mem, &disk, &gpu, &port, &storage, &errText, &updated, &ip, &desktop); e != nil {
			return e
		}
		addr := ""
		if ip != nil && port != nil {
			addr = *ip + ":" + strconv.Itoa(*port)
		}
		out = append(out, map[string]any{"id": cid, "name": name, "subscriber": user, "tier": tier, "state": state, "desired": desired,
			"cpu": cpu, "memoryMiB": mem, "diskGiB": disk, "gpu": gpu, "address": addr, "storageBytes": storage, "error": errText, "updatedAt": updated, "desktop": desktop, "host": "gpu-1"})
	}
	if e = rows.Err(); e != nil {
		return e
	}
	// Live usage from the host's Type3 telemetry, when the host is up.
	h, _ := paid.GetHost(r.Context(), s.Store.DB)
	write(w, 200, map[string]any{"containers": out, "telemetry": h.Metrics, "telemetryAt": h.MetricsAt})
	return nil
}

func (s *Service) failures(w http.ResponseWriter, r *http.Request) error {
	rows, e := s.Store.DB.Query(r.Context(), `SELECT 'container',c.name,c.error,c.updated_at FROM mea.containers c WHERE c.error IS NOT NULL
 UNION ALL SELECT 'desktop',d.id::text,d.error,d.created_at FROM mea.desktops d WHERE d.tier IN ('power','eleet') AND d.state='failed' AND d.created_at>now()-interval '7 days'
 UNION ALL SELECT 'host','gpu-1',h.error,h.last_transition FROM mea.gpu_hosts h WHERE h.error IS NOT NULL ORDER BY 4 DESC LIMIT 100`)
	if e != nil {
		return e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var kind, name string
		var msg *string
		var at time.Time
		if e = rows.Scan(&kind, &name, &msg, &at); e != nil {
			return e
		}
		out = append(out, map[string]any{"kind": kind, "name": name, "error": msg, "at": at})
	}
	write(w, 200, out)
	return rows.Err()
}

func (s *Service) client(ctx context.Context) (Type3, error) {
	h, e := paid.GetHost(ctx, s.Store.DB)
	if e != nil {
		return nil, e
	}
	if s.Type3 == nil {
		return nil, httpError{409, "Power and Eleet are not deployed in this environment"}
	}
	if h.State != "running" || !h.Ready || h.PrivateIP == nil {
		return nil, httpError{409, "The GPU host is not running. Start it first."}
	}
	return s.Type3(ctx, h)
}

func (s *Service) containerAction(w http.ResponseWriter, r *http.Request, actor, cid, action string) error {
	ctx := r.Context()
	var name string
	var t3id *string
	if e := s.Store.DB.QueryRow(ctx, `SELECT name,type3_id FROM mea.containers WHERE id=$1 AND state<>'deleted'`, cid).Scan(&name, &t3id); e != nil {
		return httpError{404, "Container not found"}
	}
	audit := func(details any) error { return beta.Audit(ctx, s.Store.DB, actor, "infra.container."+action, name, details) }
	switch {
	case r.Method == "POST" && (action == "start" || action == "stop"):
		desired := map[string]string{"start": "running", "stop": "stopped"}[action]
		if _, e := s.Store.DB.Exec(ctx, `UPDATE mea.containers SET desired=$2,updated_at=now() WHERE id=$1 AND desired<>'deleted'`, cid, desired); e != nil {
			return e
		}
		if action == "stop" {
			// Ends the subscriber's desktop lease; their data stays.
			if _, e := s.Store.DB.Exec(ctx, `UPDATE mea.desktops SET state='terminating',keep_alive_until=NULL,error='Stopped by an administrator' WHERE container_id=$1 AND state IN ('waiting','queued','provisioning','ready')`, cid); e != nil {
				return e
			}
		}
		if e := audit(nil); e != nil {
			return e
		}
		write(w, 202, map[string]string{"desired": desired})
		return nil
	case r.Method == "POST" && action == "restart":
		t3, e := s.client(ctx)
		if e != nil {
			return e
		}
		if t3id == nil {
			return httpError{409, "Container has not been created yet"}
		}
		op, e := t3.Action(ctx, *t3id, "restart", map[string]any{"timeout_seconds": 60}, "")
		if e != nil {
			return e
		}
		_ = audit(nil)
		write(w, 202, op)
		return nil
	case r.Method == "POST" && action == "delete":
		var b struct{ Confirm string }
		if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&b); e != nil || b.Confirm != name {
			return httpError{400, "Type the container name to confirm deletion"}
		}
		if _, e := s.Store.DB.Exec(ctx, `UPDATE mea.containers SET desired='deleted',updated_at=now() WHERE id=$1`, cid); e != nil {
			return e
		}
		if _, e := s.Store.DB.Exec(ctx, `UPDATE mea.desktops SET state='terminating',keep_alive_until=NULL,error='Deleted by an administrator' WHERE container_id=$1 AND state IN ('waiting','queued','provisioning','ready')`, cid); e != nil {
			return e
		}
		_ = audit(nil)
		write(w, 202, map[string]string{"desired": "deleted"})
		return nil
	case r.Method == "POST" && action == "resources":
		var b struct{ CPU, MemoryMiB int }
		if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&b); e != nil {
			return httpError{400, "Invalid resources"}
		}
		if b.CPU < 1 || b.CPU > 8 || b.MemoryMiB < 1024 || b.MemoryMiB > 16384 {
			return httpError{400, "CPU 1-8 and memory 1024-16384 MiB"}
		}
		if t3id != nil {
			t3, e := s.client(ctx)
			if e != nil {
				return e
			}
			if _, e = t3.Resize(ctx, *t3id, type3.Resources{CPU: b.CPU, MemoryMiB: b.MemoryMiB}); e != nil {
				return e
			}
		}
		if _, e := s.Store.DB.Exec(ctx, `UPDATE mea.containers SET cpu=$2,memory_mib=$3,updated_at=now() WHERE id=$1`, cid, b.CPU, b.MemoryMiB); e != nil {
			return e
		}
		_ = audit(b)
		write(w, 200, map[string]bool{"ok": true})
		return nil
	case r.Method == "GET" && (action == "logs" || action == "snapshots"):
		if t3id == nil {
			return httpError{409, "Container has not been created yet"}
		}
		t3, e := s.client(ctx)
		if e != nil {
			return e
		}
		if action == "logs" {
			log, e := t3.Logs(ctx, *t3id)
			if e != nil {
				return e
			}
			write(w, 200, map[string]string{"log": log})
			return nil
		}
		snaps, e := t3.Snapshots(ctx, *t3id)
		if e != nil {
			return e
		}
		write(w, 200, snaps)
		return nil
	case r.Method == "POST" && action == "snapshots":
		if t3id == nil {
			return httpError{409, "Container has not been created yet"}
		}
		t3, e := s.client(ctx)
		if e != nil {
			return e
		}
		op, e := t3.Snapshot(ctx, *t3id, "admin-"+time.Now().UTC().Format("20060102-150405"))
		if e != nil {
			return e
		}
		_ = audit(nil)
		write(w, 202, op)
		return nil
	}
	return httpError{404, "Not found"}
}

// Package paid runs Power and Eleet desktops: persistent LXD containers on the
// single GPU host, orchestrated through Type3. The database is the source of
// truth: the API records what users want, the worker (outside the VPC) drives
// EC2 start/stop of the host, and the control function (inside the VPC)
// drives Type3 and Guacamole. Every step is idempotent and safe to repeat.
package paid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/security"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/type3"
	"github.com/jackc/pgx/v5"
)

type Size struct{ CPU, MemoryMiB, DiskGiB int }

// Config holds the configurable limits. Allocations are maximums enforced by
// LXD, not dedicated cores; admission keeps their sum within the host budget.
type Config struct {
	Enabled bool
	Power   Size
	Eleet   Size
	// HostCPU/HostMemoryMiB are what containers may use after reserving
	// capacity for the host itself (g6.4xlarge: 16 vCPU, 64 GiB).
	HostCPU, HostMemoryMiB int
	// EleetLimit is the conservative concurrent Eleet admission limit.
	EleetLimit        int
	HostIdleSeconds   int
	KeepAliveMaxHours int
	HourlyCost        float64
}

func Defaults() Config {
	return Config{Power: Size{2, 6144, 40}, Eleet: Size{4, 12288, 60}, HostCPU: 14, HostMemoryMiB: 54 * 1024, EleetLimit: 2, HostIdleSeconds: 900, KeepAliveMaxHours: 12, HourlyCost: 1.3282}
}
func (c Config) Size(tier string) Size {
	if tier == "eleet" {
		return c.Eleet
	}
	return c.Power
}

type Fail struct {
	Code    int
	Message string
}

func (f Fail) Error() string { return f.Message }

// active desktop states hold a container lease.
const activeStates = `('waiting','queued','provisioning','ready','terminating')`

// Request records a user's request for their Power or Eleet desktop. It never
// talks to AWS or Type3; the worker and control loops act on the rows.
func Request(ctx context.Context, st *store.Store, c Config, user, tier string, lifetime time.Duration) (store.Desktop, error) {
	var d store.Desktop
	if !c.Enabled {
		return d, Fail{503, "Power and Eleet desktops are not available yet"}
	}
	access, e := beta.AccessFor(ctx, st.DB, user)
	if e != nil {
		return d, e
	}
	if !access.Has(tier) {
		return d, Fail{403, "Your account does not include " + strings.Title(tier) + ". Choose a plan to continue."}
	}
	tx, e := st.DB.Begin(ctx)
	if e != nil {
		return d, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(739103)"); e != nil {
		return d, e
	}
	d, e = store.ScanDesktop(tx.QueryRow(ctx, "SELECT "+store.DesktopColumns+" FROM mea.desktops WHERE user_id=$1 AND state IN "+activeStates, user))
	if e == nil {
		if d.Tier != tier {
			return d, Fail{409, "Stop your current desktop before opening another plan's desktop"}
		}
		return d, tx.Commit(ctx)
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return d, e
	}
	var container string
	e = tx.QueryRow(ctx, `SELECT id::text FROM mea.containers WHERE user_id=$1 AND tier=$2 AND state<>'deleted'`, user, tier).Scan(&container)
	if errors.Is(e, pgx.ErrNoRows) {
		size := c.Size(tier)
		container = security.ID()
		var port int
		if e = tx.QueryRow(ctx, `SELECT p FROM generate_series(40000,40999) p WHERE p NOT IN (SELECT rdp_port FROM mea.containers WHERE rdp_port IS NOT NULL) ORDER BY p LIMIT 1`).Scan(&port); e != nil {
			return d, Fail{503, "No desktop ports are free"}
		}
		name := fmt.Sprintf("mea-%s-%s", tier, strings.ReplaceAll(user, "-", "")[:12])
		if _, e = tx.Exec(ctx, `INSERT INTO mea.containers(id,user_id,tier,name,state,desired,host_slot,rdp_port,cpu,memory_mib,disk_gib,gpu)
 VALUES($1,$2,$3,$4,'requested','stopped',1,$5,$6,$7,$8,$9)`, container, user, tier, name, port, size.CPU, size.MemoryMiB, size.DiskGiB, tier == "eleet"); e != nil {
			return d, e
		}
	} else if e != nil {
		return d, e
	}
	id, op := security.ID(), security.ID()
	d, e = store.ScanDesktop(tx.QueryRow(ctx, `INSERT INTO mea.desktops(id,user_id,state,operation_id,expires_at,tier,container_id)
 VALUES($1,$2,'waiting',$3,now()+$4::interval,$5,$6) RETURNING `+store.DesktopColumns, id, user, op, fmt.Sprintf("%d seconds", int(lifetime.Seconds())), tier, container))
	if e != nil {
		return d, e
	}
	if _, e = tx.Exec(ctx, `UPDATE mea.gpu_hosts SET desired='running' WHERE slot=1`); e != nil {
		return d, e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO mea.audit(user_id,desktop_id,action) VALUES($1,$2,'desktop.create.'||$3)`, user, id, tier); e != nil {
		return d, e
	}
	return d, tx.Commit(ctx)
}

// KeepAlive lets a running paid desktop survive inactivity and disconnects
// for a bounded time, for authorized long-running work.
func KeepAlive(ctx context.Context, st *store.Store, c Config, user, desktop string, hours int) error {
	if hours < 0 || hours > c.KeepAliveMaxHours {
		return Fail{400, fmt.Sprintf("Keep-alive can be between 0 and %d hours", c.KeepAliveMaxHours)}
	}
	tag, e := st.DB.Exec(ctx, `UPDATE mea.desktops SET keep_alive_until=CASE WHEN $3=0 THEN NULL ELSE now()+$3*interval '1 hour' END
 WHERE id=$1 AND user_id=$2 AND tier IN ('power','eleet') AND state='ready'`, desktop, user, hours)
	if e == nil && tag.RowsAffected() != 1 {
		return Fail{409, "Keep-alive is available for a running Power or Eleet desktop"}
	}
	return e
}

// Host is the single GPU host row.
type Host struct {
	InstanceID  *string         `json:"instanceId"`
	State       string          `json:"state"`
	Desired     string          `json:"desired"`
	PrivateIP   *string         `json:"privateIp"`
	Ready       bool            `json:"ready"`
	Fingerprint *string         `json:"-"`
	StartedAt   *time.Time      `json:"startedAt"`
	LastBusyAt  *time.Time      `json:"lastBusyAt"`
	HoldUntil   *time.Time      `json:"holdUntil"`
	RunningSec  int64           `json:"runningSeconds"`
	Launches    int             `json:"launches"`
	Metrics     json.RawMessage `json:"metrics"`
	MetricsAt   *time.Time      `json:"metricsAt"`
	Error       *string         `json:"error"`
}

func GetHost(ctx context.Context, db beta.DB) (Host, error) {
	var h Host
	e := db.QueryRow(ctx, `SELECT instance_id,state,desired,private_ip,ready,fingerprint,started_at,last_busy_at,hold_until,running_seconds,launches,metrics,metrics_at,error FROM mea.gpu_hosts WHERE slot=1`).
		Scan(&h.InstanceID, &h.State, &h.Desired, &h.PrivateIP, &h.Ready, &h.Fingerprint, &h.StartedAt, &h.LastBusyAt, &h.HoldUntil, &h.RunningSec, &h.Launches, &h.Metrics, &h.MetricsAt, &h.Error)
	return h, e
}

// Plan is what the worker needs to drive EC2 for the host.
type Plan struct {
	Host      Host `json:"host"`
	Needed    bool `json:"needed"`
	StopNow   bool `json:"stopNow"`
	LeaseHeld bool `json:"leaseHeld"`
}

// needed reports whether anything requires the host to run: a paid desktop
// lease, a container not yet at rest, a pending delete or an admin hold.
const neededSQL = `SELECT EXISTS(SELECT 1 FROM mea.desktops WHERE tier IN ('power','eleet') AND state IN ` + activeStates + `)
 OR EXISTS(SELECT 1 FROM mea.containers WHERE state IN ('creating','starting','running','stopping','deleting') OR (desired='running' AND state<>'deleted') OR (desired='deleted' AND state<>'deleted' AND type3_id IS NOT NULL))
 OR EXISTS(SELECT 1 FROM mea.gpu_hosts WHERE slot=1 AND hold_until>now())`

// HostPlan decides the desired host state. The host stops only after nothing
// has needed it for HostIdleSeconds, so an active Power user keeps it up.
func HostPlan(ctx context.Context, st *store.Store, c Config) (Plan, error) {
	var p Plan
	tx, e := st.DB.Begin(ctx)
	if e != nil {
		return p, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT 1 FROM mea.gpu_hosts WHERE slot=1 FOR UPDATE`); e != nil {
		return p, e
	}
	if e = tx.QueryRow(ctx, neededSQL).Scan(&p.Needed); e != nil {
		return p, e
	}
	if p.Needed {
		_, e = tx.Exec(ctx, `UPDATE mea.gpu_hosts SET desired='running',last_busy_at=now() WHERE slot=1`)
	} else {
		_, e = tx.Exec(ctx, `UPDATE mea.gpu_hosts SET desired='stopped' WHERE slot=1 AND desired='running' AND (last_busy_at IS NULL OR last_busy_at<now()-$1*interval '1 second')`, c.HostIdleSeconds)
	}
	if e != nil {
		return p, e
	}
	if p.Host, e = GetHost(ctx, tx); e != nil {
		return p, e
	}
	p.StopNow = p.Host.Desired == "stopped" && (p.Host.State == "running" || p.Host.State == "pending")
	return p, tx.Commit(ctx)
}

// Lease serializes host launches/starts across concurrent worker runs.
func Lease(ctx context.Context, st *store.Store, owner string, d time.Duration) (bool, error) {
	tag, e := st.DB.Exec(ctx, `UPDATE mea.gpu_hosts SET lease_owner=$1,lease_until=now()+$2*interval '1 second'
 WHERE slot=1 AND (lease_until IS NULL OR lease_until<now() OR lease_owner=$1)`, owner, int(d.Seconds()))
	return e == nil && tag.RowsAffected() == 1, e
}

// Observation is the worker's report of the EC2 instance.
type Observation struct {
	InstanceID  string `json:"instanceId"`
	State       string `json:"state"`
	PrivateIP   string `json:"privateIp"`
	Fingerprint string `json:"fingerprint"`
	Launched    bool   `json:"launched"`
	Error       string `json:"error"`
}

func Observe(ctx context.Context, st *store.Store, o Observation) error {
	state := o.State
	switch state {
	case "", "terminated":
		state = "absent"
	case "shutting-down":
		state = "stopping"
	}
	_, e := st.DB.Exec(ctx, `UPDATE mea.gpu_hosts SET
 running_seconds=running_seconds+CASE WHEN state='running' THEN GREATEST(0,EXTRACT(EPOCH FROM now()-last_transition)::bigint) ELSE 0 END,
 last_transition=now(),
 started_at=CASE WHEN $2='running' AND state<>'running' THEN now() WHEN $2<>'running' THEN NULL ELSE started_at END,
 ready=CASE WHEN $2='running' THEN ready ELSE false END,
 instance_id=NULLIF($1,''),state=$2,private_ip=NULLIF($3,''),fingerprint=COALESCE(NULLIF($4,''),fingerprint),
 launches=launches+CASE WHEN $5 THEN 1 ELSE 0 END,error=NULLIF($6,'') WHERE slot=1`,
		o.InstanceID, state, o.PrivateIP, o.Fingerprint, o.Launched, o.Error)
	return e
}

// Type3 is the subset of the Type3 client the reconciler uses.
type Type3 interface {
	Health(ctx context.Context) error
	EnsureLocalHost(ctx context.Context, name string) (type3.Host, error)
	Create(ctx context.Context, r type3.CreateRequest, idem string) (type3.Container, type3.Operation, error)
	Container(ctx context.Context, id string) (type3.Container, error)
	Action(ctx context.Context, id, action string, body any, idem string) (type3.Operation, error)
	Delete(ctx context.Context, id, reason string) (type3.Operation, error)
	SetPassword(ctx context.Context, id, password string) error
	Telemetry(ctx context.Context, host string, storage bool) (json.RawMessage, error)
}

// Engine reconciles containers and paid desktops with Type3.
type Engine struct {
	Store  *store.Store
	Config Config
	Type3  func(ctx context.Context, h Host) (Type3, error)
	// Ready probes XRDP and publishes the Guacamole connection (control "ready").
	Ready func(ctx context.Context, desktop, ip string, port int, password string) (bool, error)
	// Terminated finishes a desktop lease (control "terminated").
	Terminated func(ctx context.Context, desktop string) error
}

const hostName = "gpu-1"

type row struct {
	id, user, tier, name, state, desired string
	type3ID                              *string
	port                                 int
	size                                 Size
}

// Reconcile performs one bounded pass. It returns a summary for logs.
func (en *Engine) Reconcile(ctx context.Context) (map[string]any, error) {
	db := en.Store.DB
	out := map[string]any{}
	h, e := GetHost(ctx, db)
	if e != nil {
		return out, e
	}
	// Abandoned waits and failed placements are surfaced, never retried forever.
	if _, e = db.Exec(ctx, `UPDATE mea.desktops SET state='terminating',error='Your desktop could not start in time. Please try again.' WHERE tier IN ('power','eleet') AND state IN ('queued','provisioning') AND created_at<now()-interval '45 minutes'`); e != nil {
		return out, e
	}
	if e = en.admit(ctx); e != nil {
		return out, e
	}
	// Leases on containers that are not running finish without the host, so
	// a cancelled wait never wakes the GPU host.
	rows, e := db.Query(ctx, `SELECT d.id::text FROM mea.desktops d JOIN mea.containers c ON c.id=d.container_id
 WHERE d.state='terminating' AND c.state IN ('requested','stopped','failed','deleted') LIMIT 20`)
	if e != nil {
		return out, e
	}
	done, e := pgx.CollectRows(rows, pgx.RowTo[string])
	if e != nil {
		return out, e
	}
	for _, id := range done {
		if e = en.Terminated(ctx, id); e != nil {
			return out, e
		}
	}
	if h.State != "running" || h.PrivateIP == nil || h.Fingerprint == nil {
		out["host"] = h.State
		return out, nil
	}
	t3, e := en.Type3(ctx, h)
	if e != nil {
		return out, e
	}
	if e = t3.Health(ctx); e != nil {
		_, _ = db.Exec(ctx, `UPDATE mea.gpu_hosts SET ready=false WHERE slot=1`)
		out["type3"] = "unavailable"
		return out, nil
	}
	host, e := t3.EnsureLocalHost(ctx, hostName)
	if e != nil {
		return out, fmt.Errorf("register LXD host: %w", e)
	}
	if host.Status != "online" {
		out["lxd"] = host.Status
		return out, nil
	}
	if _, e = db.Exec(ctx, `UPDATE mea.gpu_hosts SET ready=true WHERE slot=1`); e != nil {
		return out, e
	}
	if raw, err := t3.Telemetry(ctx, host.ID, false); err == nil {
		_, _ = db.Exec(ctx, `UPDATE mea.gpu_hosts SET metrics=$1,metrics_at=now() WHERE slot=1`, raw)
	}
	// Containers whose desired state differs from Type3, at most 8 per pass.
	rows, e = db.Query(ctx, `SELECT c.id::text,c.user_id::text,c.tier,c.name,c.state,c.desired,c.type3_id,c.rdp_port,c.cpu,c.memory_mib,c.disk_gib
 FROM mea.containers c WHERE c.state<>'deleted' AND (c.state NOT IN ('stopped','requested') OR c.desired<>'stopped'
  OR EXISTS(SELECT 1 FROM mea.desktops d WHERE d.container_id=c.id AND d.state IN ('queued','provisioning','ready','terminating')))
 ORDER BY c.updated_at LIMIT 8`)
	if e != nil {
		return out, e
	}
	list, e := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.user, &x.tier, &x.name, &x.state, &x.desired, &x.type3ID, &x.port, &x.size.CPU, &x.size.MemoryMiB, &x.size.DiskGiB)
		return x, err
	})
	if e != nil {
		return out, e
	}
	acted := 0
	for _, c := range list {
		if err := en.converge(ctx, t3, host.ID, c, h); err != nil {
			_, _ = db.Exec(ctx, `UPDATE mea.containers SET error=$2,updated_at=now() WHERE id=$1`, c.id, truncate(err.Error()))
			out["error:"+c.name] = truncate(err.Error())
			continue
		}
		acted++
	}
	out["containers"] = acted
	return out, nil
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// admit moves waiting desktops into the provisioning queue in arrival order
// while the host's CPU, memory and Eleet budgets allow. Requests beyond
// capacity wait (and retry automatically) instead of starting another host.
func (en *Engine) admit(ctx context.Context) error {
	tx, e := en.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(739104)"); e != nil {
		return e
	}
	var cpu, mem, eleet int
	if e = tx.QueryRow(ctx, `SELECT COALESCE(sum(c.cpu),0),COALESCE(sum(c.memory_mib),0),count(*) FILTER (WHERE c.tier='eleet') FROM mea.containers c
 WHERE c.state IN ('creating','starting','running','stopping') OR EXISTS(SELECT 1 FROM mea.desktops d WHERE d.container_id=c.id AND d.state IN ('queued','provisioning','ready'))`).Scan(&cpu, &mem, &eleet); e != nil {
		return e
	}
	rows, e := tx.Query(ctx, `SELECT d.id::text,d.tier,c.cpu,c.memory_mib,c.state FROM mea.desktops d JOIN mea.containers c ON c.id=d.container_id WHERE d.state='waiting' ORDER BY d.created_at LIMIT 20`)
	if e != nil {
		return e
	}
	type wait struct {
		id, tier, state string
		cpu, mem        int
	}
	waits, e := pgx.CollectRows(rows, func(r pgx.CollectableRow) (wait, error) {
		var w wait
		err := r.Scan(&w.id, &w.tier, &w.cpu, &w.mem, &w.state)
		return w, err
	})
	if e != nil {
		return e
	}
	c := en.Config
	position := 0
	for _, w := range waits {
		// A container that is already running is counted; admit its desktop.
		running := w.state == "running"
		fits := running || (cpu+w.cpu <= c.HostCPU && mem+w.mem <= c.HostMemoryMiB && (w.tier != "eleet" || eleet < c.EleetLimit))
		if !fits {
			position++
			if _, e = tx.Exec(ctx, `UPDATE mea.desktops SET error=$2 WHERE id=$1`, w.id, fmt.Sprintf("All %s capacity is in use. You are number %d in line; your desktop starts automatically when a slot frees.", strings.Title(w.tier), position)); e != nil {
				return e
			}
			continue
		}
		if !running {
			cpu, mem = cpu+w.cpu, mem+w.mem
			if w.tier == "eleet" {
				eleet++
			}
		}
		if _, e = tx.Exec(ctx, `UPDATE mea.desktops SET state='queued',error=NULL WHERE id=$1 AND state='waiting'`, w.id); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}

// converge moves one container toward its desired state and serves the
// desktop attached to it.
func (en *Engine) converge(ctx context.Context, t3 Type3, hostID string, c row, h Host) error {
	db := en.Store.DB
	var desktop, dstate string
	err := db.QueryRow(ctx, `SELECT id::text,state FROM mea.desktops WHERE container_id=$1 AND state IN ('queued','provisioning','ready','terminating') ORDER BY created_at DESC LIMIT 1`, c.id).Scan(&desktop, &dstate)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var keep bool
	if err = db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mea.desktops WHERE container_id=$1 AND state='ready' AND keep_alive_until>now())`, c.id).Scan(&keep); err != nil {
		return err
	}
	// containers.desired is 'stopped' by default, 'running' when an
	// administrator pins it, or 'deleted'. A desktop lease or keep-alive also
	// needs it running; otherwise it stops and the host can sleep.
	desired := "stopped"
	switch {
	case c.desired == "deleted":
		desired = "deleted"
	case c.desired == "running" || keep || dstate == "queued" || dstate == "provisioning" || dstate == "ready":
		desired = "running"
	}
	set := func(state string) error {
		_, err := db.Exec(ctx, `UPDATE mea.containers SET state=$2,error=NULL,updated_at=now() WHERE id=$1`, c.id, state)
		return err
	}
	if c.type3ID == nil {
		if desired != "running" {
			if desired == "deleted" {
				return set("deleted")
			}
			return nil
		}
		_, op, err := t3.Create(ctx, type3.CreateRequest{Name: c.name, OwnerID: c.user, Tier: c.tier, HostID: hostID,
			Resources: type3.Resources{CPU: c.size.CPU, MemoryMiB: c.size.MemoryMiB, DiskGiB: c.size.DiskGiB}, RDPPort: c.port, PersistentHome: true}, "mea-"+c.id)
		if err != nil {
			return err
		}
		if _, err = db.Exec(ctx, `UPDATE mea.containers SET type3_id=$2,operation_id=$3,state='creating',error=NULL,updated_at=now() WHERE id=$1`, c.id, c.name, op.ID); err != nil {
			return err
		}
		if desktop != "" && dstate == "queued" {
			_, err = db.Exec(ctx, `UPDATE mea.desktops SET state='provisioning',execution_arn=$2 WHERE id=$1 AND state='queued'`, desktop, "type3:"+op.ID)
		}
		return err
	}
	live, err := t3.Container(ctx, *c.type3ID)
	if type3.IsNotFound(err) {
		// Type3 lost the record (host replaced): recreate from the same name;
		// Type3 adopts an existing LXD instance with a matching spec.
		_, err = db.Exec(ctx, `UPDATE mea.containers SET type3_id=NULL,state='requested',updated_at=now() WHERE id=$1`, c.id)
		return err
	}
	if err != nil {
		return err
	}
	if live.ActiveOp != nil && (live.ActiveOp.Status == "running" || live.ActiveOp.Status == "pending") {
		if s, ok := map[string]string{"container.create": "creating", "container.start": "starting", "container.stop": "stopping", "container.delete": "deleting"}[live.ActiveOp.Type]; ok {
			return set(s)
		}
		return nil
	}
	switch {
	case desired == "deleted":
		if live.Status == "missing" || live.Status == "" {
			return set("deleted")
		}
		if _, err = t3.Delete(ctx, *c.type3ID, "MEA administrator deleted the desktop"); err != nil && !type3.IsNotFound(err) {
			return err
		}
		return set("deleting")
	case desired == "running" && live.Status == "error":
		_ = set("failed")
		if desktop != "" {
			_, _ = db.Exec(ctx, `UPDATE mea.desktops SET state='terminating',error='Your desktop failed to start. An administrator has been notified.' WHERE id=$1 AND state IN ('queued','provisioning')`, desktop)
		}
		return fmt.Errorf("type3 reports error: %s", live.Error)
	case desired == "running" && live.Status != "running":
		if _, err = t3.Action(ctx, *c.type3ID, "start", nil, ""); err != nil {
			return err
		}
		if desktop != "" && dstate == "queued" {
			_, _ = db.Exec(ctx, `UPDATE mea.desktops SET state='provisioning' WHERE id=$1 AND state='queued'`, desktop)
		}
		return set("starting")
	case desired == "running":
		if err = set("running"); err != nil {
			return err
		}
		if desktop == "" {
			return nil
		}
		if dstate == "queued" {
			if _, err = db.Exec(ctx, `UPDATE mea.desktops SET state='provisioning' WHERE id=$1 AND state='queued'`, desktop); err != nil {
				return err
			}
			dstate = "provisioning"
		}
		if dstate != "provisioning" || !live.HomeReady {
			return nil
		}
		// A fresh random password per connection lease; it is stored only in
		// the Guacamole connection parameters, never shown to anyone.
		password := security.Random(24)
		if err = t3.SetPassword(ctx, *c.type3ID, password); err != nil {
			return err
		}
		_, err = en.Ready(ctx, desktop, *h.PrivateIP, c.port, password)
		return err
	case desired == "stopped" && (live.Status == "running" || live.Status == "starting"):
		if _, err = t3.Action(ctx, *c.type3ID, "stop", map[string]any{"timeout_seconds": 60}, ""); err != nil {
			return err
		}
		return set("stopping")
	default:
		if live.Status == "stopped" {
			if err = set("stopped"); err != nil {
				return err
			}
			// The container is safely stopped: release the desktop lease and
			// its Guacamole connection. Files and installed software persist.
			if desktop != "" && dstate == "terminating" {
				err = en.Terminated(ctx, desktop)
			}
		}
		return err
	}
}

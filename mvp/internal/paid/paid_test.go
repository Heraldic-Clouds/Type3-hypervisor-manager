package paid_test

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/control"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/paid"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/security"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/type3"
)

// fakeType3 behaves like Type3 on the host: creates finish immediately and
// containers move between stopped and running.
type fakeType3 struct {
	mu        sync.Mutex
	c         map[string]*type3.Container
	creates   []type3.CreateRequest
	passwords map[string]string
}

func (f *fakeType3) Health(context.Context) error { return nil }
func (f *fakeType3) EnsureLocalHost(context.Context, string) (type3.Host, error) {
	return type3.Host{ID: "h1", Name: "gpu-1", Status: "online"}, nil
}
func (f *fakeType3) Create(_ context.Context, r type3.CreateRequest, _ string) (type3.Container, type3.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, r)
	if _, ok := f.c[r.Name]; !ok {
		f.c[r.Name] = &type3.Container{ID: r.Name, Tier: r.Tier, GPU: r.Tier == "eleet", Status: "running", RDPPort: r.RDPPort, HomeReady: r.PersistentHome}
	}
	return *f.c[r.Name], type3.Operation{ID: "op-" + r.Name, Status: "succeeded"}, nil
}
func (f *fakeType3) Container(_ context.Context, id string) (type3.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.c[id]
	if !ok {
		return type3.Container{}, type3.APIError{Status: 404}
	}
	return *c, nil
}
func (f *fakeType3) Action(_ context.Context, id, action string, _ any, _ string) (type3.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.c[id]
	if action == "stop" {
		c.Status = "stopped"
	} else {
		c.Status = "running"
	}
	return type3.Operation{Status: "succeeded"}, nil
}
func (f *fakeType3) Delete(_ context.Context, id, _ string) (type3.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.c, id)
	return type3.Operation{}, nil
}
func (f *fakeType3) SetPassword(_ context.Context, id, pw string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.passwords[id] = pw
	return nil
}
func (f *fakeType3) Telemetry(context.Context, string, bool) (json.RawMessage, error) {
	return json.RawMessage(`{"gpus":[{"gpu_percent":12}]}`), nil
}

// rdp answers the X.224 connection request like XRDP so the control
// function's readiness probe and Guacamole connection publishing run for real.
func rdp(t *testing.T, port string) {
	l, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", port, err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 64)
				if n, _ := c.Read(buf); n > 0 {
					c.Write([]byte{3, 0, 0, 19, 14, 0xd0, 0, 0, 0, 0, 0, 2, 0, 8, 0, 1, 0, 0, 0})
				}
			}()
		}
	}()
}

func TestPaidLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for real PostgreSQL integration")
	}
	if u, _ := url.Parse(dsn); u == nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatal("local PostgreSQL only")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.DB.Close()
	schema, _ := os.ReadFile("../../schema/guacamole.sql")
	if err = st.Migrate(ctx, string(schema)); err != nil {
		t.Fatal(err)
	}
	st.DB.Exec(ctx, `TRUNCATE mea.admin_audit,mea.entitlements,mea.subscriptions,mea.stripe_customers,mea.stripe_events,mea.invitation_reservations,mea.invitations,mea.user_roles,mea.audit,mea.outbox,mea.desktop_sessions,mea.desktops,mea.containers,mea.tokens,mea.sessions,mea.users,mea.rate_limits,guacamole_entity CASCADE`)
	st.DB.Exec(ctx, `UPDATE mea.gpu_hosts SET instance_id=NULL,state='absent',desired='stopped',private_ip=NULL,ready=false,lease_owner=NULL,lease_until=NULL,last_busy_at=NULL,hold_until=NULL,fingerprint=NULL,launches=0,error=NULL`)
	if _, err = st.DB.Exec(ctx, `INSERT INTO mea.gpu_hosts(slot) VALUES(2)`); err == nil {
		t.Fatal("database allowed a second GPU host slot")
	}
	user := func(name string) string {
		var entity, guac int
		st.DB.QueryRow(ctx, `INSERT INTO guacamole_entity(name,type) VALUES($1,'USER') RETURNING entity_id`, name).Scan(&entity)
		st.DB.QueryRow(ctx, `INSERT INTO guacamole_user(entity_id,password_hash,password_salt,password_date) VALUES($1,'\x00','\x00',now()) RETURNING user_id`, entity).Scan(&guac)
		id := security.ID()
		st.DB.Exec(ctx, `INSERT INTO mea.users(id,guac_user_id,entity_id,username,email,name,password_hash,verified) VALUES($1,$2,$3,$4,$5,$4,'x',true)`, id, guac, entity, name, name+"@example.test")
		return id
	}
	admin, alice, bob, carol := user("tantalus"), user("alice"), user("bob"), user("carol")
	beta.GrantAdmin(ctx, st.DB, "", admin, "", "test")
	for _, u := range []string{bob, carol} {
		beta.SetComp(ctx, st.DB, admin, u, "eleet", true, "test")
	}
	cfg := paid.Defaults()
	cfg.Enabled, cfg.EleetLimit, cfg.HostIdleSeconds = true, 1, 0
	life := 10 * 365 * 24 * time.Hour

	if _, err = paid.Request(ctx, st, cfg, alice, "power", life); err == nil || !strings.Contains(err.Error(), "does not include") {
		t.Fatalf("unentitled user got a Power desktop: %v", err)
	}
	if _, err = paid.Request(ctx, st, cfg, bob, "power", life); err == nil {
		t.Fatal("Eleet subscriber received Power without entitlement")
	}
	d1, err := paid.Request(ctx, st, cfg, admin, "power", life)
	if err != nil || d1.State != "waiting" || d1.Tier != "power" {
		t.Fatalf("admin power %v %+v", err, d1)
	}
	again, _ := paid.Request(ctx, st, cfg, admin, "power", life)
	if again.ID != d1.ID {
		t.Fatal("duplicate desktop for the same user")
	}
	d2, _ := paid.Request(ctx, st, cfg, bob, "eleet", life)
	d3, _ := paid.Request(ctx, st, cfg, carol, "eleet", life)
	var gpuFlags []bool
	rows, _ := st.DB.Query(ctx, `SELECT gpu FROM mea.containers ORDER BY tier DESC,created_at`)
	for rows.Next() {
		var g bool
		rows.Scan(&g)
		gpuFlags = append(gpuFlags, g)
	}
	rows.Close()
	if len(gpuFlags) != 3 || gpuFlags[0] || !gpuFlags[1] || !gpuFlags[2] {
		t.Fatalf("container GPU flags %v (power must be false)", gpuFlags)
	}

	plan, err := paid.HostPlan(ctx, st, cfg)
	if err != nil || !plan.Needed || plan.Host.Desired != "running" {
		t.Fatalf("plan %+v %v", plan, err)
	}
	// Concurrent workers: exactly one gets the launch lease.
	var mu sync.Mutex
	won := 0
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if ok, _ := paid.Lease(ctx, st, "worker-"+string(rune('a'+i)), time.Minute); ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d workers held the GPU launch lease", won)
	}

	fake := &fakeType3{c: map[string]*type3.Container{}, passwords: map[string]string{}}
	svc := &control.Service{Store: st, IdleSeconds: 1200, GraceSeconds: 300, Paid: cfg, Type3: func(context.Context, paid.Host) (paid.Type3, error) { return fake, nil }}
	reconcile := func() {
		t.Helper()
		if _, err := svc.Handle(ctx, control.Request{Action: "paid-reconcile"}); err != nil {
			t.Fatal(err)
		}
	}
	state := func(id string) (s string, msg *string) {
		st.DB.QueryRow(ctx, `SELECT state,error FROM mea.desktops WHERE id=$1`, id).Scan(&s, &msg)
		return
	}
	// Host still stopped: admission happens, nothing is created.
	reconcile()
	if s, _ := state(d1.ID); s != "queued" {
		t.Fatalf("power not admitted: %s", s)
	}
	if s, msg := state(d3.ID); s != "waiting" || msg == nil || !strings.Contains(*msg, "number 1 in line") {
		t.Fatalf("over-capacity Eleet should wait: %s %v", s, msg)
	}
	if len(fake.creates) != 0 {
		t.Fatal("containers created before the host was running")
	}
	if err = paid.Observe(ctx, st, paid.Observation{InstanceID: "i-0gpu", State: "running", PrivateIP: "127.0.0.1", Fingerprint: "ab", Launched: true}); err != nil {
		t.Fatal(err)
	}
	var ports []string
	prow, _ := st.DB.Query(ctx, `SELECT rdp_port::text FROM mea.containers ORDER BY rdp_port`)
	for prow.Next() {
		var p string
		prow.Scan(&p)
		ports = append(ports, p)
	}
	prow.Close()
	for _, p := range ports {
		rdp(t, p)
	}
	for i := 0; i < 4; i++ {
		reconcile()
	}
	if s, _ := state(d1.ID); s != "ready" {
		t.Fatalf("power desktop %s", s)
	}
	if s, _ := state(d2.ID); s != "ready" {
		t.Fatalf("eleet desktop %s", s)
	}
	for _, cr := range fake.creates {
		if !cr.PersistentHome || cr.RDPPort < 40000 {
			t.Fatalf("create request %+v", cr)
		}
	}
	var conns, perms int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM guacamole_connection WHERE connection_name LIKE 'MEA %'`).Scan(&conns)
	st.DB.QueryRow(ctx, `SELECT count(*) FROM guacamole_connection_permission p JOIN mea.desktops d ON d.connection_id=p.connection_id JOIN mea.users u ON u.entity_id=p.entity_id AND u.id=d.user_id`).Scan(&perms)
	if conns != 2 || perms != 2 {
		t.Fatalf("guacamole connections %d, owner permissions %d", conns, perms)
	}
	if len(fake.passwords) != 2 {
		t.Fatal("desktop passwords were not set")
	}

	// Keep-alive survives inactivity; plain idle does not.
	if err = paid.KeepAlive(ctx, st, cfg, admin, d1.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err = paid.KeepAlive(ctx, st, cfg, admin, d1.ID, 48); err == nil {
		t.Fatal("unbounded keep-alive accepted")
	}
	st.DB.Exec(ctx, `UPDATE mea.desktops SET last_activity=now()-interval '2 hours',disconnect_at=now()-interval '2 hours' WHERE id IN ($1,$2)`, d1.ID, d2.ID)
	svc.Handle(ctx, control.Request{Action: "reconcile"})
	if s, _ := state(d1.ID); s != "ready" {
		t.Fatalf("keep-alive desktop was stopped: %s", s)
	}
	if s, _ := state(d2.ID); s != "terminating" {
		t.Fatalf("idle Eleet desktop not released: %s", s)
	}
	for i := 0; i < 4; i++ {
		reconcile()
	}
	if s, _ := state(d2.ID); s != "terminated" {
		t.Fatalf("eleet lease not finished: %s", s)
	}
	var bobState string
	st.DB.QueryRow(ctx, `SELECT state FROM mea.containers WHERE user_id=$1`, bob).Scan(&bobState)
	if bobState != "stopped" || fake.c[containerName(st, bob)].Status != "stopped" {
		t.Fatalf("idle container must be stopped, not deleted: %s", bobState)
	}
	// The freed Eleet slot goes to the waiting user automatically.
	for i := 0; i < 4; i++ {
		reconcile()
	}
	if s, _ := state(d3.ID); s != "ready" {
		t.Fatalf("queued Eleet desktop did not start after capacity freed: %s", s)
	}
	// Reconnect to a stopped container reuses it (no second create).
	before := len(fake.creates)
	d2b, _ := paid.Request(ctx, st, cfg, bob, "eleet", life)
	if d2b.State != "waiting" {
		t.Fatal("re-request")
	}
	if len(fake.creates) != before {
		t.Fatal("re-request created a duplicate container")
	}

	// Everyone leaves: the host becomes idle and is told to stop.
	st.DB.Exec(ctx, `UPDATE mea.desktops SET state='terminating',keep_alive_until=NULL WHERE state IN ('waiting','queued','provisioning','ready')`)
	for i := 0; i < 6; i++ {
		reconcile()
	}
	var live int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM mea.containers WHERE state IN ('creating','starting','running','stopping')`).Scan(&live)
	if live != 0 {
		t.Fatalf("%d containers still running", live)
	}
	plan, _ = paid.HostPlan(ctx, st, cfg)
	if plan.Needed || plan.Host.Desired != "stopped" || !plan.StopNow {
		t.Fatalf("idle host not stopping: %+v", plan)
	}
	var count int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM mea.containers WHERE state='deleted'`).Scan(&count)
	if count != 0 {
		t.Fatal("idle shutdown deleted containers")
	}
}

func containerName(st *store.Store, user string) string {
	var n string
	st.DB.QueryRow(context.Background(), `SELECT name FROM mea.containers WHERE user_id=$1`, user).Scan(&n)
	return n
}

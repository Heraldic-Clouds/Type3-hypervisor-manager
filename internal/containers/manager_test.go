package containers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canonical/lxd/shared/api"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/config"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/hosts"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/images"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/logging"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/lxdclient"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/lxdclient/fake"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/operations"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/profiles"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/testutil"
)

func init() { lxdclient.PollInterval = 10 * time.Millisecond }

type harness struct {
	t    *testing.T
	st   *store.FileStore
	conn *fake.Connector
	reg  *hosts.Registry
	ops  *operations.Manager
	mgr  *Manager
	cpu  *fake.Server
	gpu  *fake.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	certPath, keyPath := testutil.ClientIdentityFiles(t)
	serverCert, _ := testutil.SelfSignedCert(t, "lxd")
	st := store.NewMemoryStore()
	conn := fake.NewConnector()
	log := logging.Discard()
	reg := hosts.NewRegistry(st, conn, nil, hosts.Options{
		ClientCertRef: certPath, ClientKeyRef: keyPath, RequestTimeout: 2 * time.Second, FailureThreshold: 1,
		DefaultNetwork: "lxdbr0", DefaultStoragePool: "default", CPUOvercommit: 2, MemoryOvercommit: 1, MaxContainersPerGPU: 4,
	}, log)
	ops := operations.NewManager(st, operations.Options{DefaultTimeout: 5 * time.Second}, log)
	im := images.NewManager(reg, ops, nil, images.Options{
		Servers:       map[string]config.ImageServer{"ubuntu": {URL: "https://cloud-images.ubuntu.com/releases/", Protocol: "simplestreams"}},
		DefaultServer: "ubuntu",
	}, log)
	cat, err := profiles.NewCatalog(profiles.DefaultTiers())
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(st, reg, im, ops, cat, Options{GracefulStopTimeout: time.Second, IdmapIsolated: true, PortIsolation: true}, log)

	h := &harness{t: t, st: st, conn: conn, reg: reg, ops: ops, mgr: mgr}
	h.cpu = fake.New("cpu-node")
	h.gpu = fake.New("gpu-node")
	h.gpu.AddNvidiaGPU("RTX 4090")
	conn.Add("https://10.0.0.1:8443", h.cpu)
	conn.Add("https://10.0.0.2:8443", h.gpu)
	for name, ep := range map[string]string{"cpu-node": "https://10.0.0.1:8443", "gpu-node": "https://10.0.0.2:8443"} {
		if _, err := reg.Register(context.Background(), hosts.RegisterRequest{Name: name, Endpoint: ep, ServerCert: serverCert}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = ops.Shutdown(context.Background()) })
	return h
}

func (h *harness) wait(op model.Operation) model.Operation {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := h.ops.Get(context.Background(), op.ID)
		if err != nil {
			h.t.Fatal(err)
		}
		if cur.Status.Final() {
			return cur
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("operation %s did not finish", op.ID)
	return model.Operation{}
}

func (h *harness) mustSucceed(op model.Operation, err error) model.Operation {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
	done := h.wait(op)
	if done.Status != model.OpSucceeded {
		h.t.Fatalf("operation %s %s: %s %s", done.Type, done.Status, done.ErrorCode, done.ErrorMessage)
	}
	return done
}

func (h *harness) create(name, tier string) model.Container {
	h.t.Helper()
	res, err := h.mgr.Create(context.Background(), CreateRequest{Name: name, OwnerID: "cust-1", Tier: tier}, "test")
	if err != nil {
		h.t.Fatalf("create %s: %v", name, err)
	}
	h.mustSucceed(res.Operation, nil)
	c, err := h.st.GetContainer(context.Background(), name)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func code(err error) string {
	if e, ok := errs.As(err); ok {
		return e.Code
	}
	return fmt.Sprint(err)
}

func TestCreatePowerContainer(t *testing.T) {
	h := newHarness(t)
	c := h.create("game-1", "power")
	if c.Status != model.ContainerRunning || c.HostID != "cpu-node" {
		t.Fatalf("container = %+v", c)
	}
	if len(c.IPv4) != 1 || !strings.HasPrefix(c.IPv4[0], "10.0.0.") {
		t.Fatalf("ipv4 = %v", c.IPv4)
	}
	inst, ok := h.cpu.Instance("game-1")
	if !ok {
		t.Fatal("instance not created in LXD")
	}
	want := map[string]string{
		"limits.cpu": "2", "limits.memory": "6144MiB", "security.privileged": "false",
		"security.nesting": "false", "security.idmap.isolated": "true", "user.mea.managed": "true",
		"user.mea.tier": "power", "user.mea.owner": "cust-1",
	}
	for k, v := range want {
		if inst.Config[k] != v {
			t.Errorf("config %s = %q, want %q", k, inst.Config[k], v)
		}
	}
	if inst.Devices["root"]["size"] != "40GiB" || inst.Devices["eth0"]["security.port_isolation"] != "true" {
		t.Errorf("devices = %v", inst.Devices)
	}
	if _, hasGPU := inst.Devices["gpu0"]; hasGPU {
		t.Error("power tier must not get a GPU device")
	}
	if !strings.Contains(inst.Config["cloud-init.user-data"], "NOPASSWD") {
		t.Error("power tier should have sudo inside the container")
	}
	if inst.Type != "container" {
		t.Errorf("type = %s, want container", inst.Type)
	}
}

func TestNewbieHasNoSudoAndIsEphemeral(t *testing.T) {
	h := newHarness(t)
	h.create("newbie-1", "newbie")
	inst, _ := h.cpu.Instance("newbie-1")
	ud := inst.Config["cloud-init.user-data"]
	if strings.Contains(ud, "NOPASSWD") || !strings.Contains(ud, "sudo: false") {
		t.Fatalf("newbie user-data grants sudo:\n%s", ud)
	}
	if !inst.Ephemeral {
		t.Fatal("newbie should be ephemeral")
	}
	// Stopping an ephemeral container removes it in LXD and in Type3.
	op := h.mustSucceed(h.mgr.Stop(context.Background(), "newbie-1", StopRequest{}, "test", ""))
	if op.Result["deleted"] != true {
		t.Fatalf("stop result = %v", op.Result)
	}
	if _, err := h.st.GetContainer(context.Background(), "newbie-1"); !errs.Is(err, errs.KindNotFound) {
		t.Fatalf("record should be gone: %v", err)
	}
}

func TestEleetGetsGPUOnGPUHost(t *testing.T) {
	h := newHarness(t)
	c := h.create("eleet-1", "eleet")
	if c.HostID != "gpu-node" || !c.GPU {
		t.Fatalf("eleet placed on %s gpu=%v", c.HostID, c.GPU)
	}
	inst, _ := h.gpu.Instance("eleet-1")
	dev := inst.Devices["gpu0"]
	if dev["type"] != "gpu" || dev["vendorid"] != "10de" || dev["gputype"] != "physical" {
		t.Fatalf("gpu device = %v", dev)
	}
	if inst.Config["nvidia.runtime"] != "true" || inst.Config["security.privileged"] != "false" {
		t.Fatalf("gpu config = %v", inst.Config)
	}
}

func TestGPUEligibility(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	yes, no := true, false
	_, err := h.mgr.Create(ctx, CreateRequest{Name: "p1", OwnerID: "c", Tier: "power", GPU: &yes}, "t")
	if code(err) != "gpu_not_allowed" || !errs.Is(err, errs.KindForbidden) {
		t.Fatalf("power+gpu: %v", err)
	}
	_, err = h.mgr.Create(ctx, CreateRequest{Name: "n1", OwnerID: "c", Tier: "newbie", GPU: &yes}, "t")
	if code(err) != "gpu_not_allowed" {
		t.Fatalf("newbie+gpu: %v", err)
	}
	_, err = h.mgr.Create(ctx, CreateRequest{Name: "e1", OwnerID: "c", Tier: "eleet", GPU: &no}, "t")
	if code(err) != "gpu_required" {
		t.Fatalf("eleet without gpu: %v", err)
	}
	_, err = h.mgr.Create(ctx, CreateRequest{Name: "e2", OwnerID: "c", Tier: "eleet", HostID: "cpu-node"}, "t")
	if code(err) != "gpu_unavailable" {
		t.Fatalf("eleet on CPU host: %v", err)
	}
}

func TestCreateValidatesAgainstProfile(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	big := 64
	_, err := h.mgr.Create(ctx, CreateRequest{Name: "x1", OwnerID: "c", Tier: "power", Resources: profiles.ResourceRequest{CPU: &big}}, "t")
	if code(err) != "resource_limit_exceeded" {
		t.Fatalf("over-limit cpu: %v", err)
	}
	for _, name := range []string{"UPPER", "-bad", "a", "has_underscore", "x--y", strings.Repeat("a", 64)} {
		if _, err := h.mgr.Create(ctx, CreateRequest{Name: name, OwnerID: "c", Tier: "power"}, "t"); code(err) != "invalid_name" {
			t.Errorf("name %q: %v", name, err)
		}
	}
	if _, err := h.mgr.Create(ctx, CreateRequest{Name: "ok-name", OwnerID: "c", Tier: "platinum"}, "t"); code(err) != "unknown_tier" {
		t.Errorf("unknown tier: %v", err)
	}
	if _, err := h.mgr.Create(ctx, CreateRequest{Name: "ok-name", OwnerID: "c", Tier: "power", SSHKeys: []string{"not a key"}}, "t"); code(err) != "invalid_ssh_keys" {
		t.Errorf("ssh key: %v", err)
	}
}

func TestIdempotentCreate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	req := CreateRequest{Name: "dup-1", OwnerID: "c", Tier: "power", IdempotencyKey: "req-12345678"}
	first, err := h.mgr.Create(ctx, req, "t")
	if err != nil {
		t.Fatal(err)
	}
	h.wait(first.Operation)
	second, err := h.mgr.Create(ctx, req, "t")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Existing || second.Operation.ID != first.Operation.ID {
		t.Fatalf("second create not deduplicated: %+v", second)
	}
	// Same name without key, same spec: still deduplicated.
	req.IdempotencyKey = ""
	third, err := h.mgr.Create(ctx, req, "t")
	if err != nil || !third.Existing {
		t.Fatalf("third: %+v %v", third, err)
	}
	// Same name, different spec: conflict.
	req.Tier = "newbie"
	if _, err := h.mgr.Create(ctx, req, "t"); code(err) != "container_exists" {
		t.Fatalf("different spec: %v", err)
	}
	// Same key, different name: conflict.
	if _, err := h.mgr.Create(ctx, CreateRequest{Name: "dup-2", OwnerID: "c", Tier: "power", IdempotencyKey: "req-12345678"}, "t"); code(err) != "idempotency_key_reused" {
		t.Fatalf("reused key: %v", err)
	}
	if n := h.cpu.CallCount("CreateInstance"); n != 1 {
		t.Fatalf("CreateInstance called %d times, want 1", n)
	}
}

func TestConcurrentDuplicateCreates(t *testing.T) {
	h := newHarness(t)
	h.cpu.SetOpDelay(20 * time.Millisecond)
	var wg sync.WaitGroup
	var created, existing int
	var mu sync.Mutex
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := h.mgr.Create(context.Background(), CreateRequest{Name: "race-1", OwnerID: "c", Tier: "power"}, "t")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			if res.Existing {
				existing++
			} else {
				created++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if created != 1 || existing != 9 {
		t.Fatalf("created=%d existing=%d", created, existing)
	}
	op, _ := h.ops.Active("container", "race-1")
	if op.ID != "" {
		h.wait(op)
	}
	time.Sleep(50 * time.Millisecond)
	if h.cpu.InstanceCount() != 1 || h.cpu.CallCount("CreateInstance") != 1 {
		t.Fatalf("instances=%d creates=%d", h.cpu.InstanceCount(), h.cpu.CallCount("CreateInstance"))
	}
}

func TestCreateAdoptsInstanceFromEarlierAttempt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// First attempt: LXD creates the instance but the start fails.
	h.cpu.FailNext("UpdateInstanceState", errors.New("transient"))
	res, err := h.mgr.Create(ctx, CreateRequest{Name: "retry-1", OwnerID: "c", Tier: "power"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if op := h.wait(res.Operation); op.Status != model.OpFailed {
		t.Fatalf("first attempt should fail, got %s", op.Status)
	}
	rec, _ := h.st.GetContainer(ctx, "retry-1")
	if rec.Status != model.ContainerStopped || rec.Error == "" {
		t.Fatalf("record after partial failure = %s %q", rec.Status, rec.Error)
	}
	// Starting it recovers.
	h.mustSucceed(h.mgr.Start(ctx, "retry-1", "t", ""))
	rec, _ = h.st.GetContainer(ctx, "retry-1")
	if rec.Status != model.ContainerRunning {
		t.Fatalf("status after start = %s", rec.Status)
	}
	if h.cpu.CallCount("CreateInstance") != 1 {
		t.Fatal("must not create twice")
	}
}

func TestCreateRefusesForeignInstanceWithSameName(t *testing.T) {
	h := newHarness(t)
	h.cpu.InjectInstance(api.Instance{Name: "taken-1", Config: map[string]string{}})
	res, err := h.mgr.Create(context.Background(), CreateRequest{Name: "taken-1", OwnerID: "c", Tier: "power", HostID: "cpu-node"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	op := h.wait(res.Operation)
	if op.Status != model.OpFailed || op.ErrorCode != "instance_name_taken" {
		t.Fatalf("op = %s %s", op.Status, op.ErrorCode)
	}
}

func TestLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.create("life-1", "power")

	h.mustSucceed(h.mgr.Stop(ctx, "life-1", StopRequest{}, "t", ""))
	if v, _ := h.mgr.Get(ctx, "life-1", true); v.Status != model.ContainerStopped || len(v.IPv4) != 0 {
		t.Fatalf("after stop: %s %v", v.Status, v.IPv4)
	}
	// Stopping again is harmless.
	h.mustSucceed(h.mgr.Stop(ctx, "life-1", StopRequest{}, "t", ""))

	h.mustSucceed(h.mgr.Start(ctx, "life-1", "t", ""))
	h.mustSucceed(h.mgr.Start(ctx, "life-1", "t", "")) // already running
	if v, _ := h.mgr.Get(ctx, "life-1", true); v.Status != model.ContainerRunning {
		t.Fatalf("after start: %s", v.Status)
	}
	h.mustSucceed(h.mgr.Restart(ctx, "life-1", StopRequest{}, "t", ""))

	if _, err := h.mgr.Delete(ctx, "life-1", DeleteRequest{}, "t", ""); code(err) != "container_running" {
		t.Fatalf("delete running without force: %v", err)
	}
	if _, err := h.mgr.Delete(ctx, "life-1", DeleteRequest{Force: true}, "t", ""); code(err) != "reason_required" {
		t.Fatalf("force delete without reason: %v", err)
	}
	h.mustSucceed(h.mgr.Delete(ctx, "life-1", DeleteRequest{Force: true, Reason: "test teardown"}, "t", ""))
	if _, ok := h.cpu.Instance("life-1"); ok {
		t.Fatal("instance still exists in LXD")
	}
	if _, err := h.mgr.Get(ctx, "life-1", false); !errs.Is(err, errs.KindNotFound) {
		t.Fatalf("record still exists: %v", err)
	}
}

func TestForcedStopSafeguards(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.create("hung-1", "power")
	inst, _ := h.cpu.Instance("hung-1")
	inst.Config["volatile.fake.ignore_sigpwr"] = "true"
	h.cpu.InjectInstance(inst)

	op, err := h.mgr.Stop(ctx, "hung-1", StopRequest{}, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	if done := h.wait(op); done.Status != model.OpFailed || done.ErrorCode != "graceful_stop_failed" {
		t.Fatalf("graceful stop of hung container = %s %s", done.Status, done.ErrorCode)
	}
	if _, err := h.mgr.Stop(ctx, "hung-1", StopRequest{Force: true}, "t", ""); code(err) != "reason_required" {
		t.Fatalf("force without reason: %v", err)
	}
	done := h.mustSucceed(h.mgr.Stop(ctx, "hung-1", StopRequest{Force: true, Reason: "unresponsive"}, "t", ""))
	if done.Result["forced"] != true {
		t.Fatalf("result = %v", done.Result)
	}
}

func TestUpdateResources(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.create("res-1", "power")
	cpu, mem, disk := 4, 8192, 60
	h.mustSucceed(h.mgr.UpdateResources(ctx, "res-1", profiles.ResourceRequest{CPU: &cpu, MemoryMiB: &mem, DiskGiB: &disk}, "t", ""))
	inst, _ := h.cpu.Instance("res-1")
	if inst.Config["limits.cpu"] != "4" || inst.Config["limits.memory"] != "8192MiB" || inst.Devices["root"]["size"] != "60GiB" {
		t.Fatalf("limits not applied: %v %v", inst.Config, inst.Devices["root"])
	}
	rec, _ := h.st.GetContainer(ctx, "res-1")
	if rec.Resources != (model.Resources{CPU: 4, MemoryMiB: 8192, DiskGiB: 60}) {
		t.Fatalf("record = %+v", rec.Resources)
	}
	over := 16
	if _, err := h.mgr.UpdateResources(ctx, "res-1", profiles.ResourceRequest{CPU: &over}, "t", ""); code(err) != "resource_limit_exceeded" {
		t.Fatalf("over tier max: %v", err)
	}
	small := 20
	if _, err := h.mgr.UpdateResources(ctx, "res-1", profiles.ResourceRequest{DiskGiB: &small}, "t", ""); code(err) != "disk_shrink_unsupported" {
		t.Fatalf("shrink: %v", err)
	}
}

func TestSnapshots(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.create("snap-1", "power")
	h.mustSucceed(h.mgr.CreateSnapshot(ctx, "snap-1", SnapshotRequest{Name: "before"}, "t", ""))
	cpu := 4
	h.mustSucceed(h.mgr.UpdateResources(ctx, "snap-1", profiles.ResourceRequest{CPU: &cpu}, "t", ""))
	snaps, err := h.mgr.ListSnapshots(ctx, "snap-1")
	if err != nil || len(snaps) != 1 || snaps[0].Name != "before" {
		t.Fatalf("snapshots = %v %v", snaps, err)
	}
	h.mustSucceed(h.mgr.RestoreSnapshot(ctx, "snap-1", "before", "t", ""))
	rec, _ := h.st.GetContainer(ctx, "snap-1")
	if rec.Resources.CPU != 2 {
		t.Fatalf("restore should revert cpu to 2, got %d", rec.Resources.CPU)
	}
	if op, _ := h.mgr.RestoreSnapshot(ctx, "snap-1", "nope", "t", ""); h.wait(op).ErrorCode != "snapshot_not_found" {
		t.Fatal("restoring a missing snapshot should fail with snapshot_not_found")
	}
	h.mustSucceed(h.mgr.DeleteSnapshot(ctx, "snap-1", "before", "t"))
	if snaps, _ := h.mgr.ListSnapshots(ctx, "snap-1"); len(snaps) != 0 {
		t.Fatalf("snapshot not deleted: %v", snaps)
	}
}

func TestOneOperationPerContainer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.create("busy-1", "power")
	h.cpu.BlockOps()
	stop, err := h.mgr.Stop(ctx, "busy-1", StopRequest{}, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.mgr.Stop(ctx, "busy-1", StopRequest{}, "t", "")
	if err != nil || again.ID != stop.ID {
		t.Fatalf("repeated stop should return the running op: %v %v", again.ID, err)
	}
	if _, err := h.mgr.Start(ctx, "busy-1", "t", ""); code(err) != "operation_in_progress" {
		t.Fatalf("conflicting start: %v", err)
	}
	h.cpu.UnblockOps()
	h.wait(stop)
}

func TestOperationTimeout(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.create("slow-1", "power")
	h.mgr.opt.OperationTimeout = 50 * time.Millisecond
	h.cpu.BlockOps()
	defer h.cpu.UnblockOps()
	op, err := h.mgr.Stop(ctx, "slow-1", StopRequest{}, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	done := h.wait(op)
	if done.Status != model.OpFailed || done.ErrorCode != "operation_timeout" {
		t.Fatalf("op = %s %s %s", done.Status, done.ErrorCode, done.ErrorMessage)
	}
}

func TestHostDisconnection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.create("hd-1", "power")
	h.cpu.SetDown(true)

	v, err := h.mgr.Get(ctx, "hd-1", true)
	if err != nil || !v.Stale || v.Status != model.ContainerRunning {
		t.Fatalf("get on down host: %+v %v", v, err)
	}
	op, err := h.mgr.Stop(ctx, "hd-1", StopRequest{}, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	if done := h.wait(op); done.Status != model.OpFailed || done.ErrorCode != "host_unavailable" {
		t.Fatalf("stop on down host = %s %s", done.Status, done.ErrorCode)
	}
	if host, _ := h.reg.Get(ctx, "cpu-node"); host.Status != model.HostOffline {
		t.Fatalf("host status = %s", host.Status)
	}
	// Automatic placement skips the offline host.
	if _, err := h.mgr.Create(ctx, CreateRequest{Name: "hd-2", OwnerID: "c", Tier: "power", HostID: "cpu-node"}, "t"); code(err) != "host_unavailable" {
		t.Fatalf("explicit create on offline host: %v", err)
	}

	h.cpu.SetDown(false)
	if _, err := h.reg.Health(ctx, "cpu-node"); err != nil {
		t.Fatal(err)
	}
	h.mustSucceed(h.mgr.Stop(ctx, "hd-1", StopRequest{}, "t", ""))
}

func TestReconcile(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.create("rec-gone", "power")
	h.create("rec-drift", "power")

	h.cpu.RemoveInstance("rec-gone")
	inst, _ := h.cpu.Instance("rec-drift")
	inst.Status, inst.StatusCode = "Stopped", api.Stopped
	h.cpu.InjectInstance(inst)
	h.cpu.InjectInstance(api.Instance{Name: "orphan-1", Status: "Running", StatusCode: api.Running, Config: map[string]string{
		"user.mea.managed": "true", "user.mea.owner": "cust-9", "user.mea.tier": "power", "limits.cpu": "2", "limits.memory": "6GiB",
	}, Devices: map[string]map[string]string{"root": {"type": "disk", "path": "/", "pool": "default", "size": "40GiB"}}})
	h.cpu.InjectInstance(api.Instance{Name: "not-ours", Config: map[string]string{}})

	rep, err := h.mgr.Reconcile(ctx, "cpu-node")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Missing) != 1 || rep.Missing[0] != "rec-gone" {
		t.Errorf("missing = %v", rep.Missing)
	}
	if len(rep.Updated) != 1 || rep.Updated[0] != "rec-drift" {
		t.Errorf("updated = %v", rep.Updated)
	}
	if len(rep.Adopted) != 1 || rep.Adopted[0] != "orphan-1" {
		t.Errorf("adopted = %v", rep.Adopted)
	}
	orphan, err := h.st.GetContainer(ctx, "orphan-1")
	if err != nil || !orphan.Orphaned || orphan.OwnerID != "cust-9" || orphan.Resources != (model.Resources{CPU: 2, MemoryMiB: 6144, DiskGiB: 40}) {
		t.Errorf("orphan = %+v %v", orphan, err)
	}
	if _, err := h.st.GetContainer(ctx, "not-ours"); err == nil {
		t.Error("unmanaged instance must not be adopted")
	}
	// Deleting a missing container just drops the record.
	h.mustSucceed(h.mgr.Delete(ctx, "rec-gone", DeleteRequest{}, "t", ""))
}

func TestParseMiB(t *testing.T) {
	for in, want := range map[string]int{"6144MiB": 6144, "6GiB": 6144, "1073741824": 1024, "512MB": 488, "1TiB": 1 << 20} {
		if got, ok := ParseMiB(in); !ok || got != want {
			t.Errorf("ParseMiB(%q) = %d %v, want %d", in, got, ok, want)
		}
	}
	if _, ok := ParseMiB("50%"); ok {
		t.Error("percentages are not supported")
	}
}

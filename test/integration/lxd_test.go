//go:build integration

// Package integration runs Type3 against a real LXD daemon. It is skipped
// unless TYPE3_IT_LXD_ENDPOINT is set. See docs/testing.md.
//
//	TYPE3_IT_LXD_ENDPOINT   https://10.0.0.5:8443 or unix:// (local socket)
//	TYPE3_IT_SERVER_CERT    path to the LXD server certificate (https only)
//	TYPE3_IT_CLIENT_CERT    path to a client certificate trusted by LXD (https only)
//	TYPE3_IT_CLIENT_KEY     path to its key (https only)
//	TYPE3_IT_IMAGE          image alias on the ubuntu server (default 24.04)
//	TYPE3_IT_GPU=1          also create an Eleet container (needs an NVIDIA host)
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/config"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/containers"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/hosts"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/images"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/logging"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/lxdclient"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/operations"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/profiles"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
)

type stack struct {
	reg *hosts.Registry
	ops *operations.Manager
	cm  *containers.Manager
}

func setup(t *testing.T) stack {
	endpoint := os.Getenv("TYPE3_IT_LXD_ENDPOINT")
	if endpoint == "" {
		t.Skip("TYPE3_IT_LXD_ENDPOINT not set; skipping real LXD integration tests")
	}
	log := logging.New(os.Stderr, "debug", "text")
	st := store.NewMemoryStore()
	reg := hosts.NewRegistry(st, lxdclient.TLSConnector{}, nil, hosts.Options{
		ClientCertRef: os.Getenv("TYPE3_IT_CLIENT_CERT"), ClientKeyRef: os.Getenv("TYPE3_IT_CLIENT_KEY"),
		RequestTimeout: 30 * time.Second, FailureThreshold: 3, AllowUnixSocket: true,
		DefaultNetwork: envOr("TYPE3_IT_NETWORK", "lxdbr0"), DefaultStoragePool: envOr("TYPE3_IT_POOL", "default"),
		CPUOvercommit: 8, MemoryOvercommit: 2,
	}, log)
	ops := operations.NewManager(st, operations.Options{DefaultTimeout: 15 * time.Minute}, log)
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	im := images.NewManager(reg, ops, images.SimpleStreamsLister{}, images.Options{Servers: cfg.ImageServers, DefaultServer: "ubuntu"}, log)
	cat, _ := profiles.NewCatalog(profiles.DefaultTiers())
	cm := containers.NewManager(st, reg, im, ops, cat, containers.Options{GracefulStopTimeout: 60 * time.Second,
		IdmapIsolated: os.Getenv("TYPE3_IT_IDMAP_ISOLATED") != "0", PortIsolation: true}, log)

	var cert string
	if p := os.Getenv("TYPE3_IT_SERVER_CERT"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		cert = string(b)
	}
	if _, err := reg.Register(context.Background(), hosts.RegisterRequest{Name: "it-host", Endpoint: endpoint, ServerCert: cert}); err != nil {
		t.Fatalf("register real LXD host: %v", err)
	}
	t.Cleanup(func() { _ = ops.Shutdown(context.Background()); reg.Close() })
	return stack{reg: reg, ops: ops, cm: cm}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// waiter returns a function that waits for an operation to succeed, so it
// can wrap manager calls directly: w(s.cm.Stop(...)).
func waiter(t *testing.T, s stack) func(model.Operation, error) model.Operation {
	return func(op model.Operation, err error) model.Operation { return wait(t, s, op, err) }
}

func wait(t *testing.T, s stack, op model.Operation, err error) model.Operation {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Minute)
	for time.Now().Before(deadline) {
		cur, _ := s.ops.Get(context.Background(), op.ID)
		if cur.Status.Final() {
			if cur.Status != model.OpSucceeded {
				t.Fatalf("%s failed: %s %s", cur.Type, cur.ErrorCode, cur.ErrorMessage)
			}
			return cur
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s timed out", op.Type)
	return op
}

func TestRealHostDiscovery(t *testing.T) {
	s := setup(t)
	ctx := context.Background()
	h, _ := s.reg.Get(ctx, "it-host")
	if h.Status != model.HostOnline {
		t.Fatalf("host status %s: %s", h.Status, h.StatusReason)
	}
	res, err := s.reg.Resources(ctx, "it-host")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cpu threads=%d memory=%d MiB pools=%d gpus=%d (nvidia usable: %v)",
		res.CPU.Threads, res.Memory.TotalBytes>>20, len(res.Storage), res.GPU.Total, res.GPUCapable)
	if res.CPU.Threads == 0 || res.Memory.TotalBytes == 0 {
		t.Fatal("empty resources")
	}
}

func TestRealContainerLifecycle(t *testing.T) {
	s := setup(t)
	ctx := context.Background()
	name := "type3-it-" + time.Now().UTC().Format("150405")
	img := &model.ImageRef{Server: "ubuntu", Alias: envOr("TYPE3_IT_IMAGE", "24.04")}

	res, err := s.cm.Create(ctx, containers.CreateRequest{Name: name, OwnerID: "integration", Tier: "power", Image: img}, "it")
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, res.Operation, nil)
	t.Cleanup(func() {
		op, err := s.cm.Delete(context.Background(), name, containers.DeleteRequest{Force: true, Reason: "integration cleanup"}, "it", "")
		if err == nil {
			wait(t, s, op, nil)
		}
	})

	w := waiter(t, s)
	v, err := s.cm.Get(ctx, name, true)
	if err != nil || v.Status != model.ContainerRunning {
		t.Fatalf("after create: %+v %v", v, err)
	}
	// Retry-safe: the same request must not create another instance.
	again, err := s.cm.Create(ctx, containers.CreateRequest{Name: name, OwnerID: "integration", Tier: "power", Image: img}, "it")
	if err != nil || !again.Existing {
		t.Fatalf("duplicate create: %+v %v", again, err)
	}

	cpu := 3
	w(s.cm.UpdateResources(ctx, name, profiles.ResourceRequest{CPU: &cpu}, "it", ""))
	w(s.cm.Stop(ctx, name, containers.StopRequest{}, "it", ""))
	if v, _ := s.cm.Get(ctx, name, true); v.Status != model.ContainerStopped {
		t.Fatalf("after stop: %s", v.Status)
	}
	w(s.cm.Start(ctx, name, "it", ""))
	w(s.cm.Restart(ctx, name, containers.StopRequest{}, "it", ""))
	w(s.cm.CreateSnapshot(ctx, name, containers.SnapshotRequest{Name: "it-snap"}, "it", ""))
	w(s.cm.RestoreSnapshot(ctx, name, "it-snap", "it", ""))
	if v, _ := s.cm.Get(ctx, name, true); v.Status != model.ContainerRunning {
		t.Logf("status after restore: %s (restoring a running container may leave it stopped)", v.Status)
	}
	rep, err := s.cm.Reconcile(ctx, "it-host")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reconcile: %+v", rep)
}

func TestRealGPUContainer(t *testing.T) {
	if os.Getenv("TYPE3_IT_GPU") != "1" {
		t.Skip("TYPE3_IT_GPU!=1; skipping GPU test")
	}
	s := setup(t)
	ctx := context.Background()
	name := "type3-it-gpu-" + time.Now().UTC().Format("150405")
	res, err := s.cm.Create(ctx, containers.CreateRequest{Name: name, OwnerID: "integration", Tier: "eleet"}, "it")
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, res.Operation, nil)
	t.Cleanup(func() {
		op, err := s.cm.Delete(context.Background(), name, containers.DeleteRequest{Force: true, Reason: "integration cleanup"}, "it", "")
		if err == nil {
			wait(t, s, op, nil)
		}
	})
	v, _ := s.cm.Get(ctx, name, true)
	if !v.GPU || v.Status != model.ContainerRunning {
		t.Fatalf("gpu container: %+v", v)
	}
	t.Log("GPU container running; verify `lxc exec " + name + " -- nvidia-smi` manually")
}

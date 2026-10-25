package hosts

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/logging"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/lxdclient/fake"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/testutil"
)

type env struct {
	reg  *Registry
	st   *store.FileStore
	conn *fake.Connector
	cert string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	certPath, keyPath := testutil.ClientIdentityFiles(t)
	serverCert, _ := testutil.SelfSignedCert(t, "lxd-server")
	st := store.NewMemoryStore()
	conn := fake.NewConnector()
	reg := NewRegistry(st, conn, nil, Options{
		ClientCertRef: certPath, ClientKeyRef: keyPath,
		RequestTimeout: 2 * time.Second, FailureThreshold: 2,
		DefaultNetwork: "lxdbr0", DefaultStoragePool: "default",
		CPUOvercommit: 1, MemoryOvercommit: 1, MaxContainersPerGPU: 2,
		LookupIP: func(_ context.Context, host string) ([]net.IP, error) {
			switch host {
			case "lxd.internal":
				return []net.IP{net.ParseIP("10.1.2.3")}, nil
			case "lxd.example.com":
				return []net.IP{net.ParseIP("93.184.216.34")}, nil
			}
			return nil, errors.New("no such host")
		},
	}, logging.Discard())
	return &env{reg: reg, st: st, conn: conn, cert: serverCert}
}

func (e *env) add(name, endpoint string) *fake.Server {
	s := fake.New(name)
	e.conn.Add(endpoint, s)
	return s
}

func (e *env) register(t *testing.T, name, endpoint string) model.Host {
	t.Helper()
	h, err := e.reg.Register(context.Background(), RegisterRequest{Name: name, Endpoint: endpoint, ServerCert: e.cert})
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return h
}

func TestRegisterDiscoversHost(t *testing.T) {
	e := newEnv(t)
	srv := e.add("node1", "https://10.0.0.1:8443")
	srv.AddNvidiaGPU("RTX 4090")

	h := e.register(t, "node1", "https://10.0.0.1:8443")
	if h.Status != model.HostOnline {
		t.Fatalf("status = %s (%s), want online", h.Status, h.StatusReason)
	}
	if h.ServerCertFingerprint == "" || h.ServerVersion != "6.5" {
		t.Fatalf("server info not recorded: %+v", h)
	}
	if h.Capacity.CPUThreads != 8 || h.Capacity.NvidiaGPUs != 1 {
		t.Fatalf("capacity = %+v", h.Capacity)
	}
	got, err := e.reg.Get(context.Background(), "node1")
	if err != nil || got.Endpoint != "https://10.0.0.1:8443" {
		t.Fatalf("not persisted: %v %+v", err, got)
	}
}

func TestRegisterValidation(t *testing.T) {
	e := newEnv(t)
	e.add("n", "https://10.0.0.1:8443")
	ctx := context.Background()
	cases := []struct {
		name string
		req  RegisterRequest
		code string
	}{
		{"bad name", RegisterRequest{Name: "Bad_Name", Endpoint: "https://10.0.0.1:8443", ServerCert: e.cert}, "invalid_name"},
		{"http scheme", RegisterRequest{Name: "n1", Endpoint: "http://10.0.0.1:8443", ServerCert: e.cert}, "invalid_endpoint"},
		{"credentials in url", RegisterRequest{Name: "n1", Endpoint: "https://u:p@10.0.0.1:8443", ServerCert: e.cert}, "invalid_endpoint"},
		{"public ip", RegisterRequest{Name: "n1", Endpoint: "https://8.8.8.8:8443", ServerCert: e.cert}, "public_endpoint_rejected"},
		{"public hostname", RegisterRequest{Name: "n1", Endpoint: "https://lxd.example.com:8443", ServerCert: e.cert}, "public_endpoint_rejected"},
		{"missing cert", RegisterRequest{Name: "n1", Endpoint: "https://10.0.0.1:8443"}, "server_cert_required"},
		{"garbage cert", RegisterRequest{Name: "n1", Endpoint: "https://10.0.0.1:8443", ServerCert: "nope"}, "invalid_server_cert"},
		{"inline key", RegisterRequest{Name: "n1", Endpoint: "https://10.0.0.1:8443", ServerCert: e.cert, ClientCertRef: "x", ClientKeyRef: "-----BEGIN EC PRIVATE KEY-----"}, "inline_secret_rejected"},
		{"unknown provider", RegisterRequest{Name: "n1", Endpoint: "https://10.0.0.1:8443", ServerCert: e.cert, Provider: "ec2"}, "unknown_provider"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.reg.Register(ctx, tc.req)
			ae, ok := errs.As(err)
			if !ok || ae.Code != tc.code {
				t.Fatalf("err = %v, want code %s", err, tc.code)
			}
		})
	}
}

func TestRegisterDuplicate(t *testing.T) {
	e := newEnv(t)
	e.add("n", "https://10.0.0.1:8443")
	e.register(t, "node1", "https://10.0.0.1:8443")
	_, err := e.reg.Register(context.Background(), RegisterRequest{Name: "node1", Endpoint: "https://10.0.0.2:8443", ServerCert: e.cert})
	if !errs.Is(err, errs.KindConflict) {
		t.Fatalf("duplicate name: err = %v", err)
	}
	_, err = e.reg.Register(context.Background(), RegisterRequest{Name: "node2", Endpoint: "https://10.0.0.1:8443", ServerCert: e.cert})
	if ae, _ := errs.As(err); ae == nil || ae.Code != "endpoint_registered" {
		t.Fatalf("duplicate endpoint: err = %v", err)
	}
}

func TestRegisterUnreachable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, err := e.reg.Register(ctx, RegisterRequest{Name: "node1", Endpoint: "https://10.0.0.9:8443", ServerCert: e.cert})
	if !errs.Is(err, errs.KindUnavailable) {
		t.Fatalf("err = %v, want unavailable", err)
	}
	h, err := e.reg.Register(ctx, RegisterRequest{Name: "node1", Endpoint: "https://10.0.0.9:8443", ServerCert: e.cert, AllowOffline: true})
	if err != nil || h.Status != model.HostOffline {
		t.Fatalf("allow_offline: %v %s", err, h.Status)
	}
}

func TestHostDisconnectAndReconnect(t *testing.T) {
	e := newEnv(t)
	srv := e.add("n", "https://10.0.0.1:8443")
	e.register(t, "node1", "https://10.0.0.1:8443")
	ctx := context.Background()

	var reconnects atomic.Int32
	done := make(chan struct{}, 1)
	e.reg.OnReconnect(func(id string) {
		reconnects.Add(1)
		done <- struct{}{}
	})

	srv.SetDown(true)
	rep, _ := e.reg.Health(ctx, "node1")
	if rep.Status != model.HostDegraded {
		t.Fatalf("after 1 failure status = %s, want degraded", rep.Status)
	}
	rep, _ = e.reg.Health(ctx, "node1")
	if rep.Status != model.HostOffline || rep.ConsecutiveFailures != 2 {
		t.Fatalf("after 2 failures = %+v, want offline", rep)
	}
	if _, _, err := e.reg.Client(ctx, "node1"); !errs.Is(err, errs.KindUnavailable) {
		t.Fatalf("Client on down host: %v", err)
	}

	srv.SetDown(false)
	before := e.conn.ConnectCount("https://10.0.0.1:8443")
	rep, _ = e.reg.Health(ctx, "node1")
	if rep.Status != model.HostOnline {
		t.Fatalf("after recovery status = %s", rep.Status)
	}
	if e.conn.ConnectCount("https://10.0.0.1:8443") <= before {
		t.Fatal("expected a fresh connection after reconnect")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect callback not called")
	}
	if reconnects.Load() != 1 {
		t.Fatalf("reconnects = %d", reconnects.Load())
	}
}

func TestHealthDegradedWhenNetworkMissing(t *testing.T) {
	e := newEnv(t)
	srv := e.add("n", "https://10.0.0.1:8443")
	srv.Networks = []string{"othernet"}
	h := e.register(t, "node1", "https://10.0.0.1:8443")
	if h.Status != model.HostDegraded {
		t.Fatalf("status = %s, want degraded", h.Status)
	}
}

func TestResourcesInventory(t *testing.T) {
	e := newEnv(t)
	srv := e.add("n", "https://10.0.0.1:8443")
	srv.AddNvidiaGPU("RTX A4000")
	e.register(t, "node1", "https://10.0.0.1:8443")
	ctx := context.Background()
	_ = e.st.PutContainer(ctx, model.Container{ID: "c1", HostID: "node1", Resources: model.Resources{CPU: 2, MemoryMiB: 1024, DiskGiB: 10}, GPU: true})

	res, err := e.reg.Resources(ctx, "node1")
	if err != nil {
		t.Fatal(err)
	}
	if res.CPU.Threads != 8 || res.Memory.TotalBytes != 32<<30 {
		t.Fatalf("cpu/mem = %+v %+v", res.CPU, res.Memory)
	}
	if len(res.Storage) != 1 || res.Storage[0].TotalBytes != 500<<30 {
		t.Fatalf("storage = %+v", res.Storage)
	}
	if !res.GPUCapable || res.GPU.Nvidia != 1 || res.GPU.Cards[0].Nvidia == nil || res.GPU.Cards[0].Nvidia.Model != "RTX A4000" {
		t.Fatalf("gpu = %+v", res.GPU)
	}
	if res.Allocated.CPU != 2 || res.Allocated.GPUContainers != 1 || res.Allocatable.GPUContainers != 2 {
		t.Fatalf("allocation = %+v / %+v", res.Allocated, res.Allocatable)
	}
}

func TestUnregisterSafety(t *testing.T) {
	e := newEnv(t)
	e.add("n", "https://10.0.0.1:8443")
	e.register(t, "node1", "https://10.0.0.1:8443")
	ctx := context.Background()
	_ = e.st.PutContainer(ctx, model.Container{ID: "c1", HostID: "node1"})

	if _, err := e.reg.Unregister(ctx, "node1", false); !errs.Is(err, errs.KindConflict) {
		t.Fatalf("unregister with containers: %v", err)
	}
	n, err := e.reg.Unregister(ctx, "node1", true)
	if err != nil || n != 1 {
		t.Fatalf("force unregister: %d %v", n, err)
	}
	if _, err := e.reg.Get(ctx, "node1"); !errs.Is(err, errs.KindNotFound) {
		t.Fatalf("host still present: %v", err)
	}
}

func TestSelectPlacement(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add("a", "https://10.0.0.1:8443")
	gpuSrv := e.add("b", "https://10.0.0.2:8443")
	gpuSrv.AddNvidiaGPU("RTX 4090")
	small := e.add("c", "https://10.0.0.3:8443")
	small.Resources.Memory.Total = 2 << 30
	e.register(t, "cpu-node", "https://10.0.0.1:8443")
	e.register(t, "gpu-node", "https://10.0.0.2:8443")
	e.register(t, "small-node", "https://10.0.0.3:8443")

	h, err := e.reg.Select(ctx, PlacementRequest{Resources: model.Resources{CPU: 1, MemoryMiB: 1024}})
	if err != nil || h.ID != "cpu-node" {
		t.Fatalf("non-GPU placement = %s %v, want cpu-node (keeps GPU host free)", h.ID, err)
	}
	h, err = e.reg.Select(ctx, PlacementRequest{Resources: model.Resources{CPU: 4, MemoryMiB: 12288}, GPU: true})
	if err != nil || h.ID != "gpu-node" {
		t.Fatalf("GPU placement = %s %v", h.ID, err)
	}

	// Fill the GPU host's sharing slots (MaxContainersPerGPU = 2).
	for _, id := range []string{"g1", "g2"} {
		_ = e.st.PutContainer(ctx, model.Container{ID: id, HostID: "gpu-node", GPU: true, Resources: model.Resources{CPU: 1, MemoryMiB: 512}})
	}
	if _, err := e.reg.Select(ctx, PlacementRequest{Resources: model.Resources{CPU: 1, MemoryMiB: 512}, GPU: true}); !errs.Is(err, errs.KindConflict) {
		t.Fatalf("expected no GPU capacity, got %v", err)
	}
	if _, err := e.reg.CheckPlacement(ctx, "small-node", PlacementRequest{Resources: model.Resources{CPU: 1, MemoryMiB: 4096}}); !errs.Is(err, errs.KindConflict) {
		t.Fatalf("explicit placement over memory: %v", err)
	}
	if _, err := e.reg.CheckPlacement(ctx, "cpu-node", PlacementRequest{Resources: model.Resources{CPU: 1, MemoryMiB: 512}, GPU: true}); !errs.Is(err, errs.KindPrecondition) {
		t.Fatalf("explicit GPU placement on CPU host: %v", err)
	}
}

func TestIsPrivateIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"10.0.0.1": true, "192.168.1.1": true, "172.16.0.1": true, "127.0.0.1": true,
		"100.64.1.1": true, "fd00::1": true, "fe80::1": true, "8.8.8.8": false, "2001:4860::1": false,
	} {
		if got := IsPrivateIP(net.ParseIP(ip)); got != want {
			t.Errorf("IsPrivateIP(%s) = %v", ip, got)
		}
	}
}

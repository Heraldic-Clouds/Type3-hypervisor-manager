package containers

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/agent"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
)

func withAgent(h *harness) *[]string {
	calls := &[]string{}
	a := &agent.Agent{GuestUID: 1000, HomeRoot: "/mnt/mea-efs/users",
		MkdirAll: func(string, os.FileMode) error { return nil },
		Chown:    func(string, int, int) error { return nil },
		Chmod:    func(string, os.FileMode) error { return nil },
	}
	a.Run = func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
		argv := name + " " + strings.Join(args, " ")
		*calls = append(*calls, argv)
		if strings.Contains(argv, "volatile.idmap.base") {
			return []byte("1065536\n"), nil
		}
		return nil, nil
	}
	h.mgr.opt.Agent = a
	h.mgr.opt.RDPListenAddress = "10.84.0.20"
	return calls
}

func TestPaidDesktopDevices(t *testing.T) {
	h := newHarness(t)
	withAgent(h)
	ctx := context.Background()
	for _, tc := range []struct {
		name, tier string
		port       int
	}{{"mea-power-a", "power", 40001}, {"mea-eleet-a", "eleet", 40002}} {
		res, err := h.mgr.Create(ctx, CreateRequest{Name: tc.name, OwnerID: "8664cac9-34fe-494e-a33a-afa46a195b57", Tier: tc.tier, RDPPort: tc.port, PersistentHome: true}, "mea")
		if err != nil {
			t.Fatal(err)
		}
		h.mustSucceed(res.Operation, nil)
		srv := h.cpu
		if tc.tier == "eleet" {
			srv = h.gpu
		}
		inst, ok := srv.Instance(tc.name)
		if !ok {
			t.Fatalf("%s not created", tc.name)
		}
		rdp := inst.Devices["rdp"]
		if rdp["type"] != "proxy" || rdp["connect"] != "tcp:127.0.0.1:3389" || !strings.HasSuffix(rdp["listen"], ":"+strconv.Itoa(tc.port)) || !strings.HasPrefix(rdp["listen"], "tcp:10.84.0.20:") {
			t.Fatalf("%s rdp device %v", tc.name, rdp)
		}
		home := inst.Devices["home"]
		if home["type"] != "disk" || home["path"] != "/home/mea" || home["source"] != "/mnt/mea-efs/users/8664cac9-34fe-494e-a33a-afa46a195b57" {
			t.Fatalf("%s home device %v", tc.name, home)
		}
		_, hasGPU := inst.Devices["gpu0"]
		if hasGPU != (tc.tier == "eleet") {
			t.Fatalf("%s gpu device present=%v", tc.name, hasGPU)
		}
		rec, _ := h.st.GetContainer(ctx, tc.name)
		if !rec.HomeReady || rec.RDPPort != tc.port {
			t.Fatalf("record %+v", rec)
		}
	}
	// Restarting must not attach a second home device or fail.
	h.mustSucceed(h.mgr.Stop(ctx, "mea-power-a", StopRequest{}, "mea", ""))
	h.mustSucceed(h.mgr.Start(ctx, "mea-power-a", "mea", ""))
	if c, _ := h.st.GetContainer(ctx, "mea-power-a"); c.Status != model.ContainerRunning {
		t.Fatalf("restart status %s", c.Status)
	}
}

func TestDesktopRequestValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.mgr.Create(ctx, CreateRequest{Name: "x-1", OwnerID: "o1", Tier: "power", RDPPort: 40001}, "mea"); code(err) != "invalid_rdp_port" {
		t.Fatalf("rdp without listen address: %v", err)
	}
	if _, err := h.mgr.Create(ctx, CreateRequest{Name: "x-2", OwnerID: "o1", Tier: "power", PersistentHome: true}, "mea"); code(err) != "persistent_home_unavailable" {
		t.Fatalf("home without agent: %v", err)
	}
	withAgent(h)
	if _, err := h.mgr.Create(ctx, CreateRequest{Name: "x-3", OwnerID: "o1", Tier: "power", RDPPort: 3389}, "mea"); code(err) != "invalid_rdp_port" {
		t.Fatalf("privileged port accepted: %v", err)
	}
	gpu := true
	if _, err := h.mgr.Create(ctx, CreateRequest{Name: "x-4", OwnerID: "o1", Tier: "power", GPU: &gpu}, "mea"); code(err) != "gpu_not_allowed" {
		t.Fatalf("power GPU accepted: %v", err)
	}
	if err := h.mgr.SetPassword(ctx, "missing", "abcdefghijklmnopqrstuvwxyz"); err == nil {
		t.Fatal("password set on unknown container")
	}
}

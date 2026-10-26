package profiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/canonical/lxd/shared/api"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
)

func TestDefaultTiers(t *testing.T) {
	c, err := NewCatalog(DefaultTiers())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		cpu, mem  int
		gpu, root bool
	}{
		"newbie": {1, 1024, false, false},
		"power":  {2, 6144, false, true},
		"eleet":  {4, 12288, true, true},
	}
	for name, w := range want {
		tier, err := c.Get(strings.ToUpper(name))
		if err != nil {
			t.Fatal(err)
		}
		if tier.Defaults.CPU != w.cpu || tier.Defaults.MemoryMiB != w.mem || tier.GPU != w.gpu || tier.RootAccess != w.root {
			t.Errorf("%s = %+v", name, tier)
		}
	}
	if _, err := c.Get("gold"); err == nil {
		t.Error("unknown tier accepted")
	}
}

func TestResolveBounds(t *testing.T) {
	c, _ := NewCatalog(DefaultTiers())
	power, _ := c.Get("power")
	r, err := power.Resolve(ResourceRequest{})
	if err != nil || r != power.Defaults {
		t.Fatalf("defaults: %+v %v", r, err)
	}
	four := 4
	if r, err := power.Resolve(ResourceRequest{CPU: &four}); err != nil || r.CPU != 4 {
		t.Fatalf("override: %+v %v", r, err)
	}
	for _, v := range []int{0, 5, -1} {
		v := v
		if _, err := power.Resolve(ResourceRequest{CPU: &v}); err == nil {
			t.Errorf("cpu %d accepted", v)
		}
	}
}

func TestLoadCatalogAndDenylist(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	_ = os.WriteFile(good, []byte(`{"tiers":[{"name":"power","defaults":{"cpu":3,"memory_mib":4096,"disk_gib":20},
		"min":{"cpu":1,"memory_mib":1024,"disk_gib":10},"max":{"cpu":4,"memory_mib":8192,"disk_gib":50},
		"root_access":true,"image":{"server":"ubuntu","alias":"24.04"},"lxd_profiles":["default"]}]}`), 0o600)
	c, err := LoadCatalog(good)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := c.Get("power"); p.Defaults.CPU != 3 {
		t.Fatal("override not applied")
	}
	for _, key := range []string{"security.privileged", "raw.lxc", "raw.apparmor", "security.nesting", "limits.cpu", "user.mea.owner"} {
		tiers := DefaultTiers()
		tiers[0].ExtraConfig = map[string]string{key: "true"}
		if _, err := NewCatalog(tiers); err == nil {
			t.Errorf("extra_config %s accepted", key)
		}
	}
	bad := DefaultTiers()
	bad[1].Defaults.CPU = 99
	if _, err := NewCatalog(bad); err == nil {
		t.Error("default above max accepted")
	}
}

func TestBuildInstanceSecurity(t *testing.T) {
	c, _ := NewCatalog(DefaultTiers())
	for _, name := range []string{"newbie", "power", "eleet"} {
		tier, _ := c.Get(name)
		inst := BuildInstance(InstanceSpec{
			Name: "c1", OwnerID: "o", Tier: tier, Resources: tier.Defaults,
			Source: api.InstanceSource{Type: api.SourceTypeImage, Alias: "24.04"}, Network: "lxdbr0", StoragePool: "default",
			IdmapIsolated: true, PortIsolation: true,
		})
		if inst.Type != api.InstanceTypeContainer {
			t.Errorf("%s: type %s", name, inst.Type)
		}
		if inst.Config["security.privileged"] != "false" || inst.Config["security.nesting"] != "false" {
			t.Errorf("%s: privileged/nesting not disabled", name)
		}
		for k, d := range inst.Devices {
			if d["type"] == "disk" && d["path"] != "/" {
				t.Errorf("%s: host mount device %s", name, k)
			}
			if d["type"] == "gpu" && !tier.GPU {
				t.Errorf("%s: GPU device on non-GPU tier", name)
			}
		}
		if _, ok := inst.Devices["gpu0"]; ok != tier.GPU {
			t.Errorf("%s: gpu device presence = %v", name, ok)
		}
		for k := range inst.Config {
			if strings.HasPrefix(k, "raw.") {
				t.Errorf("%s: raw key %s", name, k)
			}
		}
	}
}

func TestUserDataQuotesKeys(t *testing.T) {
	ud := UserData(true, []string{`ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample user@host`})
	if !strings.Contains(ud, `"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample user@host"`) {
		t.Fatalf("key not quoted:\n%s", ud)
	}
}

func TestSpecHashStable(t *testing.T) {
	a := SpecHash(SpecInput{OwnerID: "o", Tier: "Power", Resources: model.Resources{CPU: 2}})
	b := SpecHash(SpecInput{OwnerID: "o", Tier: "power", Resources: model.Resources{CPU: 2}})
	if a != b {
		t.Fatal("tier case changes hash")
	}
	if a == SpecHash(SpecInput{OwnerID: "o", Tier: "power", Resources: model.Resources{CPU: 3}}) {
		t.Fatal("different resources same hash")
	}
}

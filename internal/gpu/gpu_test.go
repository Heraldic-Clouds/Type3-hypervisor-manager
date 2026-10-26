package gpu

import (
	"errors"
	"testing"

	"github.com/canonical/lxd/shared/api"
)

func TestInventoryAndEligibility(t *testing.T) {
	res := &api.Resources{GPU: api.ResourcesGPU{Cards: []api.ResourcesGPUCard{
		{Vendor: "Intel", VendorID: "8086", Driver: "i915"},
		{Vendor: "NVIDIA", VendorID: "10DE", Driver: "nvidia", PCIAddress: "0000:01:00.0",
			Nvidia: &api.ResourcesGPUCardNvidia{Model: "RTX 4090", UUID: "GPU-1", NVRMVersion: "550"}},
	}}}
	inv := FromResources(res)
	if inv.Total != 2 || inv.Nvidia != 1 || !inv.GPUCapable() || inv.Cards[1].Nvidia.DriverVersion != "550" {
		t.Fatalf("inventory = %+v", inv)
	}
	if err := CheckEligibility(false, true, inv); !errors.Is(err, ErrGPUNotAllowed) {
		t.Fatalf("non-GPU tier: %v", err)
	}
	if err := CheckEligibility(true, true, FromResources(&api.Resources{})); !errors.Is(err, ErrNoGPU) {
		t.Fatalf("CPU-only host: %v", err)
	}
	if err := CheckEligibility(true, true, inv); err != nil {
		t.Fatal(err)
	}
	nouveau := FromResources(&api.Resources{GPU: api.ResourcesGPU{Cards: []api.ResourcesGPUCard{{VendorID: "10de", Driver: "nouveau"}}}})
	if nouveau.GPUCapable() {
		t.Fatal("nouveau-bound card must not count as usable")
	}
	if d := Device("0000:01:00.0"); d["type"] != "gpu" || d["pci"] != "0000:01:00.0" || d["vendorid"] != "10de" {
		t.Fatalf("device = %v", d)
	}
	if _, err := (NoopMonitor{}).Utilization(nil, "h"); !errors.Is(err, ErrMonitoringUnavailable) {
		t.Fatal("noop monitor should report unavailable")
	}
}

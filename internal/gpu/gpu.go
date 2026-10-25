// Package gpu builds GPU inventory from LXD resource data and decides which
// containers may receive GPU devices. It uses LXD's own "gpu" device type
// (physical passthrough of the host's device nodes into an unprivileged
// container, shared between containers). It does not create NVIDIA vGPU or
// MIG partitions.
package gpu

import (
	"context"
	"errors"
	"strings"

	"github.com/canonical/lxd/shared/api"
)

// NvidiaVendorID is the PCI vendor ID for NVIDIA.
const NvidiaVendorID = "10de"

// Card is one GPU reported by an LXD host.
type Card struct {
	Index         int         `json:"index"`
	Vendor        string      `json:"vendor"`
	VendorID      string      `json:"vendor_id"`
	Product       string      `json:"product"`
	ProductID     string      `json:"product_id"`
	Driver        string      `json:"driver"`
	DriverVersion string      `json:"driver_version"`
	PCIAddress    string      `json:"pci_address"`
	DRMCard       string      `json:"drm_card,omitempty"`
	Nvidia        *NvidiaInfo `json:"nvidia,omitempty"`
}

// NvidiaInfo is the NVIDIA-specific data LXD collects when the NVIDIA
// driver and container toolkit are present on the host.
type NvidiaInfo struct {
	Model         string `json:"model"`
	Brand         string `json:"brand"`
	UUID          string `json:"uuid"`
	CUDAVersion   string `json:"cuda_version"`
	DriverVersion string `json:"driver_version"`
	Architecture  string `json:"architecture,omitempty"`
}

// Inventory summarises the GPUs of one host.
type Inventory struct {
	Cards  []Card `json:"cards"`
	Total  int    `json:"total"`
	Nvidia int    `json:"nvidia"`
	// NvidiaDriverReady is true when at least one NVIDIA card is bound to the
	// proprietary "nvidia" kernel driver and LXD could read its NVIDIA
	// details (nouveau- or vfio-bound cards cannot be shared into containers
	// with the NVIDIA runtime).
	NvidiaDriverReady bool `json:"nvidia_driver_ready"`
}

// GPUCapable reports whether the host can serve GPU containers.
func (inv Inventory) GPUCapable() bool { return inv.Nvidia > 0 && inv.NvidiaDriverReady }

// FromResources extracts the inventory from an LXD resources response.
func FromResources(r *api.Resources) Inventory {
	inv := Inventory{Cards: []Card{}}
	if r == nil {
		return inv
	}
	for i, c := range r.GPU.Cards {
		card := Card{
			Index: i, Vendor: c.Vendor, VendorID: strings.ToLower(c.VendorID), Product: c.Product,
			ProductID: c.ProductID, Driver: c.Driver, DriverVersion: c.DriverVersion, PCIAddress: c.PCIAddress,
		}
		if c.DRM != nil {
			card.DRMCard = c.DRM.CardName
		}
		if c.Nvidia != nil {
			card.Nvidia = &NvidiaInfo{
				Model: c.Nvidia.Model, Brand: c.Nvidia.Brand, UUID: c.Nvidia.UUID,
				CUDAVersion: c.Nvidia.CUDAVersion, DriverVersion: c.Nvidia.NVRMVersion, Architecture: c.Nvidia.Architecture,
			}
		}
		inv.Cards = append(inv.Cards, card)
		if card.VendorID == NvidiaVendorID {
			inv.Nvidia++
			if card.Driver == "nvidia" && card.Nvidia != nil {
				inv.NvidiaDriverReady = true
			}
		}
	}
	inv.Total = len(inv.Cards)
	return inv
}

// ErrGPUNotAllowed is returned when a tier without GPU entitlement asks for one.
var ErrGPUNotAllowed = errors.New("tier is not entitled to GPU access")

// ErrNoGPU is returned when a GPU tier is placed on a host without a usable GPU.
var ErrNoGPU = errors.New("host has no usable NVIDIA GPU")

// CheckEligibility enforces the tier rule: GPU tiers need a GPU-capable host,
// non-GPU tiers never get a GPU device.
func CheckEligibility(tierAllowsGPU, wantGPU bool, inv Inventory) error {
	if wantGPU && !tierAllowsGPU {
		return ErrGPUNotAllowed
	}
	if wantGPU && !inv.GPUCapable() {
		return ErrNoGPU
	}
	return nil
}

// Device returns the LXD device definition that shares NVIDIA GPU(s) with a
// container. With pciAddress empty all NVIDIA GPUs on the host are exposed.
func Device(pciAddress string) map[string]string {
	d := map[string]string{"type": "gpu", "gputype": "physical", "vendorid": NvidiaVendorID}
	if pciAddress != "" {
		d["pci"] = pciAddress
	}
	return d
}

// IsGPUDevice reports whether an LXD device definition passes a GPU through.
func IsGPUDevice(dev map[string]string) bool {
	return dev["type"] == "gpu"
}

// Utilization is a GPU usage sample. Filled in by a future monitor (M6).
type Utilization struct {
	UUID              string  `json:"uuid"`
	GPUPercent        float64 `json:"gpu_percent"`
	MemoryUsedBytes   uint64  `json:"memory_used_bytes"`
	MemoryTotalBytes  uint64  `json:"memory_total_bytes"`
	EncoderPercent    float64 `json:"encoder_percent"`
	DecoderPercent    float64 `json:"decoder_percent"`
	EncoderSessions   int     `json:"encoder_sessions"`
	AttachedInstances int     `json:"attached_instances"`
}

// UtilizationMonitor reports live GPU and NVENC/NVDEC usage per host. The
// Milestone 6 scheduler will implement it (e.g. via NVML on the host).
type UtilizationMonitor interface {
	Utilization(ctx context.Context, hostID string) ([]Utilization, error)
}

// ErrMonitoringUnavailable is returned by NoopMonitor.
var ErrMonitoringUnavailable = errors.New("GPU utilization monitoring is not implemented yet (Milestone 6)")

// NoopMonitor is the Milestone 1 placeholder.
type NoopMonitor struct{}

// Utilization implements UtilizationMonitor.
func (NoopMonitor) Utilization(context.Context, string) ([]Utilization, error) {
	return nil, ErrMonitoringUnavailable
}

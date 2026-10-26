package profiles

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/canonical/lxd/shared/api"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/gpu"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
)

// Keys Type3 stores on every managed instance.
const (
	KeyManaged  = "user.mea.managed"
	KeyOwner    = "user.mea.owner"
	KeyTier     = "user.mea.tier"
	KeySpecHash = "user.mea.spec-hash"
	KeyCPU      = "limits.cpu"
	KeyMemory   = "limits.memory"
	RootDevice  = "root"
	NICDevice   = "eth0"
	GPUDevice   = "gpu0"
	// GuestUser is the customer account created inside every container.
	GuestUser = "mea"
)

// InstanceSpec is everything needed to build an LXD instance definition.
type InstanceSpec struct {
	Name          string
	OwnerID       string
	Tier          Tier
	Resources     model.Resources
	Source        api.InstanceSource
	Network       string
	StoragePool   string
	GPUPCIAddress string
	SSHKeys       []string
	IdmapIsolated bool
	PortIsolation bool
	SpecHash      string
}

// BuildInstance returns the InstancesPost for spec. Containers are always
// unprivileged system containers with no nesting, LXD's default AppArmor and
// seccomp policy, hard memory limits and no host mounts.
func BuildInstance(s InstanceSpec) api.InstancesPost {
	cfg := maps.Clone(s.Tier.ExtraConfig)
	if cfg == nil {
		cfg = map[string]string{}
	}
	maps.Copy(cfg, ResourceConfig(s.Resources))
	maps.Copy(cfg, map[string]string{
		"security.privileged":   "false",
		"security.nesting":      "false",
		"limits.memory.enforce": "hard",
		"limits.memory.swap":    "false",
		"boot.autostart":        "false",
		"cloud-init.user-data":  UserData(s.Tier.RootAccess, s.SSHKeys),
		KeyManaged:              "true",
		KeyOwner:                s.OwnerID,
		KeyTier:                 s.Tier.Name,
		KeySpecHash:             s.SpecHash,
	})
	if s.IdmapIsolated {
		cfg["security.idmap.isolated"] = "true"
	}
	if s.Tier.ProcessLimit > 0 {
		cfg["limits.processes"] = strconv.Itoa(s.Tier.ProcessLimit)
	}

	devices := map[string]map[string]string{
		RootDevice: RootDisk(s.StoragePool, s.Resources.DiskGiB),
		NICDevice:  NIC(s.Network, s.PortIsolation),
	}
	if s.Tier.GPU {
		devices[GPUDevice] = gpu.Device(s.GPUPCIAddress)
		cfg["nvidia.runtime"] = "true"
		if s.Tier.NvidiaCapabilities != "" {
			cfg["nvidia.driver.capabilities"] = s.Tier.NvidiaCapabilities
		}
	}

	return api.InstancesPost{
		Name:   s.Name,
		Type:   api.InstanceTypeContainer,
		Source: s.Source,
		InstancePut: api.InstancePut{
			Config:      cfg,
			Devices:     devices,
			Ephemeral:   s.Tier.Ephemeral,
			Profiles:    slices.Clone(s.Tier.LXDProfiles),
			Description: fmt.Sprintf("Mise En Abyme %s container for %s", s.Tier.Name, s.OwnerID),
		},
	}
}

// ResourceConfig returns the CPU and memory limit keys.
func ResourceConfig(r model.Resources) map[string]string {
	return map[string]string{
		KeyCPU:    strconv.Itoa(r.CPU),
		KeyMemory: fmt.Sprintf("%dMiB", r.MemoryMiB),
	}
}

// RootDisk returns the root disk device with a size quota.
func RootDisk(pool string, gib int) map[string]string {
	return map[string]string{"type": "disk", "path": "/", "pool": pool, "size": fmt.Sprintf("%dGiB", gib)}
}

// NIC returns the network device. With isolation enabled, containers on the
// same bridge cannot talk to each other (only to the gateway) and cannot
// spoof MAC addresses, which separates customers sharing a host.
func NIC(network string, isolate bool) map[string]string {
	d := map[string]string{"type": "nic", "network": network, "name": "eth0"}
	if isolate {
		d["security.port_isolation"] = "true"
		d["security.mac_filtering"] = "true"
	}
	return d
}

// UserData renders cloud-init user-data creating the customer account. Root
// tiers get passwordless sudo inside the container; Newbie does not. Root's
// password is locked and SSH password login is disabled in every tier.
func UserData(rootAccess bool, sshKeys []string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("disable_root: true\n")
	b.WriteString("ssh_pwauth: false\n")
	b.WriteString("users:\n")
	b.WriteString("  - name: " + GuestUser + "\n")
	b.WriteString("    gecos: Mise En Abyme user\n")
	b.WriteString("    shell: /bin/bash\n")
	b.WriteString("    lock_passwd: true\n")
	if rootAccess {
		b.WriteString("    groups: [sudo]\n")
		b.WriteString("    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n")
	} else {
		b.WriteString("    sudo: false\n")
	}
	if len(sshKeys) > 0 {
		b.WriteString("    ssh_authorized_keys:\n")
		for _, k := range sshKeys {
			q, _ := json.Marshal(strings.TrimSpace(k))
			b.WriteString("      - " + string(q) + "\n")
		}
	}
	b.WriteString("runcmd:\n")
	b.WriteString("  - [passwd, -l, root]\n")
	return b.String()
}

// SpecInput is the subset of a request that defines a container's identity
// for idempotency.
type SpecInput struct {
	OwnerID     string          `json:"owner_id"`
	Tier        string          `json:"tier"`
	Resources   model.Resources `json:"resources"`
	Image       model.ImageRef  `json:"image"`
	Network     string          `json:"network"`
	StoragePool string          `json:"storage_pool"`
	HostID      string          `json:"host_id,omitempty"`
	SSHKeys     []string        `json:"ssh_keys,omitempty"`
}

// SpecHash returns a stable hash of a creation request.
func SpecHash(in SpecInput) string {
	in.Tier = strings.ToLower(in.Tier)
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

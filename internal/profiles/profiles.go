// Package profiles defines the Mise En Abyme subscription tiers (Newbie,
// Power, Eleet) as server-side resource profiles and turns a validated
// request into an LXD instance definition. Customers never supply raw LXD
// configuration: every key Type3 sends to LXD comes from this package.
package profiles

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/validate"
)

// Tier is one subscription profile. All allocations are configurable
// defaults with server-side bounds, not fixed business limits.
type Tier struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Defaults    model.Resources `json:"defaults"`
	Min         model.Resources `json:"min"`
	Max         model.Resources `json:"max"`
	GPU         bool            `json:"gpu"`
	// RootAccess grants the customer passwordless sudo inside the
	// (always unprivileged) container.
	RootAccess bool `json:"root_access"`
	// Ephemeral containers are destroyed by LXD when they stop.
	Ephemeral bool `json:"ephemeral"`
	// PersistentStorage documents the storage class; "efs" is reserved for
	// a later milestone and currently behaves like "local".
	PersistentStorage string         `json:"persistent_storage"`
	Image             model.ImageRef `json:"image"`
	LXDProfiles       []string       `json:"lxd_profiles"`
	StoragePool       string         `json:"storage_pool,omitempty"`
	Network           string         `json:"network,omitempty"`
	ProcessLimit      int            `json:"process_limit"`
	// NvidiaCapabilities is passed to nvidia.driver.capabilities for GPU tiers.
	NvidiaCapabilities string `json:"nvidia_driver_capabilities,omitempty"`
	// ExtraConfig is operator-supplied LXD config, checked against a denylist.
	ExtraConfig map[string]string `json:"extra_config,omitempty"`
}

// Catalog is the set of tiers, keyed by lowercase name.
type Catalog struct {
	tiers map[string]Tier
}

// DefaultTiers returns the built-in Mise En Abyme tiers.
func DefaultTiers() []Tier {
	img := model.ImageRef{Server: "ubuntu", Alias: "24.04"}
	return []Tier{
		{
			Name: "newbie", Description: "Disposable Ubuntu container, no GPU, no administrator access",
			Defaults: model.Resources{CPU: 1, MemoryMiB: 1024, DiskGiB: 10},
			Min:      model.Resources{CPU: 1, MemoryMiB: 512, DiskGiB: 5},
			Max:      model.Resources{CPU: 2, MemoryMiB: 2048, DiskGiB: 20},
			GPU:      false, RootAccess: false, Ephemeral: true, PersistentStorage: "none",
			Image: img, LXDProfiles: []string{"default"}, ProcessLimit: 2000,
		},
		{
			Name: "power", Description: "Ubuntu container with root inside the unprivileged container, no GPU",
			Defaults: model.Resources{CPU: 2, MemoryMiB: 6144, DiskGiB: 40},
			Min:      model.Resources{CPU: 1, MemoryMiB: 1024, DiskGiB: 10},
			Max:      model.Resources{CPU: 4, MemoryMiB: 8192, DiskGiB: 100},
			GPU:      false, RootAccess: true, Ephemeral: false, PersistentStorage: "local",
			Image: img, LXDProfiles: []string{"default"}, ProcessLimit: 5000,
		},
		{
			Name: "eleet", Description: "Ubuntu container with shared NVIDIA GPU access and root inside the unprivileged container",
			Defaults: model.Resources{CPU: 4, MemoryMiB: 12288, DiskGiB: 80},
			Min:      model.Resources{CPU: 2, MemoryMiB: 4096, DiskGiB: 20},
			Max:      model.Resources{CPU: 8, MemoryMiB: 16384, DiskGiB: 200},
			GPU:      true, RootAccess: true, Ephemeral: false, PersistentStorage: "local",
			Image: img, LXDProfiles: []string{"default"}, ProcessLimit: 10000,
			NvidiaCapabilities: "compute,utility,graphics,video,display",
		},
	}
}

// NewCatalog validates tiers and builds a catalog.
func NewCatalog(tiers []Tier) (*Catalog, error) {
	c := &Catalog{tiers: map[string]Tier{}}
	for _, t := range tiers {
		t.Name = strings.ToLower(strings.TrimSpace(t.Name))
		if err := validateTier(t); err != nil {
			return nil, fmt.Errorf("tier %q: %w", t.Name, err)
		}
		if _, dup := c.tiers[t.Name]; dup {
			return nil, fmt.Errorf("tier %q defined twice", t.Name)
		}
		c.tiers[t.Name] = t
	}
	if len(c.tiers) == 0 {
		return nil, fmt.Errorf("no tiers defined")
	}
	return c, nil
}

// LoadCatalog reads tiers from a JSON file ({"tiers": [...]}); an empty path
// returns the defaults.
func LoadCatalog(path string) (*Catalog, error) {
	if path == "" {
		return NewCatalog(DefaultTiers())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read profiles: %w", err)
	}
	var doc struct {
		Tiers []Tier `json:"tiers"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse profiles %s: %w", path, err)
	}
	return NewCatalog(doc.Tiers)
}

// Get returns a tier by name (case-insensitive).
func (c *Catalog) Get(name string) (Tier, error) {
	t, ok := c.tiers[strings.ToLower(name)]
	if !ok {
		return Tier{}, errs.Invalid("unknown_tier", "tier %q does not exist", name).
			WithDetails(map[string]any{"available": c.Names()})
	}
	return t, nil
}

// List returns all tiers sorted by default CPU then name.
func (c *Catalog) List() []Tier {
	out := make([]Tier, 0, len(c.tiers))
	for _, t := range c.tiers {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Defaults.CPU != out[j].Defaults.CPU {
			return out[i].Defaults.CPU < out[j].Defaults.CPU
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Names returns tier names.
func (c *Catalog) Names() []string {
	var out []string
	for _, t := range c.List() {
		out = append(out, t.Name)
	}
	return out
}

// ResourceRequest carries optional overrides; nil fields use tier defaults.
type ResourceRequest struct {
	CPU       *int `json:"cpu,omitempty"`
	MemoryMiB *int `json:"memory_mib,omitempty"`
	DiskGiB   *int `json:"disk_gib,omitempty"`
}

// Resolve applies overrides to the tier defaults and enforces the tier's
// bounds, so a client can never exceed what the server-side profile allows.
func (t Tier) Resolve(req ResourceRequest) (model.Resources, error) {
	return t.Apply(t.Defaults, req)
}

// Apply applies overrides to base and enforces bounds.
func (t Tier) Apply(base model.Resources, req ResourceRequest) (model.Resources, error) {
	r := base
	if req.CPU != nil {
		r.CPU = *req.CPU
	}
	if req.MemoryMiB != nil {
		r.MemoryMiB = *req.MemoryMiB
	}
	if req.DiskGiB != nil {
		r.DiskGiB = *req.DiskGiB
	}
	if err := validate.Range("cpu", r.CPU, t.Min.CPU, t.Max.CPU); err != nil {
		return r, tierErr(err, t.Name)
	}
	if err := validate.Range("memory_mib", r.MemoryMiB, t.Min.MemoryMiB, t.Max.MemoryMiB); err != nil {
		return r, tierErr(err, t.Name)
	}
	if err := validate.Range("disk_gib", r.DiskGiB, t.Min.DiskGiB, t.Max.DiskGiB); err != nil {
		return r, tierErr(err, t.Name)
	}
	return r, nil
}

func tierErr(err error, tier string) error {
	if e, ok := errs.As(err); ok {
		e.Code = "resource_limit_exceeded"
		e.Message = fmt.Sprintf("%s for tier %s", e.Message, tier)
		return e.WithDetails(map[string]any{"tier": tier})
	}
	return err
}

func validateTier(t Tier) error {
	if err := validate.Name("tier", t.Name); err != nil {
		return err
	}
	for _, chk := range []struct {
		field         string
		min, def, max int
	}{
		{"cpu", t.Min.CPU, t.Defaults.CPU, t.Max.CPU},
		{"memory_mib", t.Min.MemoryMiB, t.Defaults.MemoryMiB, t.Max.MemoryMiB},
		{"disk_gib", t.Min.DiskGiB, t.Defaults.DiskGiB, t.Max.DiskGiB},
	} {
		if chk.min < 1 || chk.min > chk.def || chk.def > chk.max {
			return fmt.Errorf("%s needs 1 <= min <= default <= max (got %d/%d/%d)", chk.field, chk.min, chk.def, chk.max)
		}
	}
	if t.ProcessLimit < 0 {
		return fmt.Errorf("process_limit must be >= 0")
	}
	if t.Image.Alias == "" && t.Image.Fingerprint == "" {
		return fmt.Errorf("image needs an alias or fingerprint")
	}
	switch t.PersistentStorage {
	case "", "none", "local", "efs":
	default:
		return fmt.Errorf("persistent_storage %q, want none, local or efs", t.PersistentStorage)
	}
	if t.Ephemeral && t.PersistentStorage != "" && t.PersistentStorage != "none" {
		return fmt.Errorf("ephemeral tiers cannot have persistent storage")
	}
	for _, p := range t.LXDProfiles {
		if err := validate.Name("lxd_profile", p); err != nil && p != "default" {
			return err
		}
	}
	return CheckConfig(t.ExtraConfig)
}

// deniedConfigPrefixes can weaken isolation and are never accepted from
// configuration files or callers.
var deniedConfigPrefixes = []string{
	"raw.", "security.privileged", "security.nesting", "security.idmap", "security.syscalls",
	"security.devlxd", "security.protection", "linux.kernel_modules", "limits.", "nvidia.",
	"user.mea.", "volatile.", "boot.host_shutdown", "cloud-init.", "user.user-data", "user.vendor-data",
	"environment.",
}

// CheckConfig rejects LXD configuration keys that could break container
// isolation or override values Type3 controls.
func CheckConfig(cfg map[string]string) error {
	for k := range cfg {
		lk := strings.ToLower(k)
		for _, p := range deniedConfigPrefixes {
			if strings.HasPrefix(lk, p) {
				return errs.Invalid("forbidden_config", "LXD config key %q is not allowed", k)
			}
		}
	}
	return nil
}

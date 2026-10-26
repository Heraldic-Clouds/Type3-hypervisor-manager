package hosts

import (
	"context"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
)

// HostProvider manages the machine behind a host: an EC2 instance in
// Milestone 2, a dedicated server's BMC or nothing at all for static hosts.
// The registry only depends on this interface, so non-AWS hosts never need
// AWS code.
type HostProvider interface {
	// Name is the value stored in model.Host.Provider.
	Name() string
	// Describe returns provider metadata (instance ID, region, type, ...).
	Describe(ctx context.Context, h model.Host) (map[string]string, error)
	// PowerOn starts the underlying machine.
	PowerOn(ctx context.Context, h model.Host) error
	// PowerOff stops the underlying machine.
	PowerOff(ctx context.Context, h model.Host) error
}

// ProvisionSpec describes a host a provider should create.
type ProvisionSpec struct {
	Name         string            `json:"name"`
	InstanceType string            `json:"instance_type"`
	ImageID      string            `json:"image_id"`
	GPU          bool              `json:"gpu"`
	Labels       map[string]string `json:"labels"`
}

// HostProvisioner creates and destroys hosts. Milestone 2 implements it for
// EC2 (AMI-based LXD hosts); Milestone 1 ships no implementation.
type HostProvisioner interface {
	Provision(ctx context.Context, spec ProvisionSpec) (RegisterRequest, error)
	Deprovision(ctx context.Context, h model.Host) error
}

// StaticProvider is used for hosts Type3 does not control: an existing
// Ubuntu server or VM with LXD installed by an operator.
type StaticProvider struct{}

// Name implements HostProvider.
func (StaticProvider) Name() string { return "static" }

// Describe implements HostProvider.
func (StaticProvider) Describe(context.Context, model.Host) (map[string]string, error) {
	return map[string]string{"provider": "static"}, nil
}

// PowerOn implements HostProvider.
func (StaticProvider) PowerOn(context.Context, model.Host) error {
	return errs.New(errs.KindPrecondition, "provider_unsupported", "static hosts cannot be powered on by Type3")
}

// PowerOff implements HostProvider.
func (StaticProvider) PowerOff(context.Context, model.Host) error {
	return errs.New(errs.KindPrecondition, "provider_unsupported", "static hosts cannot be powered off by Type3")
}

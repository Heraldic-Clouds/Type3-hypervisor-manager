// Package lxdclient wraps github.com/canonical/lxd/client behind a narrow,
// context-aware interface. Everything above this package depends on Server,
// so unit tests run against an in-memory fake and only the adapter talks to a
// real LXD daemon.
package lxdclient

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/canonical/lxd/shared/api"
)

// Server is the subset of the LXD API Type3 uses. All calls are scoped to
// containers (system containers, never virtual machines) in one LXD project.
type Server interface {
	GetServer(ctx context.Context) (*api.Server, error)
	GetServerResources(ctx context.Context) (*api.Resources, error)
	GetStoragePools(ctx context.Context) ([]api.StoragePool, error)
	GetStoragePoolResources(ctx context.Context, pool string) (*api.ResourcesStoragePool, error)
	GetNetworks(ctx context.Context) ([]api.Network, error)
	GetProfileNames(ctx context.Context) ([]string, error)
	GetClusterMembers(ctx context.Context) ([]api.ClusterMember, error)

	GetInstances(ctx context.Context) ([]api.Instance, error)
	GetInstance(ctx context.Context, name string) (*api.Instance, string, error)
	GetInstanceState(ctx context.Context, name string) (*api.InstanceState, error)
	CreateInstance(ctx context.Context, req api.InstancesPost) (Op, error)
	UpdateInstance(ctx context.Context, name string, put api.InstancePut, etag string) (Op, error)
	UpdateInstanceState(ctx context.Context, name string, put api.InstanceStatePut) (Op, error)
	DeleteInstance(ctx context.Context, name string) (Op, error)

	GetInstanceSnapshots(ctx context.Context, name string) ([]api.InstanceSnapshot, error)
	CreateInstanceSnapshot(ctx context.Context, name string, req api.InstanceSnapshotsPost) (Op, error)
	DeleteInstanceSnapshot(ctx context.Context, name, snapshot string) (Op, error)

	GetImages(ctx context.Context) ([]api.Image, error)
	GetImage(ctx context.Context, fingerprint string) (*api.Image, error)
	GetImageAlias(ctx context.Context, name string) (*api.ImageAliasesEntry, error)
	CreateImage(ctx context.Context, req api.ImagesPost) (Op, error)
	DeleteImage(ctx context.Context, fingerprint string) (Op, error)

	Disconnect()
}

// Op is a background LXD operation.
type Op interface {
	// ID is the LXD operation UUID.
	ID() string
	// Wait blocks until the operation finishes or ctx ends. progress, when
	// non-nil, is called with each refreshed operation snapshot.
	Wait(ctx context.Context, progress func(api.Operation)) error
	Cancel() error
}

// IsNotFound reports whether err is an LXD 404.
func IsNotFound(err error) bool {
	return err != nil && api.StatusErrorCheck(err, http.StatusNotFound)
}

// IsConflict reports whether err is an LXD 409 (e.g. already exists).
func IsConflict(err error) bool {
	return err != nil && api.StatusErrorCheck(err, http.StatusConflict)
}

// ErrOperationTimeout is returned by Wait when the context deadline passes.
var ErrOperationTimeout = errors.New("lxd operation did not finish before the deadline")

// PollInterval is how often Wait refreshes an operation.
var PollInterval = 750 * time.Millisecond

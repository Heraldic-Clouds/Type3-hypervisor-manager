// Package store persists Type3 state behind an interface so the development
// JSON-file backend can be replaced by PostgreSQL in Milestone 3.
package store

import (
	"context"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
)

// ContainerFilter narrows ListContainers. Empty fields match everything.
type ContainerFilter struct {
	HostID  string
	OwnerID string
	Tier    string
}

// Store is the persistence contract. Implementations must be safe for
// concurrent use and return copies, never shared pointers into their state.
type Store interface {
	ListHosts(ctx context.Context) ([]model.Host, error)
	GetHost(ctx context.Context, id string) (model.Host, error)
	PutHost(ctx context.Context, h model.Host) error
	DeleteHost(ctx context.Context, id string) error

	ListContainers(ctx context.Context, f ContainerFilter) ([]model.Container, error)
	GetContainer(ctx context.Context, id string) (model.Container, error)
	// FindContainerByIdempotencyKey returns the container created with key.
	FindContainerByIdempotencyKey(ctx context.Context, key string) (model.Container, error)
	PutContainer(ctx context.Context, c model.Container) error
	DeleteContainer(ctx context.Context, id string) error

	ListOperations(ctx context.Context) ([]model.Operation, error)
	PutOperation(ctx context.Context, op model.Operation) error
	DeleteOperation(ctx context.Context, id string) error

	// Ping checks the backend is usable.
	Ping(ctx context.Context) error
	Close() error
}

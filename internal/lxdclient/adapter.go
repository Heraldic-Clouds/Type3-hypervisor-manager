package lxdclient

import (
	"context"
	"errors"
	"fmt"

	lxd "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
)

// contexter is implemented by *lxd.ProtocolLXD; it scopes requests to ctx.
type contexter interface {
	WithContext(ctx context.Context) lxd.InstanceServer
}

// adapter implements Server on top of a real LXD client.
type adapter struct {
	srv lxd.InstanceServer
}

// Wrap adapts an lxd.InstanceServer to Server.
func Wrap(srv lxd.InstanceServer) Server { return &adapter{srv: srv} }

func (a *adapter) c(ctx context.Context) lxd.InstanceServer {
	if wc, ok := a.srv.(contexter); ok && ctx != nil {
		return wc.WithContext(ctx)
	}
	return a.srv
}

func (a *adapter) GetServer(ctx context.Context) (*api.Server, error) {
	s, _, err := a.c(ctx).GetServer()
	return s, err
}

func (a *adapter) GetServerResources(ctx context.Context) (*api.Resources, error) {
	return a.c(ctx).GetServerResources()
}

func (a *adapter) GetStoragePools(ctx context.Context) ([]api.StoragePool, error) {
	return a.c(ctx).GetStoragePools()
}

func (a *adapter) GetStoragePoolResources(ctx context.Context, pool string) (*api.ResourcesStoragePool, error) {
	return a.c(ctx).GetStoragePoolResources(pool)
}

func (a *adapter) GetNetworks(ctx context.Context) ([]api.Network, error) {
	return a.c(ctx).GetNetworks()
}

func (a *adapter) GetProfileNames(ctx context.Context) ([]string, error) {
	return a.c(ctx).GetProfileNames()
}

func (a *adapter) GetClusterMembers(ctx context.Context) ([]api.ClusterMember, error) {
	c := a.c(ctx)
	if !c.IsClustered() {
		return nil, nil
	}
	return c.GetClusterMembers()
}

func (a *adapter) GetInstances(ctx context.Context) ([]api.Instance, error) {
	return a.c(ctx).GetInstances(lxd.GetInstancesArgs{InstanceType: api.InstanceTypeContainer})
}

func (a *adapter) GetInstance(ctx context.Context, name string) (*api.Instance, string, error) {
	return a.c(ctx).GetInstance(name)
}

func (a *adapter) GetInstanceState(ctx context.Context, name string) (*api.InstanceState, error) {
	st, _, err := a.c(ctx).GetInstanceState(name)
	return st, err
}

func (a *adapter) CreateInstance(ctx context.Context, req api.InstancesPost) (Op, error) {
	return wrapOp(a.c(ctx).CreateInstance(req))
}

func (a *adapter) UpdateInstance(ctx context.Context, name string, put api.InstancePut, etag string) (Op, error) {
	return wrapOp(a.c(ctx).UpdateInstance(name, put, etag))
}

func (a *adapter) UpdateInstanceState(ctx context.Context, name string, put api.InstanceStatePut) (Op, error) {
	return wrapOp(a.c(ctx).UpdateInstanceState(name, put, ""))
}

func (a *adapter) DeleteInstance(ctx context.Context, name string) (Op, error) {
	return wrapOp(a.c(ctx).DeleteInstance(name, false))
}

func (a *adapter) GetInstanceSnapshots(ctx context.Context, name string) ([]api.InstanceSnapshot, error) {
	return a.c(ctx).GetInstanceSnapshots(name)
}

func (a *adapter) CreateInstanceSnapshot(ctx context.Context, name string, req api.InstanceSnapshotsPost) (Op, error) {
	return wrapOp(a.c(ctx).CreateInstanceSnapshot(name, req))
}

func (a *adapter) DeleteInstanceSnapshot(ctx context.Context, name, snapshot string) (Op, error) {
	return wrapOp(a.c(ctx).DeleteInstanceSnapshot(name, snapshot, ""))
}

func (a *adapter) GetImages(ctx context.Context) ([]api.Image, error) {
	return a.c(ctx).GetImages()
}

func (a *adapter) GetImage(ctx context.Context, fingerprint string) (*api.Image, error) {
	img, _, err := a.c(ctx).GetImage(fingerprint)
	return img, err
}

func (a *adapter) GetImageAlias(ctx context.Context, name string) (*api.ImageAliasesEntry, error) {
	al, _, err := a.c(ctx).GetImageAlias(name)
	return al, err
}

func (a *adapter) CreateImage(ctx context.Context, req api.ImagesPost) (Op, error) {
	return wrapOp(a.c(ctx).CreateImage(req, nil))
}

func (a *adapter) DeleteImage(ctx context.Context, fingerprint string) (Op, error) {
	return wrapOp(a.c(ctx).DeleteImage(fingerprint))
}

func (a *adapter) Disconnect() { a.srv.Disconnect() }

type op struct {
	o lxd.Operation
}

func wrapOp(o lxd.Operation, err error) (Op, error) {
	if err != nil {
		return nil, err
	}
	return &op{o: o}, nil
}

func (o *op) ID() string { return o.o.Get().ID }

func (o *op) Cancel() error { return o.o.Cancel() }

// Wait polls the operation instead of using the events websocket, which keeps
// it working through proxies and gives regular progress snapshots.
func (o *op) Wait(ctx context.Context, progress func(api.Operation)) error {
	return PollUntilDone(ctx, func() (api.Operation, error) {
		if err := o.o.Refresh(); err != nil {
			return api.Operation{}, err
		}
		return o.o.Get(), nil
	}, progress)
}

// PollUntilDone polls get until the operation reaches a final state.
func PollUntilDone(ctx context.Context, get func() (api.Operation, error), progress func(api.Operation)) error {
	t := newTicker()
	defer t.Stop()
	for {
		cur, err := get()
		if err != nil {
			if ctx.Err() != nil {
				return timeoutErr(ctx)
			}
			return fmt.Errorf("refresh lxd operation: %w", err)
		}
		if progress != nil {
			progress(cur)
		}
		if cur.StatusCode.IsFinal() {
			if cur.StatusCode != api.Success {
				if cur.Err != "" {
					return errors.New(cur.Err)
				}
				return fmt.Errorf("lxd operation ended with status %s", cur.Status)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return timeoutErr(ctx)
		case <-t.C:
		}
	}
}

func timeoutErr(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrOperationTimeout
	}
	return ctx.Err()
}

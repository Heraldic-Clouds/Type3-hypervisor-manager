// Package images manages LXD images on registered hosts: listing local and
// remote images, importing from configured image servers with fingerprint
// verification, and safe deletion. It is provider-independent; S3-backed
// image publication is Milestone 4.
package images

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	lxd "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/config"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/lxdclient"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/operations"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/validate"
)

// ImageManager is the contract used by the API.
type ImageManager interface {
	ListLocal(ctx context.Context, hostID string) (LocalListing, error)
	ListRemote(ctx context.Context, server, query string) ([]RemoteImage, error)
	Import(ctx context.Context, req ImportRequest, actor, idemKey string) (model.Operation, bool, error)
	Delete(ctx context.Context, hostID, fingerprint string, force bool, actor string) (model.Operation, error)
	ResolveSource(ref model.ImageRef) (api.InstanceSource, error)
	Servers() map[string]config.ImageServer
}

// HostClients is what the image manager needs from the host registry.
type HostClients interface {
	Client(ctx context.Context, id string) (lxdclient.Server, model.Host, error)
	List(ctx context.Context) ([]model.Host, error)
}

// RemoteLister lists images published by a remote image server.
type RemoteLister interface {
	List(ctx context.Context, srv config.ImageServer) ([]api.Image, error)
}

// Image is a local image on a host.
type Image struct {
	HostID       string            `json:"host_id"`
	Fingerprint  string            `json:"fingerprint"`
	Aliases      []string          `json:"aliases"`
	Architecture string            `json:"architecture"`
	Type         string            `json:"type"`
	SizeBytes    int64             `json:"size_bytes"`
	Description  string            `json:"description,omitempty"`
	Properties   map[string]string `json:"properties,omitempty"`
	Cached       bool              `json:"cached"`
	AutoUpdate   bool              `json:"auto_update"`
	CreatedAt    time.Time         `json:"created_at"`
	UploadedAt   time.Time         `json:"uploaded_at"`
}

// LocalListing is the result of listing local images across hosts.
type LocalListing struct {
	Images []Image `json:"images"`
	// Unavailable lists hosts that could not be queried.
	Unavailable []string `json:"unavailable_hosts,omitempty"`
}

// RemoteImage is an image offered by a remote image server.
type RemoteImage struct {
	Server       string   `json:"server"`
	Fingerprint  string   `json:"fingerprint"`
	Aliases      []string `json:"aliases"`
	Architecture string   `json:"architecture"`
	Type         string   `json:"type"`
	Description  string   `json:"description,omitempty"`
	SizeBytes    int64    `json:"size_bytes"`
}

// ImportRequest pulls an image from a configured image server onto a host.
type ImportRequest struct {
	HostID string `json:"host_id"`
	// Server is a configured image server name (default: TYPE3_DEFAULT_IMAGE_SERVER).
	Server      string `json:"server"`
	Alias       string `json:"alias"`
	Fingerprint string `json:"fingerprint"`
	// Aliases are local aliases to create on the host.
	Aliases []string `json:"aliases"`
	// ExpectedFingerprint, when set, must match the imported image or the
	// import is rolled back.
	ExpectedFingerprint string `json:"expected_fingerprint"`
	AutoUpdate          bool   `json:"auto_update"`
}

// Options configures the Manager.
type Options struct {
	Servers       map[string]config.ImageServer
	DefaultServer string
	Timeout       time.Duration
}

// Manager implements ImageManager.
type Manager struct {
	hosts  HostClients
	ops    operations.OperationManager
	remote RemoteLister
	opt    Options
	log    *slog.Logger
}

// NewManager builds an image manager.
func NewManager(hosts HostClients, ops operations.OperationManager, remote RemoteLister, opt Options, log *slog.Logger) *Manager {
	return &Manager{hosts: hosts, ops: ops, remote: remote, opt: opt, log: log}
}

// Servers returns the configured image servers.
func (m *Manager) Servers() map[string]config.ImageServer { return m.opt.Servers }

func toImage(hostID string, img api.Image) Image {
	out := Image{
		HostID: hostID, Fingerprint: img.Fingerprint, Architecture: img.Architecture, Type: img.Type,
		SizeBytes: img.Size, Properties: img.Properties, Cached: img.Cached, AutoUpdate: img.AutoUpdate,
		CreatedAt: img.CreatedAt, UploadedAt: img.UploadedAt, Aliases: []string{},
	}
	out.Description = img.Properties["description"]
	for _, a := range img.Aliases {
		out.Aliases = append(out.Aliases, a.Name)
	}
	return out
}

// ListLocal lists images on one host, or on every host when hostID is empty.
func (m *Manager) ListLocal(ctx context.Context, hostID string) (LocalListing, error) {
	var ids []string
	if hostID != "" {
		ids = []string{hostID}
	} else {
		hs, err := m.hosts.List(ctx)
		if err != nil {
			return LocalListing{}, err
		}
		for _, h := range hs {
			ids = append(ids, h.ID)
		}
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = LocalListing{Images: []Image{}}
	)
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			imgs, err := m.listHost(ctx, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				out.Unavailable = append(out.Unavailable, id)
				return
			}
			out.Images = append(out.Images, imgs...)
		}(id)
	}
	wg.Wait()
	if hostID != "" && len(out.Unavailable) > 0 {
		_, err := m.listHost(ctx, hostID)
		return LocalListing{}, err
	}
	sort.Slice(out.Images, func(i, j int) bool {
		if out.Images[i].HostID != out.Images[j].HostID {
			return out.Images[i].HostID < out.Images[j].HostID
		}
		return out.Images[i].Fingerprint < out.Images[j].Fingerprint
	})
	sort.Strings(out.Unavailable)
	return out, nil
}

func (m *Manager) listHost(ctx context.Context, id string) ([]Image, error) {
	c, _, err := m.hosts.Client(ctx, id)
	if err != nil {
		return nil, err
	}
	imgs, err := c.GetImages(ctx)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindUnavailable, "host_unavailable", "list images on host %q", id)
	}
	out := make([]Image, 0, len(imgs))
	for _, img := range imgs {
		out = append(out, toImage(id, img))
	}
	return out, nil
}

// ListRemote lists container images published by a configured image server.
func (m *Manager) ListRemote(ctx context.Context, server, query string) ([]RemoteImage, error) {
	if server == "" {
		server = m.opt.DefaultServer
	}
	srv, ok := m.opt.Servers[server]
	if !ok {
		return nil, errs.Invalid("unknown_image_server", "image server %q is not configured", server)
	}
	if m.remote == nil {
		return nil, errs.Unavailable("remote_listing_disabled", "remote image listing is not available")
	}
	imgs, err := m.remote.List(ctx, srv)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindUnavailable, "image_server_unavailable", "list images on %s", server)
	}
	query = strings.ToLower(query)
	out := []RemoteImage{}
	for _, img := range imgs {
		if img.Type != "" && img.Type != "container" {
			continue
		}
		ri := RemoteImage{Server: server, Fingerprint: img.Fingerprint, Architecture: img.Architecture,
			Type: img.Type, Description: img.Properties["description"], SizeBytes: img.Size, Aliases: []string{}}
		for _, a := range img.Aliases {
			ri.Aliases = append(ri.Aliases, a.Name)
		}
		if len(ri.Aliases) == 0 {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(strings.Join(ri.Aliases, " ")+" "+ri.Description), query) {
			continue
		}
		out = append(out, ri)
		if len(out) >= 500 {
			break
		}
	}
	return out, nil
}

// ResolveSource turns an image reference into an LXD instance source.
func (m *Manager) ResolveSource(ref model.ImageRef) (api.InstanceSource, error) {
	if ref.Alias == "" && ref.Fingerprint == "" {
		return api.InstanceSource{}, errs.Invalid("invalid_image", "image needs an alias or fingerprint")
	}
	if ref.Alias != "" {
		if err := validate.Alias(ref.Alias); err != nil {
			return api.InstanceSource{}, err
		}
	}
	if ref.Fingerprint != "" {
		if err := validate.Fingerprint(ref.Fingerprint); err != nil {
			return api.InstanceSource{}, err
		}
	}
	src := api.InstanceSource{Type: api.SourceTypeImage, Alias: ref.Alias, Fingerprint: ref.Fingerprint}
	if ref.Fingerprint != "" {
		src.Alias = ""
	}
	if ref.Server != "" {
		srv, ok := m.opt.Servers[ref.Server]
		if !ok {
			return api.InstanceSource{}, errs.Invalid("unknown_image_server", "image server %q is not configured", ref.Server)
		}
		src.Server, src.Protocol, src.Mode = srv.URL, srv.Protocol, "pull"
	}
	return src, nil
}

// Import pulls an image onto a host as a background operation.
func (m *Manager) Import(ctx context.Context, req ImportRequest, actor, idemKey string) (model.Operation, bool, error) {
	if req.HostID == "" {
		return model.Operation{}, false, errs.Invalid("host_id_required", "host_id is required")
	}
	if req.Server == "" {
		req.Server = m.opt.DefaultServer
	}
	srv, ok := m.opt.Servers[req.Server]
	if !ok {
		return model.Operation{}, false, errs.Invalid("unknown_image_server", "image server %q is not configured", req.Server)
	}
	if (req.Alias == "") == (req.Fingerprint == "") {
		return model.Operation{}, false, errs.Invalid("invalid_image", "set exactly one of alias or fingerprint")
	}
	if req.Alias != "" {
		if err := validate.Alias(req.Alias); err != nil {
			return model.Operation{}, false, err
		}
	}
	if req.Fingerprint != "" {
		if err := validate.Fingerprint(req.Fingerprint); err != nil {
			return model.Operation{}, false, err
		}
	}
	if req.ExpectedFingerprint != "" {
		if err := validate.Fingerprint(req.ExpectedFingerprint); err != nil {
			return model.Operation{}, false, err
		}
	}
	if len(req.Aliases) == 0 && req.Fingerprint == "" {
		return model.Operation{}, false, errs.Invalid("aliases_required", "aliases is required when importing by alias, so the image can be referenced afterwards")
	}
	for _, a := range req.Aliases {
		if err := validate.Alias(a); err != nil {
			return model.Operation{}, false, err
		}
	}
	if _, _, err := m.hosts.Client(ctx, req.HostID); err != nil {
		return model.Operation{}, false, err
	}

	resID := req.HostID + ":" + firstNonEmpty(req.Fingerprint, req.Alias)
	return m.ops.Submit(operations.Spec{
		Type: "image.import", ResourceType: "image", ResourceID: resID, HostID: req.HostID,
		Actor: actor, IdempotencyKey: idemKey, Timeout: m.opt.Timeout,
	}, func(ctx context.Context, p *operations.Progress) (map[string]any, error) {
		return m.runImport(ctx, p, req, srv)
	})
}

func (m *Manager) runImport(ctx context.Context, p *operations.Progress, req ImportRequest, srv config.ImageServer) (map[string]any, error) {
	c, _, err := m.hosts.Client(ctx, req.HostID)
	if err != nil {
		return nil, err
	}
	post := api.ImagesPost{
		Source: &api.ImagesPostSource{
			ImageSource: api.ImageSource{Server: srv.URL, Protocol: srv.Protocol, Alias: req.Alias},
			Type:        api.SourceTypeImage, Mode: "pull", Fingerprint: req.Fingerprint,
		},
		ImagePut: api.ImagePut{AutoUpdate: req.AutoUpdate},
	}
	for _, a := range req.Aliases {
		post.Aliases = append(post.Aliases, api.ImageAlias{Name: a})
	}
	p.Stage("downloading image", -1)
	op, err := c.CreateImage(ctx, post)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindUnavailable, "image_import_failed", "start image import")
	}
	var reported string
	err = op.Wait(ctx, func(o api.Operation) {
		p.LXD(op.ID(), progressDetail(o))
		if fp, ok := o.Metadata["fingerprint"].(string); ok && fp != "" {
			reported = fp
		}
	})
	if err != nil {
		return nil, fmt.Errorf("import image: %w", err)
	}

	p.Stage("verifying fingerprint", 90)
	fp := reported
	if fp == "" && req.Fingerprint != "" {
		img, err := c.GetImage(ctx, req.Fingerprint)
		if err != nil {
			return nil, fmt.Errorf("look up imported image: %w", err)
		}
		fp = img.Fingerprint
	}
	if fp == "" && len(req.Aliases) > 0 {
		al, err := c.GetImageAlias(ctx, req.Aliases[0])
		if err != nil {
			return nil, fmt.Errorf("look up imported image alias: %w", err)
		}
		fp = al.Target
	}
	want := firstNonEmpty(req.ExpectedFingerprint, req.Fingerprint)
	if want != "" && !strings.HasPrefix(fp, want) {
		m.log.Warn("image fingerprint mismatch, rolling back import", "host", req.HostID, "want", want, "got", fp)
		if dop, derr := c.DeleteImage(ctx, fp); derr == nil {
			_ = dop.Wait(ctx, nil)
		}
		return nil, errs.New(errs.KindPrecondition, "fingerprint_mismatch", "imported image fingerprint %s does not match expected %s; import rolled back", fp, want)
	}
	return map[string]any{"host_id": req.HostID, "fingerprint": fp, "aliases": req.Aliases, "verified": want != ""}, nil
}

// Delete removes an image from a host. Images still used as the base of an
// instance are kept unless force is set.
func (m *Manager) Delete(ctx context.Context, hostID, fingerprint string, force bool, actor string) (model.Operation, error) {
	if hostID == "" {
		return model.Operation{}, errs.Invalid("host_id_required", "host_id query parameter is required")
	}
	if err := validate.Fingerprint(fingerprint); err != nil {
		return model.Operation{}, err
	}
	c, _, err := m.hosts.Client(ctx, hostID)
	if err != nil {
		return model.Operation{}, err
	}
	img, err := c.GetImage(ctx, fingerprint)
	if lxdclient.IsNotFound(err) {
		return model.Operation{}, errs.NotFound("image_not_found", "image %s not found on host %q", fingerprint, hostID)
	}
	if err != nil {
		return model.Operation{}, errs.Wrap(err, errs.KindUnavailable, "host_unavailable", "look up image")
	}
	full := img.Fingerprint
	if !force {
		insts, err := c.GetInstances(ctx)
		if err != nil {
			return model.Operation{}, errs.Wrap(err, errs.KindUnavailable, "host_unavailable", "list instances")
		}
		var users []string
		for _, in := range insts {
			if in.Config["volatile.base_image"] == full {
				users = append(users, in.Name)
			}
		}
		if len(users) > 0 {
			return model.Operation{}, errs.Conflict("image_in_use", "image is the base of %d instance(s); pass force=true to delete anyway", len(users)).
				WithDetails(map[string]any{"instances": users})
		}
	}
	op, _, err := m.ops.Submit(operations.Spec{
		Type: "image.delete", ResourceType: "image", ResourceID: hostID + ":" + full, HostID: hostID, Actor: actor,
		Timeout: m.opt.Timeout,
	}, func(ctx context.Context, p *operations.Progress) (map[string]any, error) {
		c, _, err := m.hosts.Client(ctx, hostID)
		if err != nil {
			return nil, err
		}
		p.Stage("deleting image", -1)
		op, err := c.DeleteImage(ctx, full)
		if lxdclient.IsNotFound(err) {
			return map[string]any{"fingerprint": full, "already_deleted": true}, nil
		}
		if err != nil {
			return nil, err
		}
		p.LXD(op.ID(), "")
		if err := op.Wait(ctx, nil); err != nil {
			return nil, err
		}
		return map[string]any{"fingerprint": full}, nil
	})
	return op, err
}

func progressDetail(o api.Operation) string {
	for _, k := range []string{"download_progress", "create_instance_from_image_unpack_progress", "progress"} {
		if v, ok := o.Metadata[k]; ok {
			return fmt.Sprint(v)
		}
	}
	return ""
}

// ProgressDetail extracts a human-readable progress string from LXD
// operation metadata.
func ProgressDetail(o api.Operation) string { return progressDetail(o) }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// SimpleStreamsLister lists remote images over the simplestreams protocol
// using the LXD client library.
type SimpleStreamsLister struct {
	UserAgent string
}

// List implements RemoteLister.
func (l SimpleStreamsLister) List(ctx context.Context, srv config.ImageServer) ([]api.Image, error) {
	if srv.Protocol != "simplestreams" {
		return nil, fmt.Errorf("protocol %q not supported for listing", srv.Protocol)
	}
	type result struct {
		imgs []api.Image
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		is, err := lxd.ConnectSimpleStreams(srv.URL, &lxd.ConnectionArgs{UserAgent: l.UserAgent})
		if err != nil {
			ch <- result{err: err}
			return
		}
		imgs, err := is.GetImages()
		ch <- result{imgs: imgs, err: err}
	}()
	select {
	case r := <-ch:
		return r.imgs, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

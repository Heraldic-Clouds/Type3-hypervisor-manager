package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
)

const stateVersion = 1

type fileState struct {
	Version    int                        `json:"version"`
	Hosts      map[string]model.Host      `json:"hosts"`
	Containers map[string]model.Container `json:"containers"`
	Operations map[string]model.Operation `json:"operations"`
}

// FileStore keeps all state in memory and writes it to a JSON file on every
// change (write to temp file, fsync, rename), so a crash never leaves a torn
// file. It suits development and single-instance deployments. An empty path
// gives a purely in-memory store for tests.
type FileStore struct {
	path string
	mu   sync.RWMutex
	st   fileState
}

// OpenFileStore loads path if it exists, or starts empty.
func OpenFileStore(path string) (*FileStore, error) {
	s := &FileStore{path: path, st: fileState{
		Version:    stateVersion,
		Hosts:      map[string]model.Host{},
		Containers: map[string]model.Container{},
		Operations: map[string]model.Operation{},
	}}
	if path == "" {
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, s.flushLocked()
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(b, &s.st); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if s.st.Version > stateVersion {
		return nil, fmt.Errorf("state %s has version %d, this build supports %d", path, s.st.Version, stateVersion)
	}
	if s.st.Hosts == nil {
		s.st.Hosts = map[string]model.Host{}
	}
	if s.st.Containers == nil {
		s.st.Containers = map[string]model.Container{}
	}
	if s.st.Operations == nil {
		s.st.Operations = map[string]model.Operation{}
	}
	return s, nil
}

// NewMemoryStore returns a store that never touches disk.
func NewMemoryStore() *FileStore {
	s, _ := OpenFileStore("")
	return s
}

func (s *FileStore) flushLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.json")
	if err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// mutate applies fn under the write lock and persists; on a failed write the
// in-memory change is rolled back so memory and disk never diverge.
func (s *FileStore) mutate(fn func(st *fileState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	backup := cloneState(s.st)
	fn(&s.st)
	if err := s.flushLocked(); err != nil {
		s.st = backup
		return errs.Wrap(err, errs.KindInternal, "state_write_failed", "persist state")
	}
	return nil
}

func cloneState(st fileState) fileState {
	out := fileState{
		Version:    st.Version,
		Hosts:      make(map[string]model.Host, len(st.Hosts)),
		Containers: make(map[string]model.Container, len(st.Containers)),
		Operations: make(map[string]model.Operation, len(st.Operations)),
	}
	for k, v := range st.Hosts {
		out.Hosts[k] = v
	}
	for k, v := range st.Containers {
		out.Containers[k] = v
	}
	for k, v := range st.Operations {
		out.Operations[k] = v
	}
	return out
}

func copyHost(h model.Host) model.Host {
	if h.Labels != nil {
		l := make(map[string]string, len(h.Labels))
		for k, v := range h.Labels {
			l[k] = v
		}
		h.Labels = l
	}
	h.ClusterMembers = slices.Clone(h.ClusterMembers)
	return h
}

func copyContainer(c model.Container) model.Container {
	c.Profiles = slices.Clone(c.Profiles)
	c.IPv4 = slices.Clone(c.IPv4)
	c.IPv6 = slices.Clone(c.IPv6)
	return c
}

// ListHosts implements Store.
func (s *FileStore) ListHosts(_ context.Context) ([]model.Host, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Host, 0, len(s.st.Hosts))
	for _, h := range s.st.Hosts {
		out = append(out, copyHost(h))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// GetHost implements Store.
func (s *FileStore) GetHost(_ context.Context, id string) (model.Host, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.st.Hosts[id]
	if !ok {
		return model.Host{}, errs.NotFound("host_not_found", "host %q not found", id)
	}
	return copyHost(h), nil
}

// PutHost implements Store.
func (s *FileStore) PutHost(_ context.Context, h model.Host) error {
	return s.mutate(func(st *fileState) { st.Hosts[h.ID] = copyHost(h) })
}

// DeleteHost implements Store.
func (s *FileStore) DeleteHost(_ context.Context, id string) error {
	return s.mutate(func(st *fileState) { delete(st.Hosts, id) })
}

// ListContainers implements Store.
func (s *FileStore) ListContainers(_ context.Context, f ContainerFilter) ([]model.Container, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []model.Container{}
	for _, c := range s.st.Containers {
		if f.HostID != "" && c.HostID != f.HostID {
			continue
		}
		if f.OwnerID != "" && c.OwnerID != f.OwnerID {
			continue
		}
		if f.Tier != "" && !strings.EqualFold(c.Tier, f.Tier) {
			continue
		}
		out = append(out, copyContainer(c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// GetContainer implements Store.
func (s *FileStore) GetContainer(_ context.Context, id string) (model.Container, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.st.Containers[id]
	if !ok {
		return model.Container{}, errs.NotFound("container_not_found", "container %q not found", id)
	}
	return copyContainer(c), nil
}

// FindContainerByIdempotencyKey implements Store.
func (s *FileStore) FindContainerByIdempotencyKey(_ context.Context, key string) (model.Container, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if key != "" {
		for _, c := range s.st.Containers {
			if c.IdempotencyKey == key {
				return copyContainer(c), nil
			}
		}
	}
	return model.Container{}, errs.NotFound("container_not_found", "no container for idempotency key")
}

// PutContainer implements Store.
func (s *FileStore) PutContainer(_ context.Context, c model.Container) error {
	return s.mutate(func(st *fileState) { st.Containers[c.ID] = copyContainer(c) })
}

// DeleteContainer implements Store.
func (s *FileStore) DeleteContainer(_ context.Context, id string) error {
	return s.mutate(func(st *fileState) { delete(st.Containers, id) })
}

// ListOperations implements Store.
func (s *FileStore) ListOperations(_ context.Context) ([]model.Operation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Operation, 0, len(s.st.Operations))
	for _, op := range s.st.Operations {
		out = append(out, op)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// PutOperation implements Store.
func (s *FileStore) PutOperation(_ context.Context, op model.Operation) error {
	return s.mutate(func(st *fileState) { st.Operations[op.ID] = op })
}

// DeleteOperation implements Store.
func (s *FileStore) DeleteOperation(_ context.Context, id string) error {
	return s.mutate(func(st *fileState) { delete(st.Operations, id) })
}

// Ping implements Store.
func (s *FileStore) Ping(_ context.Context) error {
	if s.path == "" {
		return nil
	}
	_, err := os.Stat(filepath.Dir(s.path))
	return err
}

// Close implements Store.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

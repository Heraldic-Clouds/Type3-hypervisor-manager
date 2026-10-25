package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
)

func TestFileStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "state.json")
	ctx := context.Background()
	s, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutHost(ctx, model.Host{ID: "h1", Labels: map[string]string{"az": "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutContainer(ctx, model.Container{ID: "c1", HostID: "h1", OwnerID: "o", IdempotencyKey: "key-12345"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutOperation(ctx, model.Operation{ID: "op1", Status: model.OpRunning}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v %v", fi.Mode(), err)
	}

	s2, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s2.GetHost(ctx, "h1")
	if err != nil || h.Labels["az"] != "a" {
		t.Fatalf("host after reload = %+v %v", h, err)
	}
	if c, err := s2.FindContainerByIdempotencyKey(ctx, "key-12345"); err != nil || c.ID != "c1" {
		t.Fatalf("idempotency lookup = %+v %v", c, err)
	}
	if cs, _ := s2.ListContainers(ctx, ContainerFilter{HostID: "other"}); len(cs) != 0 {
		t.Fatal("filter by host failed")
	}
	if ops, _ := s2.ListOperations(ctx); len(ops) != 1 {
		t.Fatal("operation not persisted")
	}
	if err := s2.DeleteHost(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.GetHost(ctx, "h1"); !errs.Is(err, errs.KindNotFound) {
		t.Fatalf("deleted host: %v", err)
	}
}

func TestStoreReturnsCopies(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	_ = s.PutHost(ctx, model.Host{ID: "h1", Labels: map[string]string{"a": "1"}})
	h, _ := s.GetHost(ctx, "h1")
	h.Labels["a"] = "mutated"
	h2, _ := s.GetHost(ctx, "h1")
	if h2.Labels["a"] != "1" {
		t.Fatal("store leaked internal map")
	}
}

func TestCorruptStateRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := OpenFileStore(path); err == nil {
		t.Fatal("corrupt state accepted")
	}
}

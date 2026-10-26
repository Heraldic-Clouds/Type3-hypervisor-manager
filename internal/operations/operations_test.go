package operations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/logging"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
)

func wait(t *testing.T, m *Manager, id string) model.Operation {
	t.Helper()
	for i := 0; i < 500; i++ {
		op, _ := m.Get(context.Background(), id)
		if op.Status.Final() {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout")
	return model.Operation{}
}

func TestSubmitSuccessFailurePanic(t *testing.T) {
	m := NewManager(store.NewMemoryStore(), Options{}, logging.Discard())
	ok, _, _ := m.Submit(Spec{Type: "t", ResourceType: "r", ResourceID: "1"}, func(ctx context.Context, p *Progress) (map[string]any, error) {
		p.Stage("working", 50)
		return map[string]any{"x": 1}, nil
	})
	if op := wait(t, m, ok.ID); op.Status != model.OpSucceeded || op.Progress != 100 || op.Result["x"] != 1 {
		t.Fatalf("success op = %+v", op)
	}
	bad, _, _ := m.Submit(Spec{Type: "t", ResourceType: "r", ResourceID: "2"}, func(context.Context, *Progress) (map[string]any, error) {
		return nil, errs.Conflict("boom", "nope")
	})
	if op := wait(t, m, bad.ID); op.Status != model.OpFailed || op.ErrorCode != "boom" {
		t.Fatalf("failed op = %+v", op)
	}
	pan, _, _ := m.Submit(Spec{Type: "t", ResourceType: "r", ResourceID: "3"}, func(context.Context, *Progress) (map[string]any, error) {
		panic("kaboom")
	})
	if op := wait(t, m, pan.ID); op.Status != model.OpFailed {
		t.Fatalf("panic op = %+v", op)
	}
}

func TestTimeoutAndCancel(t *testing.T) {
	m := NewManager(store.NewMemoryStore(), Options{}, logging.Discard())
	block := func(ctx context.Context, _ *Progress) (map[string]any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	op, _, _ := m.Submit(Spec{Type: "t", ResourceType: "r", ResourceID: "1", Timeout: 20 * time.Millisecond}, block)
	if done := wait(t, m, op.ID); done.ErrorCode != "operation_timeout" {
		t.Fatalf("timeout op = %+v", done)
	}
	op, _, _ = m.Submit(Spec{Type: "t", ResourceType: "r", ResourceID: "2"}, block)
	if _, err := m.Cancel(context.Background(), op.ID); err != nil {
		t.Fatal(err)
	}
	if done := wait(t, m, op.ID); done.Status != model.OpCancelled {
		t.Fatalf("cancelled op = %+v", done)
	}
	if _, err := m.Cancel(context.Background(), op.ID); !errs.Is(err, errs.KindConflict) {
		t.Fatalf("cancel finished: %v", err)
	}
}

func TestIdempotencyAndExclusivity(t *testing.T) {
	m := NewManager(store.NewMemoryStore(), Options{}, logging.Discard())
	release := make(chan struct{})
	task := func(context.Context, *Progress) (map[string]any, error) { <-release; return nil, nil }
	first, existed, err := m.Submit(Spec{Type: "t", ResourceType: "r", ResourceID: "1", IdempotencyKey: "k-1234567"}, task)
	if err != nil || existed {
		t.Fatal(err)
	}
	again, existed, err := m.Submit(Spec{Type: "t", ResourceType: "r", ResourceID: "1", IdempotencyKey: "k-1234567"}, task)
	if err != nil || !existed || again.ID != first.ID {
		t.Fatalf("idempotent submit = %v %v %v", again.ID, existed, err)
	}
	if _, _, err := m.Submit(Spec{Type: "other", ResourceType: "r", ResourceID: "1"}, task); !errs.Is(err, errs.KindConflict) {
		t.Fatalf("concurrent op on same resource: %v", err)
	}
	if _, _, err := m.Submit(Spec{Type: "x", ResourceType: "r", ResourceID: "9", IdempotencyKey: "k-1234567"}, task); !errs.Is(err, errs.KindConflict) {
		t.Fatalf("reused key: %v", err)
	}
	close(release)
	wait(t, m, first.ID)
}

func TestRecoverMarksInterrupted(t *testing.T) {
	st := store.NewMemoryStore()
	_ = st.PutOperation(context.Background(), model.Operation{ID: "op-1", Status: model.OpRunning})
	_ = st.PutOperation(context.Background(), model.Operation{ID: "op-2", Status: model.OpSucceeded})
	m := NewManager(st, Options{}, logging.Discard())
	got, err := m.Recover(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "op-1" {
		t.Fatalf("Recover = %v %v", got, err)
	}
	op, _ := m.Get(context.Background(), "op-1")
	if op.Status != model.OpFailed || op.ErrorCode != "interrupted" {
		t.Fatalf("op-1 = %+v", op)
	}
}

func TestShutdownCancelsAfterDeadline(t *testing.T) {
	m := NewManager(store.NewMemoryStore(), Options{}, logging.Discard())
	op, _, _ := m.Submit(Spec{Type: "t", ResourceType: "r", ResourceID: "1"}, func(ctx context.Context, _ *Progress) (map[string]any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v", err)
	}
	if got, _ := m.Get(context.Background(), op.ID); !got.Status.Final() {
		t.Fatalf("op not finalised: %s", got.Status)
	}
	if _, _, err := m.Submit(Spec{Type: "t", ResourceType: "r", ResourceID: "2"}, nil); !errs.Is(err, errs.KindUnavailable) {
		t.Fatalf("submit after shutdown: %v", err)
	}
}

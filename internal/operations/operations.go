// Package operations runs long LXD work in the background and exposes its
// status, stage and progress, so API calls return immediately with an
// operation ID instead of blocking on LXD.
package operations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/lxdclient"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
)

// OperationManager is the contract used by the API and managers.
type OperationManager interface {
	Submit(spec Spec, task Task) (model.Operation, bool, error)
	Get(ctx context.Context, id string) (model.Operation, error)
	List(ctx context.Context, f Filter) []model.Operation
	Cancel(ctx context.Context, id string) (model.Operation, error)
	Active(resourceType, resourceID string) (model.Operation, bool)
}

// Spec describes an operation to run.
type Spec struct {
	Type           string
	ResourceType   string
	ResourceID     string
	HostID         string
	Actor          string
	IdempotencyKey string
	// Timeout overrides the manager default.
	Timeout time.Duration
}

// Task is the work of an operation. Returning an error fails the operation.
type Task func(ctx context.Context, p *Progress) (map[string]any, error)

// Filter narrows List.
type Filter struct {
	ResourceType string
	ResourceID   string
	Status       model.OperationStatus
}

// Options configures the Manager.
type Options struct {
	DefaultTimeout time.Duration
	Retention      time.Duration
	Now            func() time.Time
	// OnFinish is called after an operation reaches a final state.
	OnFinish func(model.Operation)
}

type entry struct {
	op     model.Operation
	cancel context.CancelFunc
	key    string
}

// Manager implements OperationManager.
type Manager struct {
	st  store.Store
	log *slog.Logger
	opt Options

	base       context.Context
	baseCancel context.CancelFunc
	wg         sync.WaitGroup

	mu       sync.Mutex
	ops      map[string]*entry
	byKey    map[string]string
	active   map[string]string // resourceType/resourceID -> op id
	stopping bool
}

// NewManager builds a manager. Call Recover once at startup.
func NewManager(st store.Store, opt Options, log *slog.Logger) *Manager {
	if opt.DefaultTimeout <= 0 {
		opt.DefaultTimeout = 15 * time.Minute
	}
	if opt.Retention <= 0 {
		opt.Retention = 24 * time.Hour
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		st: st, log: log, opt: opt, base: ctx, baseCancel: cancel,
		ops: map[string]*entry{}, byKey: map[string]string{}, active: map[string]string{},
	}
}

func (m *Manager) now() time.Time { return m.opt.Now().UTC() }

// NewID returns a random operation ID.
func NewID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "op-" + hex.EncodeToString(b)
}

func resKey(t, id string) string { return t + "/" + id }

// Recover loads persisted operations. Operations that were still running
// when Type3 stopped are marked failed so callers know to retry; the
// container reconciler then repairs the recorded state.
func (m *Manager) Recover(ctx context.Context) ([]model.Operation, error) {
	ops, err := m.st.ListOperations(ctx)
	if err != nil {
		return nil, err
	}
	var interrupted []model.Operation
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range ops {
		if !op.Status.Final() {
			now := m.now()
			op.Status = model.OpFailed
			op.ErrorCode = "interrupted"
			op.ErrorMessage = "Type3 restarted while the operation was running; state will be reconciled, retry if needed"
			op.UpdatedAt, op.FinishedAt = now, &now
			if err := m.st.PutOperation(ctx, op); err != nil {
				return nil, err
			}
			interrupted = append(interrupted, op)
		}
		m.ops[op.ID] = &entry{op: op}
	}
	return interrupted, nil
}

// Submit starts task in the background. If spec.IdempotencyKey matches an
// earlier operation of the same type and resource, that operation is
// returned with existed=true and nothing new runs. Only one operation may
// run per resource at a time.
func (m *Manager) Submit(spec Spec, task Task) (model.Operation, bool, error) {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return model.Operation{}, false, errs.Unavailable("shutting_down", "Type3 is shutting down")
	}
	if spec.IdempotencyKey != "" {
		if id, ok := m.byKey[spec.IdempotencyKey]; ok {
			if e, ok := m.ops[id]; ok {
				op := e.op
				m.mu.Unlock()
				if op.Type != spec.Type || op.ResourceID != spec.ResourceID {
					return model.Operation{}, false, errs.Conflict("idempotency_key_reused", "Idempotency-Key was already used for a different request")
				}
				return op, true, nil
			}
		}
	}
	rk := resKey(spec.ResourceType, spec.ResourceID)
	if id, busy := m.active[rk]; busy {
		op := m.ops[id].op
		m.mu.Unlock()
		return model.Operation{}, false, errs.Conflict("operation_in_progress",
			"%s %q already has a %s operation in progress", spec.ResourceType, spec.ResourceID, op.Type).
			WithDetails(map[string]any{"operation_id": op.ID})
	}

	now := m.now()
	op := model.Operation{
		ID: NewID(), Type: spec.Type, Status: model.OpPending,
		ResourceType: spec.ResourceType, ResourceID: spec.ResourceID, HostID: spec.HostID,
		Actor: spec.Actor, Progress: -1, CreatedAt: now, UpdatedAt: now,
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = m.opt.DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(m.base, timeout)
	e := &entry{op: op, cancel: cancel, key: spec.IdempotencyKey}
	m.ops[op.ID] = e
	m.active[rk] = op.ID
	if spec.IdempotencyKey != "" {
		m.byKey[spec.IdempotencyKey] = op.ID
	}
	m.wg.Add(1)
	m.mu.Unlock()

	if err := m.st.PutOperation(ctx, op); err != nil {
		m.log.Error("persist operation", "op", op.ID, "err", err)
	}
	go m.run(ctx, cancel, op.ID, rk, task)
	return op, false, nil
}

func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, id, rk string, task Task) {
	defer m.wg.Done()
	defer cancel()

	m.update(id, true, func(op *model.Operation) { op.Status = model.OpRunning })
	p := &Progress{m: m, id: id}

	var result map[string]any
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("operation panicked: %v", r)
				m.log.Error("operation panic", "op", id, "panic", r)
			}
		}()
		result, err = task(ctx, p)
	}()

	final := m.update(id, true, func(op *model.Operation) {
		now := m.now()
		op.FinishedAt = &now
		op.Result = result
		switch {
		case err == nil:
			op.Status = model.OpSucceeded
			op.Progress = 100
			op.Stage = "done"
		case errors.Is(err, context.Canceled) && ctx.Err() == context.Canceled:
			op.Status = model.OpCancelled
			op.ErrorCode = "cancelled"
			op.ErrorMessage = "operation was cancelled"
		default:
			op.Status = model.OpFailed
			op.ErrorCode, op.ErrorMessage = classify(err, ctx)
		}
	})
	m.mu.Lock()
	if m.active[rk] == id {
		delete(m.active, rk)
	}
	m.mu.Unlock()

	lvl := slog.LevelInfo
	if final.Status != model.OpSucceeded {
		lvl = slog.LevelWarn
	}
	m.log.Log(context.Background(), lvl, "operation finished", "op", id, "type", final.Type,
		"resource", final.ResourceID, "status", final.Status, "error_code", final.ErrorCode, "error", final.ErrorMessage)
	if m.opt.OnFinish != nil {
		m.opt.OnFinish(final)
	}
}

func classify(err error, ctx context.Context) (string, string) {
	if errors.Is(err, lxdclient.ErrOperationTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "operation_timeout", "operation exceeded its timeout: " + err.Error()
	}
	if e, ok := errs.As(err); ok {
		return e.Code, e.Error()
	}
	return "lxd_error", err.Error()
}

// update mutates an operation; persist=true writes it to the store.
func (m *Manager) update(id string, persist bool, fn func(*model.Operation)) model.Operation {
	m.mu.Lock()
	e, ok := m.ops[id]
	if !ok {
		m.mu.Unlock()
		return model.Operation{}
	}
	fn(&e.op)
	e.op.UpdatedAt = m.now()
	op := e.op
	m.mu.Unlock()
	if persist {
		if err := m.st.PutOperation(context.Background(), op); err != nil {
			m.log.Error("persist operation", "op", id, "err", err)
		}
	}
	return op
}

// Get returns an operation.
func (m *Manager) Get(_ context.Context, id string) (model.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.ops[id]
	if !ok {
		return model.Operation{}, errs.NotFound("operation_not_found", "operation %q not found", id)
	}
	return e.op, nil
}

// List returns operations, newest first.
func (m *Manager) List(_ context.Context, f Filter) []model.Operation {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []model.Operation{}
	for _, e := range m.ops {
		if f.ResourceType != "" && e.op.ResourceType != f.ResourceType {
			continue
		}
		if f.ResourceID != "" && e.op.ResourceID != f.ResourceID {
			continue
		}
		if f.Status != "" && e.op.Status != f.Status {
			continue
		}
		out = append(out, e.op)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Active returns the in-flight operation for a resource, if any.
func (m *Manager) Active(resourceType, resourceID string) (model.Operation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.active[resKey(resourceType, resourceID)]
	if !ok {
		return model.Operation{}, false
	}
	return m.ops[id].op, true
}

// Cancel requests cancellation of a running operation.
func (m *Manager) Cancel(_ context.Context, id string) (model.Operation, error) {
	m.mu.Lock()
	e, ok := m.ops[id]
	if !ok {
		m.mu.Unlock()
		return model.Operation{}, errs.NotFound("operation_not_found", "operation %q not found", id)
	}
	if e.op.Status.Final() {
		op := e.op
		m.mu.Unlock()
		return op, errs.Conflict("operation_finished", "operation %q already finished", id)
	}
	cancel := e.cancel
	op := e.op
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return op, nil
}

// Prune drops finished operations older than the retention period.
func (m *Manager) Prune(ctx context.Context) int {
	cutoff := m.now().Add(-m.opt.Retention)
	m.mu.Lock()
	var drop []string
	for id, e := range m.ops {
		if e.op.Status.Final() && e.op.FinishedAt != nil && e.op.FinishedAt.Before(cutoff) {
			drop = append(drop, id)
			delete(m.ops, id)
			if e.key != "" {
				delete(m.byKey, e.key)
			}
		}
	}
	m.mu.Unlock()
	for _, id := range drop {
		_ = m.st.DeleteOperation(ctx, id)
	}
	return len(drop)
}

// Counts returns operation counts by status.
func (m *Manager) Counts() map[model.OperationStatus]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[model.OperationStatus]int{}
	for _, e := range m.ops {
		out[e.op.Status]++
	}
	return out
}

// Shutdown stops accepting work, waits for running operations until ctx
// ends, then cancels whatever is left and waits for it to record its state.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()

	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		m.log.Warn("cancelling operations still running at shutdown")
		m.baseCancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		return ctx.Err()
	}
}

// Progress lets a task report what it is doing.
type Progress struct {
	m  *Manager
	id string
}

// ID returns the operation ID.
func (p *Progress) ID() string { return p.id }

// Stage sets the current stage and percentage (-1 if unknown).
func (p *Progress) Stage(stage string, pct int) {
	p.m.update(p.id, false, func(op *model.Operation) {
		op.Stage = stage
		op.Progress = pct
		op.ProgressDetail = ""
	})
}

// LXD records the underlying LXD operation and its progress metadata.
func (p *Progress) LXD(lxdOpID string, detail string) {
	p.m.update(p.id, false, func(op *model.Operation) {
		if lxdOpID != "" {
			op.LXDOperation = lxdOpID
		}
		if detail != "" {
			op.ProgressDetail = detail
		}
	})
}

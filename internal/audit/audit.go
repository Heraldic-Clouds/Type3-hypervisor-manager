// Package audit writes an append-only JSON-lines record of privileged and
// destructive actions: who did what to which resource, and the outcome.
package audit

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/logging"
)

// Event is one audit record.
type Event struct {
	Actor        string
	Role         string
	Action       string
	ResourceType string
	ResourceID   string
	Outcome      string // accepted, succeeded, failed, denied
	RequestID    string
	RemoteAddr   string
	Reason       string
	OperationID  string
	Error        string
	Details      map[string]any
}

// Logger writes audit events.
type Logger struct {
	mu     sync.Mutex
	log    *slog.Logger
	closer io.Closer
}

// Open writes audit events to path (created 0600, appended) or to w when
// path is empty.
func Open(path string, w io.Writer) (*Logger, error) {
	l := &Logger{}
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		w, l.closer = f, f
	}
	l.log = slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{ReplaceAttr: logging.Redact}))
	return l, nil
}

// Discard returns a Logger that drops events.
func Discard() *Logger {
	return &Logger{log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// Record writes an event.
func (l *Logger) Record(ctx context.Context, e Event) {
	attrs := []any{
		"audit", true, "ts", time.Now().UTC().Format(time.RFC3339Nano),
		"actor", e.Actor, "role", e.Role, "action", e.Action,
		"resource_type", e.ResourceType, "resource_id", e.ResourceID, "outcome", e.Outcome,
	}
	for k, v := range map[string]string{
		"request_id": e.RequestID, "remote_addr": e.RemoteAddr, "reason": e.Reason,
		"operation_id": e.OperationID, "error": e.Error,
	} {
		if v != "" {
			attrs = append(attrs, k, v)
		}
	}
	if len(e.Details) > 0 {
		attrs = append(attrs, "details", e.Details)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.log.InfoContext(ctx, "audit", attrs...)
}

// Close closes the underlying file.
func (l *Logger) Close() error {
	if l.closer != nil {
		return l.closer.Close()
	}
	return nil
}

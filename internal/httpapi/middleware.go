package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/audit"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/auth"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/errs"
)

type ridKey struct{}

type actorKey struct{}

// actorSlot lets the auth middleware report the caller to the outer logging
// middleware, which only sees the original request.
type actorSlot struct{ name string }

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

func requestIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(ridKey{}).(string)
	return s
}

// requestID propagates a safe X-Request-ID or generates one.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !requestIDRe.MatchString(id) {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = "req-" + hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), ridKey{}, id)
		ctx = context.WithValue(ctx, actorKey{}, &actorSlot{})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'")
		next.ServeHTTP(w, r)
	})
}

func limitBody(n int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, n)
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				a.d.Log.Error("panic in handler", "panic", v, "path", r.URL.Path, "request_id", requestIDFrom(r.Context()))
				writeError(w, r, errs.New(errs.KindInternal, "internal_error", "internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// observe logs each request and counts it by route pattern and status.
func (a *API) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		pattern := r.Pattern
		if pattern == "" {
			pattern = "unmatched"
		}
		a.d.Metrics.Inc("type3_http_requests_total", "HTTP requests by route and status code.",
			map[string]string{"route": pattern, "code": strconv.Itoa(sw.status)})
		actor := ""
		if slot, ok := r.Context().Value(actorKey{}).(*actorSlot); ok {
			actor = slot.name
		}
		a.d.Log.Info("http request", "method", r.Method, "route", pattern, "path", r.URL.Path,
			"status", sw.status, "duration_ms", time.Since(start).Milliseconds(), "actor", actor,
			"remote", remoteIP(r), "request_id", requestIDFrom(r.Context()))
	})
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authenticate requires a valid API key with at least role min.
func (a *API) authenticate(min auth.Role, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.d.Auth.Authenticate(auth.TokenFromRequest(r))
		if !ok {
			a.d.Metrics.Inc("type3_auth_failures_total", "Rejected API requests by reason.", map[string]string{"reason": "unauthenticated"})
			a.d.Audit.Record(r.Context(), audit.Event{
				Action: r.Pattern, Outcome: "denied", Error: "unauthenticated",
				RequestID: requestIDFrom(r.Context()), RemoteAddr: remoteIP(r),
			})
			w.Header().Set("WWW-Authenticate", `Bearer realm="type3"`)
			writeError(w, r, errs.New(errs.KindUnauthorized, "unauthenticated", "a valid API key is required"))
			return
		}
		if p.Role < min {
			a.d.Metrics.Inc("type3_auth_failures_total", "Rejected API requests by reason.", map[string]string{"reason": "forbidden"})
			a.d.Audit.Record(r.Context(), audit.Event{
				Actor: p.Name, Role: p.Role.String(), Action: r.Pattern, Outcome: "denied",
				Error: "requires " + min.String(), RequestID: requestIDFrom(r.Context()), RemoteAddr: remoteIP(r),
			})
			writeError(w, r, errs.New(errs.KindForbidden, "forbidden", "this action requires the %s role", min))
			return
		}
		if slot, ok := r.Context().Value(actorKey{}).(*actorSlot); ok {
			slot.name = p.Name
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

func principal(r *http.Request) auth.Principal {
	p, _ := auth.FromContext(r.Context())
	return p
}

// requireRole is used for actions whose required role depends on the body
// (e.g. forced stop needs admin).
func requireRole(r *http.Request, min auth.Role, what string) error {
	if principal(r).Role < min {
		return errs.New(errs.KindForbidden, "forbidden", "%s requires the %s role", what, min)
	}
	return nil
}

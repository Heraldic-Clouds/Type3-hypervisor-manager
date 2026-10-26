package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/security"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Config struct {
	Origin, GuacKey                                                 string
	Secure, RequireVerification, Local                              bool
	Cap, IdleSeconds, WarningSeconds, GraceSeconds, LifetimeSeconds int
}
type Server struct {
	Store  *store.Store
	Config Config
	Infra  Infra
	dummy  string
}
type auth struct {
	User       store.User
	Hash, CSRF string
}
type HTTPError struct {
	Code    int
	Message string
}

func (e HTTPError) Error() string     { return e.Message }
func fail(code int, msg string) error { return HTTPError{code, msg} }
func New(s *store.Store, c Config) *Server {
	dummy, _ := security.Password(security.Random(24))
	return &Server{Store: s, Config: c, dummy: dummy}
}
func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16*1024))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return fail(400, "Invalid request")
	}
	return nil
}
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	e := s.route(w, r)
	if e != nil {
		var he HTTPError
		if errors.As(e, &he) {
			write(w, he.Code, map[string]any{"error": he.Message})
			return
		}
		slog.Error("request failed", "path", r.URL.Path, "error", e)
		write(w, 503, map[string]string{"error": "Service temporarily unavailable. Please try again."})
	}
}
func (s *Server) authenticate(r *http.Request) (auth, error) {
	var a auth
	c, e := r.Cookie("mea_session")
	if e != nil {
		return a, fail(401, "Please sign in")
	}
	a.Hash = security.Hash(c.Value)
	e = s.Store.DB.QueryRow(r.Context(), `SELECT u.id::text,u.username,u.email,u.name,u.verified,u.subscription,u.entity_id,s.csrf
 FROM mea.sessions s JOIN mea.users u ON u.id=s.user_id WHERE s.hash=$1 AND s.revoked_at IS NULL AND s.expires_at>now() AND NOT u.disabled`, a.Hash).Scan(&a.User.ID, &a.User.Username, &a.User.Email, &a.User.Name, &a.User.Verified, &a.User.Subscription, &a.User.EntityID, &a.CSRF)
	if errors.Is(e, pgx.ErrNoRows) {
		return a, fail(401, "Your session has expired. Please sign in.")
	}
	return a, e
}
func (s *Server) origin(r *http.Request) bool { return r.Header.Get("Origin") == s.Config.Origin }
func (s *Server) route(w http.ResponseWriter, r *http.Request) error {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "/api/health" {
		if e := s.Store.DB.Ping(r.Context()); e != nil {
			return e
		}
		write(w, 200, map[string]string{"status": "ok"})
		return nil
	}
	if r.Method != "GET" && !s.origin(r) {
		return fail(403, "Invalid request origin")
	}
	if strings.HasPrefix(path, "/api/auth/") {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip == "" {
			ip = r.RemoteAddr
		}
		if !s.Store.Rate(r.Context(), "ip:"+security.Hash(ip), 40, 15*time.Minute) {
			return fail(429, "Too many attempts. Try again later.")
		}
		switch path {
		case "/api/auth/register":
			if r.Method == "POST" {
				return s.register(w, r)
			}
		case "/api/auth/login":
			if r.Method == "POST" {
				return s.login(w, r)
			}
		case "/api/auth/forgot":
			if r.Method == "POST" {
				return s.forgot(w, r)
			}
		case "/api/auth/reset":
			if r.Method == "POST" {
				return s.reset(w, r)
			}
		case "/api/auth/verify":
			if r.Method == "POST" {
				return s.verify(w, r)
			}
		}
	}
	a, e := s.authenticate(r)
	if e != nil {
		return e
	}
	if r.Method != "GET" && r.Header.Get("X-CSRF-Token") != a.CSRF {
		return fail(403, "Invalid CSRF token")
	}
	switch path {
	case "/api/me":
		if r.Method == "GET" {
			access, e := beta.AccessFor(r.Context(), s.Store.DB, a.User.ID)
			if e != nil {
				return e
			}
			write(w, 200, map[string]any{"user": a.User, "access": access, "csrf": a.CSRF, "idleSeconds": s.Config.IdleSeconds, "warningSeconds": s.Config.WarningSeconds, "graceSeconds": s.Config.GraceSeconds})
			return nil
		}
	case "/api/logout":
		if r.Method == "POST" {
			_, e = s.Store.DB.Exec(r.Context(), `UPDATE mea.sessions SET revoked_at=now() WHERE hash=$1`, a.Hash)
			s.cookie(w, "", -1)
			write(w, 200, map[string]bool{"ok": true})
			return e
		}
	case "/api/account/sessions":
		if r.Method == "GET" {
			rows, e := s.Store.DB.Query(r.Context(), `SELECT hash,created_at,expires_at,user_agent FROM mea.sessions WHERE user_id=$1 AND revoked_at IS NULL AND expires_at>now() ORDER BY created_at DESC`, a.User.ID)
			if e != nil {
				return e
			}
			defer rows.Close()
			sessions := []map[string]any{}
			for rows.Next() {
				var hash, ua string
				var c, x time.Time
				if e = rows.Scan(&hash, &c, &x, &ua); e != nil {
					return e
				}
				sessions = append(sessions, map[string]any{"id": hash, "createdAt": c, "expiresAt": x, "userAgent": ua, "current": hash == a.Hash})
			}
			if e = rows.Err(); e != nil {
				return e
			}
			write(w, 200, sessions)
			return nil
		}
		if r.Method == "DELETE" {
			var b struct {
				ID string `json:"id"`
			}
			if e = decode(r, &b); e != nil {
				return e
			}
			_, e = s.Store.DB.Exec(r.Context(), `UPDATE mea.sessions SET revoked_at=now() WHERE hash=$1 AND user_id=$2`, b.ID, a.User.ID)
			write(w, 200, map[string]bool{"ok": true})
			return e
		}
	case "/api/account/password":
		if r.Method == "POST" {
			return s.changePassword(w, r, a)
		}
	case "/api/account":
		if r.Method == "DELETE" {
			return s.deleteAccount(w, r, a)
		}
	case "/api/account/verify":
		if r.Method == "POST" {
			if a.User.Verified {
				write(w, 200, map[string]bool{"ok": true})
				return nil
			}
			return s.sendVerification(w, r, a)
		}
	case "/api/desktops":
		if r.Method == "GET" {
			d, e := store.ScanDesktop(s.Store.DB.QueryRow(r.Context(), "SELECT "+store.DesktopColumns+" FROM mea.desktops WHERE user_id=$1 ORDER BY created_at DESC LIMIT 1", a.User.ID))
			if errors.Is(e, pgx.ErrNoRows) {
				write(w, 200, map[string]any{"desktop": nil})
				return nil
			}
			if e != nil {
				return e
			}
			write(w, 200, map[string]any{"desktop": d})
			return nil
		}
		if r.Method == "POST" {
			return s.create(w, r, a)
		}
	}
	if strings.HasPrefix(path, "/api/admin/") {
		return s.admin(w, r, a, path)
	}
	if strings.HasPrefix(path, "/api/desktops/") {
		p := strings.Split(strings.TrimPrefix(path, "/api/desktops/"), "/")
		if len(p) > 2 {
			return fail(404, "Not found")
		}
		if _, e := url.Parse(p[0]); e != nil {
			return fail(404, "Desktop not found")
		}
		if !uuid.MatchString(p[0]) {
			return fail(404, "Desktop not found")
		}
		d, e := s.Store.Desktop(r.Context(), a.User.ID, p[0])
		if errors.Is(e, pgx.ErrNoRows) {
			return fail(404, "Desktop not found")
		}
		if e != nil {
			return e
		}
		if len(p) == 1 && r.Method == "DELETE" {
			return s.destroy(w, r, a, d)
		}
		if len(p) == 2 && r.Method == "POST" {
			switch p[1] {
			case "connect":
				return s.connect(w, r, a, d)
			case "activity":
				return s.activity(w, r, a, d)
			case "disconnect":
				return s.disconnect(w, r, a, d)
			}
		}
	}
	return fail(404, "Not found")
}

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var username = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,31}$`)

func (s *Server) cookie(w http.ResponseWriter, value string, max int) {
	http.SetCookie(w, &http.Cookie{Name: "mea_session", Value: value, Path: "/", HttpOnly: true, Secure: s.Config.Secure, SameSite: http.SameSiteStrictMode, MaxAge: max})
}
func (s *Server) session(ctx context.Context, tx pgx.Tx, user, ua string) (string, string, error) {
	raw := security.Random(32)
	csrf := security.Random(32)
	_, e := tx.Exec(ctx, `INSERT INTO mea.sessions(hash,user_id,csrf,expires_at,user_agent) VALUES($1,$2,$3,now()+interval '24 hours',$4)`, security.Hash(raw), user, csrf, ua)
	return raw, csrf, e
}
func (s *Server) token(ctx context.Context, tx pgx.Tx, user, email, kind string) error {
	raw := security.Random(32)
	hash := security.Hash(raw)
	_, e := tx.Exec(ctx, `INSERT INTO mea.tokens(hash,user_id,kind,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, hash, user, kind)
	if e != nil {
		return e
	}
	b, _ := json.Marshal(map[string]string{"email": email, "kind": kind, "token": raw})
	_, e = tx.Exec(ctx, `INSERT INTO mea.outbox(id,kind,payload) VALUES($1,'email',$2)`, security.ID(), b)
	return e
}
func (s *Server) register(w http.ResponseWriter, r *http.Request) error {
	var b struct{ Username, Email, Name, Password string }
	if e := decode(r, &b); e != nil {
		return e
	}
	b.Username = strings.ToLower(strings.TrimSpace(b.Username))
	b.Email = strings.ToLower(strings.TrimSpace(b.Email))
	b.Name = strings.TrimSpace(b.Name)
	if !username.MatchString(b.Username) || len(b.Email) > 254 || len(b.Name) < 1 || len(b.Name) > 100 {
		return fail(400, "Enter a name, email and username (3–32 lowercase letters, digits, underscores or hyphens)")
	}
	if addr, e := mail.ParseAddress(b.Email); e != nil || addr.Address != b.Email {
		return fail(400, "Enter a valid email address")
	}
	hash, e := security.Password(b.Password)
	if e != nil {
		return fail(400, e.Error())
	}
	tx, e := s.Store.DB.Begin(r.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(r.Context())
	user := security.ID()
	var entity, guac int
	e = tx.QueryRow(r.Context(), `INSERT INTO guacamole_entity(name,type) VALUES($1,'USER') RETURNING entity_id`, b.Username).Scan(&entity)
	if e == nil {
		e = tx.QueryRow(r.Context(), `INSERT INTO guacamole_user(entity_id,password_hash,password_salt,password_date,email_address,full_name) VALUES($1,$2,$3,now(),$4,$5) RETURNING user_id`, entity, []byte(security.Random(32))[:32], []byte(security.Random(32))[:32], b.Email, b.Name).Scan(&guac)
	}
	if e == nil {
		_, e = tx.Exec(r.Context(), `INSERT INTO mea.users(id,guac_user_id,entity_id,username,email,name,password_hash,verified) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, user, guac, entity, b.Username, b.Email, b.Name, hash, !s.Config.RequireVerification)
	}
	if e != nil {
		var pe *pgconn.PgError
		if errors.As(e, &pe) && pe.Code == "23505" {
			return fail(409, "Username or email is already registered")
		}
		return e
	}
	if s.Config.RequireVerification {
		if e = s.token(r.Context(), tx, user, b.Email, "verify"); e != nil {
			return e
		}
	}
	raw, csrf, e := s.session(r.Context(), tx, user, r.UserAgent())
	if e != nil {
		return e
	}
	if e = tx.Commit(r.Context()); e != nil {
		return e
	}
	s.cookie(w, raw, 86400)
	write(w, 201, map[string]any{"csrf": csrf, "verificationRequired": s.Config.RequireVerification})
	return nil
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	var b struct{ Email, Password string }
	if e := decode(r, &b); e != nil {
		return e
	}
	b.Email = strings.ToLower(strings.TrimSpace(b.Email))
	if !s.Store.Rate(r.Context(), "login:"+security.Hash(b.Email), 10, 15*time.Minute) {
		return fail(429, "Too many attempts. Try again in 15 minutes.")
	}
	var id, hash string
	e := s.Store.DB.QueryRow(r.Context(), `SELECT id::text,password_hash FROM mea.users WHERE email=$1 AND NOT disabled`, b.Email).Scan(&id, &hash)
	if errors.Is(e, pgx.ErrNoRows) {
		security.Verify(s.dummy, b.Password)
		return fail(401, "Email or password is incorrect")
	}
	if e != nil {
		return e
	}
	if !security.Verify(hash, b.Password) {
		return fail(401, "Email or password is incorrect")
	}
	tx, e := s.Store.DB.Begin(r.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(r.Context())
	raw, csrf, e := s.session(r.Context(), tx, id, r.UserAgent())
	if e != nil {
		return e
	}
	if e = tx.Commit(r.Context()); e != nil {
		return e
	}
	s.cookie(w, raw, 86400)
	write(w, 200, map[string]string{"csrf": csrf})
	return nil
}
func (s *Server) forgot(w http.ResponseWriter, r *http.Request) error {
	var b struct{ Email string }
	if e := decode(r, &b); e != nil {
		return e
	}
	var id, email string
	tx, e := s.Store.DB.Begin(r.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(r.Context())
	e = tx.QueryRow(r.Context(), `SELECT id::text,email FROM mea.users WHERE email=$1 AND NOT disabled`, strings.ToLower(strings.TrimSpace(b.Email))).Scan(&id, &email)
	if e == nil && s.Store.Rate(r.Context(), "reset:"+security.Hash(email), 3, time.Hour) {
		e = s.token(r.Context(), tx, id, email, "reset")
	}
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return e
	}
	if e = tx.Commit(r.Context()); e != nil {
		return e
	}
	write(w, 200, map[string]string{"message": "If an account exists, password reset instructions will be emailed."})
	return nil
}
func (s *Server) verify(w http.ResponseWriter, r *http.Request) error {
	var b struct{ Token string }
	if e := decode(r, &b); e != nil {
		return e
	}
	tx, e := s.Store.DB.Begin(r.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(r.Context())
	var id string
	e = tx.QueryRow(r.Context(), `UPDATE mea.tokens SET used_at=now() WHERE hash=$1 AND kind='verify' AND used_at IS NULL AND expires_at>now() RETURNING user_id::text`, security.Hash(b.Token)).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return fail(400, "Verification link is invalid or expired")
	}
	if e != nil {
		return e
	}
	if _, e = tx.Exec(r.Context(), `UPDATE mea.users SET verified=true WHERE id=$1`, id); e != nil {
		return e
	}
	if e = tx.Commit(r.Context()); e != nil {
		return e
	}
	write(w, 200, map[string]bool{"ok": true})
	return nil
}
func (s *Server) reset(w http.ResponseWriter, r *http.Request) error {
	var b struct{ Token, Password string }
	if e := decode(r, &b); e != nil {
		return e
	}
	hash, e := security.Password(b.Password)
	if e != nil {
		return fail(400, e.Error())
	}
	tx, e := s.Store.DB.Begin(r.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(r.Context())
	var id string
	e = tx.QueryRow(r.Context(), `UPDATE mea.tokens SET used_at=now() WHERE hash=$1 AND kind='reset' AND used_at IS NULL AND expires_at>now() RETURNING user_id::text`, security.Hash(b.Token)).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return fail(400, "Reset link is invalid or expired")
	}
	if e != nil {
		return e
	}
	if _, e = tx.Exec(r.Context(), `UPDATE mea.users SET password_hash=$2 WHERE id=$1`, id, hash); e != nil {
		return e
	}
	if _, e = tx.Exec(r.Context(), `UPDATE mea.sessions SET revoked_at=now() WHERE user_id=$1`, id); e != nil {
		return e
	}
	if _, e = tx.Exec(r.Context(), `UPDATE mea.tokens SET used_at=now() WHERE user_id=$1 AND kind='reset'`, id); e != nil {
		return e
	}
	if e = tx.Commit(r.Context()); e != nil {
		return e
	}
	write(w, 200, map[string]bool{"ok": true})
	return nil
}
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, a auth) error {
	var b struct{ CurrentPassword, Password string }
	if e := decode(r, &b); e != nil {
		return e
	}
	var old string
	if e := s.Store.DB.QueryRow(r.Context(), `SELECT password_hash FROM mea.users WHERE id=$1`, a.User.ID).Scan(&old); e != nil {
		return e
	}
	if !security.Verify(old, b.CurrentPassword) {
		return fail(400, "Current password is incorrect")
	}
	hash, e := security.Password(b.Password)
	if e != nil {
		return fail(400, e.Error())
	}
	tx, e := s.Store.DB.Begin(r.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(r.Context())
	if _, e = tx.Exec(r.Context(), `UPDATE mea.users SET password_hash=$2 WHERE id=$1`, a.User.ID, hash); e != nil {
		return e
	}
	if _, e = tx.Exec(r.Context(), `UPDATE mea.sessions SET revoked_at=now() WHERE user_id=$1 AND hash<>$2`, a.User.ID, a.Hash); e != nil {
		return e
	}
	if e = tx.Commit(r.Context()); e != nil {
		return e
	}
	write(w, 200, map[string]bool{"ok": true})
	return nil
}
func (s *Server) sendVerification(w http.ResponseWriter, r *http.Request, a auth) error {
	if !s.Store.Rate(r.Context(), "verify:"+a.User.ID, 3, time.Hour) {
		return fail(429, "Please wait before requesting another email")
	}
	tx, e := s.Store.DB.Begin(r.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(r.Context())
	if e = s.token(r.Context(), tx, a.User.ID, a.User.Email, "verify"); e != nil {
		return e
	}
	if e = tx.Commit(r.Context()); e != nil {
		return e
	}
	write(w, 200, map[string]bool{"ok": true})
	return nil
}
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request, a auth) error {
	var b struct{ Password string }
	if e := decode(r, &b); e != nil {
		return e
	}
	var hash string
	if e := s.Store.DB.QueryRow(r.Context(), `SELECT password_hash FROM mea.users WHERE id=$1`, a.User.ID).Scan(&hash); e != nil {
		return e
	}
	if !security.Verify(hash, b.Password) {
		return fail(400, "Password is incorrect")
	}
	tx, e := s.Store.DB.Begin(r.Context())
	if e != nil {
		return e
	}
	defer tx.Rollback(r.Context())
	for _, q := range []string{`UPDATE mea.users SET disabled=true,name='Deleted account',email=id::text||'@deleted.invalid',password_hash='' WHERE id=$1`, `UPDATE mea.sessions SET revoked_at=now() WHERE user_id=$1`, `UPDATE mea.desktops SET state='terminating',disconnect_at=now() WHERE user_id=$1 AND state IN ('queued','provisioning','ready')`, `UPDATE guacamole_user SET disabled=true,email_address=NULL,full_name=NULL WHERE user_id=(SELECT guac_user_id FROM mea.users WHERE id=$1)`} {
		if _, e = tx.Exec(r.Context(), q, a.User.ID); e != nil {
			return e
		}
	}
	if e = tx.Commit(r.Context()); e != nil {
		return e
	}
	s.cookie(w, "", -1)
	write(w, 200, map[string]bool{"ok": true})
	return nil
}
func (s *Server) create(w http.ResponseWriter, r *http.Request, a auth) error {
	if s.Config.RequireVerification && !a.User.Verified {
		return fail(403, "Verify your email before creating a desktop")
	}
	ctx := r.Context()
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(739103)"); e != nil {
		return e
	}
	d, e := store.ScanDesktop(tx.QueryRow(ctx, "SELECT "+store.DesktopColumns+" FROM mea.desktops WHERE user_id=$1 AND state IN ('queued','provisioning','ready','terminating')", a.User.ID))
	if e == nil {
		write(w, 200, map[string]any{"desktop": d})
		return nil
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return e
	}
	var count int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM mea.desktops WHERE state IN ('queued','provisioning','ready','terminating')`).Scan(&count); e != nil {
		return e
	}
	if count >= s.Config.Cap {
		return fail(409, "All Newbie desktops are in use. Please try again soon.")
	}
	id, op := security.ID(), security.ID()
	d, e = store.ScanDesktop(tx.QueryRow(ctx, "INSERT INTO mea.desktops(id,user_id,state,operation_id,expires_at) VALUES($1,$2,'queued',$3,now()+$4::interval) RETURNING "+store.DesktopColumns, id, a.User.ID, op, fmt.Sprintf("%d seconds", s.Config.LifetimeSeconds)))
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO mea.audit(user_id,desktop_id,action) VALUES($1,$2,'desktop.create')`, a.User.ID, id); e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	write(w, 202, map[string]any{"desktop": d})
	return nil
}
func (s *Server) destroy(w http.ResponseWriter, r *http.Request, a auth, d store.Desktop) error {
	_, e := s.Store.DB.Exec(r.Context(), `UPDATE mea.desktops SET state='terminating',disconnect_at=now() WHERE id=$1 AND user_id=$2 AND state IN ('queued','provisioning','ready')`, d.ID, a.User.ID)
	if e != nil {
		return e
	}
	write(w, 202, map[string]bool{"ok": true})
	return nil
}
func (s *Server) connect(w http.ResponseWriter, r *http.Request, a auth, d store.Desktop) error {
	if d.State != "ready" || d.ConnectionID == nil || time.Now().After(d.ExpiresAt) {
		return fail(409, "Desktop is not ready")
	}
	ctx := r.Context()
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var active bool
	if _, e = tx.Exec(ctx, `SELECT id FROM mea.desktops WHERE id=$1 FOR UPDATE`, d.ID); e != nil {
		return e
	}
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mea.desktop_sessions ds JOIN mea.sessions s ON s.hash=ds.web_session_hash WHERE ds.desktop_id=$1 AND ds.closed_at IS NULL AND s.revoked_at IS NULL AND s.expires_at>now() AND (ds.connected_at IS NOT NULL OR ds.created_at>now()-interval '35 seconds'))`, d.ID).Scan(&active)
	if e != nil {
		return e
	}
	if active {
		return fail(409, "A desktop connection is already active. Disconnect it before reconnecting.")
	}
	_, e = tx.Exec(ctx, `UPDATE mea.desktop_sessions SET closed_at=now() WHERE desktop_id=$1 AND closed_at IS NULL`, d.ID)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO mea.desktop_sessions(id,desktop_id,web_session_hash) VALUES($1,$2,$3)`, security.ID(), d.ID, a.Hash)
	if e != nil {
		return e
	}
	tag, e := tx.Exec(ctx, `UPDATE mea.desktops SET last_activity=now(),last_heartbeat=now(),disconnect_at=NULL WHERE id=$1 AND user_id=$2 AND state='ready'`, d.ID, a.User.ID)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return fail(409, "Desktop is no longer ready")
	}
	token, e := security.Guacamole(s.Config.GuacKey, a.User.Username, security.Random(24), time.Now())
	if e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	write(w, 200, map[string]string{"data": token, "connectionId": strconv.Itoa(*d.ConnectionID), "dataSource": "postgresql"})
	return nil
}
func (s *Server) activity(w http.ResponseWriter, r *http.Request, a auth, d store.Desktop) error {
	var b struct{ Active bool }
	if e := decode(r, &b); e != nil {
		return e
	}
	tag, e := s.Store.DB.Exec(r.Context(), `UPDATE mea.desktops SET last_heartbeat=now(), last_activity=CASE WHEN $3 THEN now() ELSE last_activity END WHERE id=$1 AND user_id=$2 AND state='ready' AND EXISTS(SELECT 1 FROM mea.desktop_sessions WHERE desktop_id=$1 AND web_session_hash=$4 AND closed_at IS NULL)`, d.ID, a.User.ID, b.Active, a.Hash)
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 0 {
		return fail(409, "Desktop connection has ended")
	}
	write(w, 200, map[string]bool{"ok": true})
	return nil
}
func (s *Server) disconnect(w http.ResponseWriter, r *http.Request, a auth, d store.Desktop) error {
	ctx := r.Context()
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `UPDATE mea.desktop_sessions SET closed_at=now() WHERE desktop_id=$1 AND web_session_hash=$2 AND closed_at IS NULL`, d.ID, a.Hash); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `UPDATE mea.desktops SET disconnect_at=now() WHERE id=$1 AND user_id=$2 AND state='ready' AND NOT EXISTS(SELECT 1 FROM mea.desktop_sessions WHERE desktop_id=$1 AND closed_at IS NULL)`, d.ID, a.User.ID); e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	write(w, 200, map[string]bool{"ok": true})
	return nil
}

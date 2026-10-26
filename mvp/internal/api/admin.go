package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/security"
	"github.com/jackc/pgx/v5"
)

// Infra is the administrator view of hosts and containers (Type3). It is
// optional so the Newbie-only deployment keeps working without it.
type Infra interface {
	Handle(w http.ResponseWriter, r *http.Request, actor, path string) error
}

// admin is the authorization boundary for every /api/admin endpoint. Role is
// read from the database for each request; nothing from the client counts.
func (s *Server) admin(w http.ResponseWriter, r *http.Request, a auth, path string) error {
	ctx := r.Context()
	ok, e := beta.IsAdmin(ctx, s.Store.DB, a.User.ID)
	if e != nil {
		return e
	}
	if !ok {
		_ = beta.Audit(ctx, s.Store.DB, a.User.ID, "admin.denied", r.Method+" "+path, nil)
		return fail(404, "Not found")
	}
	if r.Method != "GET" {
		if e = beta.Audit(ctx, s.Store.DB, a.User.ID, "admin.request", r.Method+" "+path, nil); e != nil {
			return e
		}
	}
	p := strings.Split(strings.TrimPrefix(path, "/api/admin/"), "/")
	switch {
	case path == "/api/admin/reauth" && r.Method == "POST":
		var b struct{ Password string }
		if e = decode(r, &b); e != nil {
			return e
		}
		var hash string
		if e = s.Store.DB.QueryRow(ctx, `SELECT password_hash FROM mea.users WHERE id=$1`, a.User.ID).Scan(&hash); e != nil {
			return e
		}
		if !s.Store.Rate(ctx, "reauth:"+a.User.ID, 5, 15*time.Minute) || !security.Verify(hash, b.Password) {
			_ = beta.Audit(ctx, s.Store.DB, a.User.ID, "admin.reauth.failed", "", nil)
			return fail(403, "Password is incorrect")
		}
		_, e = s.Store.DB.Exec(ctx, `UPDATE mea.sessions SET reauth_at=now() WHERE hash=$1`, a.Hash)
		write(w, 200, map[string]bool{"ok": true})
		return e
	case path == "/api/admin/overview" && r.Method == "GET":
		var users, admins, paid, live, connected int
		e = s.Store.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM mea.users WHERE NOT disabled),
 (SELECT count(*) FROM mea.user_roles WHERE role='admin' AND revoked_at IS NULL),
 (SELECT count(DISTINCT user_id) FROM mea.entitlements WHERE status='active' AND source='stripe'),
 (SELECT count(*) FROM mea.desktops WHERE state IN ('waiting','queued','provisioning','ready','terminating')),
 (SELECT count(*) FROM mea.desktop_sessions WHERE closed_at IS NULL AND connected_at IS NOT NULL)`).Scan(&users, &admins, &paid, &live, &connected)
		if e != nil {
			return e
		}
		write(w, 200, map[string]int{"users": users, "admins": admins, "paidSubscribers": paid, "activeDesktops": live, "connectedSessions": connected})
		return nil
	case path == "/api/admin/invitations" && r.Method == "GET":
		list, e := beta.List(ctx, s.Store.DB)
		if e != nil {
			return e
		}
		write(w, 200, list)
		return nil
	case path == "/api/admin/invitations" && r.Method == "POST":
		var b struct {
			Tiers          []string
			MaxRedemptions int
			ExpiresInDays  int
			Email, Note    string
		}
		if e = decode(r, &b); e != nil {
			return e
		}
		if b.ExpiresInDays < 1 || b.ExpiresInDays > 365 {
			return fail(400, "Expiry must be between 1 and 365 days")
		}
		code, inv, e := beta.Create(ctx, s.Store.DB, a.User.ID, beta.NewInvitation{Tiers: b.Tiers, MaxRedemptions: b.MaxRedemptions, ExpiresAt: time.Now().AddDate(0, 0, b.ExpiresInDays), Email: b.Email, Note: b.Note})
		if e != nil {
			return invalid(e)
		}
		// The plaintext code is returned once and never stored.
		write(w, 201, map[string]any{"code": code, "invitation": inv})
		return nil
	case len(p) == 2 && p[0] == "invitations" && r.Method == "DELETE":
		if !uuid.MatchString(p[1]) {
			return fail(404, "Not found")
		}
		if e = beta.Revoke(ctx, s.Store.DB, a.User.ID, p[1]); errors.Is(e, pgx.ErrNoRows) {
			return fail(404, "Invitation not found or already revoked")
		}
		if e != nil {
			return e
		}
		write(w, 200, map[string]bool{"ok": true})
		return nil
	case path == "/api/admin/redemptions" && r.Method == "GET":
		id := r.URL.Query().Get("invitation")
		if id != "" && !uuid.MatchString(id) {
			return fail(400, "Invalid invitation")
		}
		list, e := beta.Redemptions(ctx, s.Store.DB, id)
		if e != nil {
			return e
		}
		write(w, 200, list)
		return nil
	case path == "/api/admin/users" && r.Method == "GET":
		return s.adminUsers(w, r)
	case len(p) == 3 && p[0] == "users" && p[2] == "admin" && r.Method == "POST":
		if !uuid.MatchString(p[1]) {
			return fail(404, "Not found")
		}
		if e = s.stepUp(r, a); e != nil {
			return e
		}
		var b struct {
			Grant  bool
			Reason string
		}
		if e = decode(r, &b); e != nil {
			return e
		}
		if b.Grant {
			_, e = beta.GrantAdmin(ctx, s.Store.DB, a.User.ID, p[1], "", b.Reason)
		} else {
			e = beta.RevokeAdmin(ctx, s.Store.DB, a.User.ID, p[1], b.Reason)
		}
		if e != nil {
			return invalid(e)
		}
		write(w, 200, map[string]bool{"ok": true})
		return nil
	case len(p) == 3 && p[0] == "users" && p[2] == "entitlements" && r.Method == "POST":
		if !uuid.MatchString(p[1]) {
			return fail(404, "Not found")
		}
		var b struct {
			Tier   string
			Grant  bool
			Reason string
		}
		if e = decode(r, &b); e != nil {
			return e
		}
		if e = beta.SetComp(ctx, s.Store.DB, a.User.ID, p[1], b.Tier, b.Grant, b.Reason); e != nil {
			return invalid(e)
		}
		write(w, 200, map[string]bool{"ok": true})
		return nil
	case path == "/api/admin/audit" && r.Method == "GET":
		rows, e := s.Store.DB.Query(ctx, `SELECT a.id,COALESCE(u.username,''),a.action,a.target,a.details,a.created_at FROM mea.admin_audit a LEFT JOIN mea.users u ON u.id=a.actor_id ORDER BY a.id DESC LIMIT 200`)
		if e != nil {
			return e
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id int64
			var actor, action, target string
			var details map[string]any
			var at time.Time
			if e = rows.Scan(&id, &actor, &action, &target, &details, &at); e != nil {
				return e
			}
			out = append(out, map[string]any{"id": id, "actor": actor, "action": action, "target": target, "details": details, "at": at})
		}
		write(w, 200, out)
		return rows.Err()
	case path == "/api/admin/sessions" && r.Method == "GET":
		rows, e := s.Store.DB.Query(ctx, `SELECT d.id::text,u.username,d.tier,d.state,d.last_activity,d.created_at,
 EXISTS(SELECT 1 FROM mea.desktop_sessions ds WHERE ds.desktop_id=d.id AND ds.closed_at IS NULL AND ds.connected_at IS NOT NULL)
 FROM mea.desktops d JOIN mea.users u ON u.id=d.user_id WHERE d.state IN ('waiting','queued','provisioning','ready','terminating') ORDER BY d.created_at`)
		if e != nil {
			return e
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id, user, tier, state string
			var activity, created time.Time
			var connected bool
			if e = rows.Scan(&id, &user, &tier, &state, &activity, &created, &connected); e != nil {
				return e
			}
			out = append(out, map[string]any{"id": id, "user": user, "tier": tier, "state": state, "lastActivity": activity, "createdAt": created, "connected": connected})
		}
		write(w, 200, out)
		return rows.Err()
	}
	if len(p) >= 1 && p[0] == "infra" && s.Infra != nil {
		if r.Method != "GET" {
			// Destructive infrastructure changes need a recent password check.
			if strings.HasSuffix(path, "/delete") || strings.HasSuffix(path, "/host/stop") || r.Method == "DELETE" {
				if e = s.stepUp(r, a); e != nil {
					return e
				}
			}
		}
		return s.Infra.Handle(w, r, a.User.ID, "/"+strings.Join(p[1:], "/"))
	}
	return fail(404, "Not found")
}

// invalid turns beta validation failures into 400s and leaves the rest as 503s.
func invalid(e error) error {
	var iv beta.Invalid
	if errors.As(e, &iv) {
		return fail(400, iv.Error())
	}
	return e
}

func (s *Server) stepUp(r *http.Request, a auth) error {
	var recent bool
	if e := s.Store.DB.QueryRow(r.Context(), `SELECT COALESCE(reauth_at>now()-interval '10 minutes',false) FROM mea.sessions WHERE hash=$1`, a.Hash).Scan(&recent); e != nil {
		return e
	}
	if !recent {
		return fail(428, "Confirm your password to continue")
	}
	return nil
}

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) error {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if len(q) > 100 {
		return fail(400, "Search is too long")
	}
	rows, e := s.Store.DB.Query(r.Context(), `SELECT u.id::text,u.username,u.email,u.name,u.verified,u.disabled,u.created_at,
 EXISTS(SELECT 1 FROM mea.user_roles ro WHERE ro.user_id=u.id AND ro.role='admin' AND ro.revoked_at IS NULL),
 COALESCE((SELECT array_agg(e.tier||':'||e.source ORDER BY e.tier) FROM mea.entitlements e WHERE e.user_id=u.id AND e.status='active'),'{}'),
 (SELECT s.status||':'||s.tier FROM mea.subscriptions s WHERE s.user_id=u.id ORDER BY s.updated_at DESC LIMIT 1)
 FROM mea.users u WHERE NOT u.disabled AND ($1='' OR u.username LIKE '%'||$1||'%' OR u.email LIKE '%'||$1||'%') ORDER BY u.created_at DESC LIMIT 100`, q)
	if e != nil {
		return e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, username, email, name string
		var verified, disabled, admin bool
		var created time.Time
		var ent []string
		var sub *string
		if e = rows.Scan(&id, &username, &email, &name, &verified, &disabled, &created, &admin, &ent, &sub); e != nil {
			return e
		}
		out = append(out, map[string]any{"id": id, "username": username, "email": email, "name": name, "verified": verified, "createdAt": created, "admin": admin, "entitlements": ent, "subscription": sub})
	}
	write(w, 200, out)
	return rows.Err()
}

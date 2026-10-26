package api

import (
	"crypto/ed25519"
	"net/http"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/security"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/streamtoken"
)

// StreamConfig enables Gaming Mode (MEA Stream). Nil disables it.
type StreamConfig struct {
	PrivateKey ed25519.PrivateKey
	// HostID and GatewayURL identify the GPU host. The milestone allows one
	// running GPU host; multi-host placement comes from Type3 later.
	HostID     string
	GatewayURL string
	STUNURLs   []string
	TURNURLs   []string
	TURNSecret string
}

// Entitlements allowed to stream from the GPU. Newbie and Power users never
// receive GPU stream tokens.
func streamEntitlement(subscription string) (string, bool) {
	switch subscription {
	case "eleet":
		return "eleet", true
	case "admin":
		return "admin", true
	}
	return "", false
}

// stream issues a short-lived, single-use token bound to this user, desktop,
// host and a fresh stream session id. It shares the desktop_sessions record
// with Guacamole so activity, idle expiry and disconnect work the same way and
// the two views of a desktop cannot be open at once.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, a auth, d store.Desktop) error {
	cfg := s.Config.Stream
	if cfg == nil {
		return fail(404, "Gaming Mode is not available")
	}
	ent, ok := streamEntitlement(a.User.Subscription)
	if !ok {
		return fail(403, "Gaming Mode requires an Eleet subscription")
	}
	var b struct {
		Profile  string `json:"profile"`
		Takeover bool   `json:"takeover"`
	}
	if r.ContentLength > 0 {
		if e := decode(r, &b); e != nil {
			return e
		}
	}
	profile := map[string]string{"economy": "economy", "balanced": "balanced", "high": "high"}[b.Profile]
	if profile == "" {
		profile = "balanced"
	}
	if d.State != "ready" || time.Now().After(d.ExpiresAt) {
		return fail(409, "Desktop is not ready")
	}
	if !s.Store.Rate(r.Context(), "stream:"+a.User.ID, 30, 10*time.Minute) {
		return fail(429, "Too many connection attempts. Try again later.")
	}
	ctx := r.Context()
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT id FROM mea.desktops WHERE id=$1 FOR UPDATE`, d.ID); e != nil {
		return e
	}
	// A gaming reconnect from the same browser session replaces its own
	// record; another open view of this desktop must be closed first unless
	// the user explicitly switches (takeover).
	var other bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mea.desktop_sessions ds JOIN mea.sessions s ON s.hash=ds.web_session_hash WHERE ds.desktop_id=$1 AND ds.closed_at IS NULL AND ds.web_session_hash<>$2 AND s.revoked_at IS NULL AND s.expires_at>now() AND (ds.connected_at IS NOT NULL OR ds.created_at>now()-interval '35 seconds'))`, d.ID, a.Hash).Scan(&other)
	if e != nil {
		return e
	}
	if other && !b.Takeover {
		return fail(409, "This desktop is open in another window. Disconnect it or switch to Gaming Mode here.")
	}
	if _, e = tx.Exec(ctx, `UPDATE mea.desktop_sessions SET closed_at=now() WHERE desktop_id=$1 AND closed_at IS NULL`, d.ID); e != nil {
		return e
	}
	sid := security.ID()
	if _, e = tx.Exec(ctx, `INSERT INTO mea.desktop_sessions(id,desktop_id,web_session_hash) VALUES($1,$2,$3)`, sid, d.ID, a.Hash); e != nil {
		return e
	}
	tag, e := tx.Exec(ctx, `UPDATE mea.desktops SET last_activity=now(),last_heartbeat=now(),disconnect_at=NULL WHERE id=$1 AND user_id=$2 AND state='ready'`, d.ID, a.User.ID)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return fail(409, "Desktop is no longer ready")
	}
	now := time.Now()
	tok, e := streamtoken.Mint(cfg.PrivateKey, streamtoken.Claims{
		User: a.User.ID, Desktop: d.ID, Session: sid, Host: cfg.HostID, Entitlement: ent, Profile: "high",
	}, now)
	if e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	ice := []streamtoken.ICEServer{}
	if len(cfg.STUNURLs) > 0 {
		ice = append(ice, streamtoken.ICEServer{URLs: cfg.STUNURLs})
	}
	if len(cfg.TURNURLs) > 0 && cfg.TURNSecret != "" {
		u, p := streamtoken.TURNCredentials(cfg.TURNSecret, a.User.ID, 10*time.Minute, now)
		ice = append(ice, streamtoken.ICEServer{URLs: cfg.TURNURLs, Username: u, Credential: p})
	}
	write(w, 200, map[string]any{
		"token": tok, "sessionId": sid, "gatewayUrl": cfg.GatewayURL, "iceServers": ice,
		"profile": profile, "expiresIn": int(streamtoken.Lifetime / time.Second),
	})
	return nil
}

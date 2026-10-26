package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
)

// TestRealPostgresStreamTokens covers Gaming Mode authorization against a real
// PostgreSQL: entitlement, ownership, the shared desktop_sessions record and
// token binding.
func TestRealPostgresStreamTokens(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for real PostgreSQL integration")
	}
	if u, err := url.Parse(dsn); err != nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "postgres") {
		t.Fatal("Destructive test fixtures are restricted to local PostgreSQL")
	}
	ctx := context.Background()
	st, e := store.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer st.DB.Close()
	schema, _ := os.ReadFile("../../schema/guacamole.sql")
	if e = st.Migrate(ctx, string(schema)); e != nil {
		t.Fatal(e)
	}
	st.DB.Exec(ctx, "TRUNCATE mea.audit,mea.outbox,mea.desktop_sessions,mea.desktops,mea.tokens,mea.sessions,mea.users,mea.rate_limits,guacamole_entity CASCADE")

	pub, priv, _ := ed25519.GenerateKey(nil)
	cfg := Config{Origin: "http://localhost:5173", GuacKey: "00000000000000000000000000000000", Cap: 4, IdleSeconds: 1200, GraceSeconds: 300, LifetimeSeconds: 14400,
		Stream: &StreamConfig{PrivateKey: priv, HostID: "gpu-1", GatewayURL: "https://stream.example.test",
			STUNURLs: []string{"stun:stream.example.test:3478"}, TURNURLs: []string{"turn:stream.example.test:3478?transport=udp"}, TURNSecret: "turn-secret"}}
	server := httptest.NewServer(New(st, cfg).Handler())
	defer server.Close()
	client := func(name, sub string) (*testClient, string) {
		jar, _ := cookiejar.New(nil)
		c := &testClient{t: t, http: &http.Client{Jar: jar}, base: server.URL}
		code, b := c.call("POST", "/auth/register", map[string]string{"Username": name, "Email": name + "@example.test", "Name": name, "Password": "Only-a-disposable-test-password"})
		if code != 201 {
			t.Fatalf("register %d %v", code, b)
		}
		c.csrf = b["csrf"].(string)
		if _, err := st.DB.Exec(ctx, "UPDATE mea.users SET subscription=$1 WHERE username=$2", sub, name); err != nil {
			t.Fatal(err)
		}
		_, d := c.call("POST", "/desktops", map[string]any{})
		id := d["desktop"].(map[string]any)["id"].(string)
		st.DB.Exec(ctx, "UPDATE mea.desktops SET state='ready' WHERE id=$1", id)
		return c, id
	}
	eleet, eleetDesk := client("erin", "eleet")
	newbie, newbieDesk := client("nora", "newbie")

	if code, b := newbie.call("POST", "/desktops/"+newbieDesk+"/stream", map[string]any{}); code != 403 {
		t.Fatalf("newbie got gaming mode: %d %v", code, b)
	}
	if code, _ := newbie.call("POST", "/desktops/"+eleetDesk+"/stream", map[string]any{}); code != 404 {
		t.Fatal("cross-user stream token")
	}
	code, b := eleet.call("POST", "/desktops/"+eleetDesk+"/stream", map[string]any{"profile": "economy"})
	if code != 200 {
		t.Fatalf("eleet stream: %d %v", code, b)
	}
	tok := b["token"].(string)
	payload, sig, _ := strings.Cut(tok, ".")
	p, _ := base64.RawURLEncoding.DecodeString(payload)
	s, _ := base64.RawURLEncoding.DecodeString(sig)
	if !ed25519.Verify(pub, p, s) {
		t.Fatal("token signature")
	}
	var claims map[string]any
	json.Unmarshal(p, &claims)
	if claims["dsk"] != eleetDesk || claims["host"] != "gpu-1" || claims["ent"] != "eleet" || claims["sid"] != b["sessionId"] {
		t.Fatalf("claims %v", claims)
	}
	if exp := int64(claims["exp"].(float64)) - time.Now().Unix(); exp > 61 || exp < 50 {
		t.Fatalf("lifetime %d", exp)
	}
	ice := b["iceServers"].([]any)
	if len(ice) != 2 || ice[1].(map[string]any)["credential"] == "" {
		t.Fatalf("ice %v", ice)
	}
	// reconnect from the same browser session replaces its own record
	if code, _ = eleet.call("POST", "/desktops/"+eleetDesk+"/stream", map[string]any{}); code != 200 {
		t.Fatal("reconnect refused", code)
	}
	var open int
	st.DB.QueryRow(ctx, "SELECT count(*) FROM mea.desktop_sessions WHERE desktop_id=$1 AND closed_at IS NULL", eleetDesk).Scan(&open)
	if open != 1 {
		t.Fatalf("%d open desktop sessions", open)
	}
	// the same desktop open in another browser session needs an explicit switch
	jar, _ := cookiejar.New(nil)
	other := &testClient{t: t, http: &http.Client{Jar: jar}, base: server.URL}
	code, lb := other.call("POST", "/auth/login", map[string]string{"Email": "erin@example.test", "Password": "Only-a-disposable-test-password"})
	if code != 200 {
		t.Fatal("login", code, lb)
	}
	other.csrf = lb["csrf"].(string)
	if code, _ = other.call("POST", "/desktops/"+eleetDesk+"/stream", map[string]any{}); code != 409 {
		t.Fatal("second window took over silently", code)
	}
	if code, _ = other.call("POST", "/desktops/"+eleetDesk+"/stream", map[string]any{"takeover": true}); code != 200 {
		t.Fatal("explicit takeover refused", code)
	}
	// Guacamole connect is refused while the gaming view is open
	if code, _ = eleet.call("POST", "/desktops/"+eleetDesk+"/connect", map[string]any{}); code != 409 {
		t.Fatal("duplicate desktop views", code)
	}
	// activity and disconnect apply to the gaming session too
	if code, _ = other.call("POST", "/desktops/"+eleetDesk+"/activity", map[string]any{"Active": true}); code != 200 {
		t.Fatal("activity", code)
	}
	if code, _ = other.call("POST", "/desktops/"+eleetDesk+"/disconnect", map[string]any{}); code != 200 {
		t.Fatal("disconnect", code)
	}
	t.Log("PASS: Eleet-only stream tokens, ownership, Ed25519 binding, TURN credentials, single desktop view, idle integration")
}

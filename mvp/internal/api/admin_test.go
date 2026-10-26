package api

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
)

func TestAdministratorBoundary(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for real PostgreSQL integration")
	}
	if u, _ := url.Parse(dsn); u == nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") {
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
	st.DB.Exec(ctx, `TRUNCATE mea.admin_audit,mea.entitlements,mea.subscriptions,mea.stripe_customers,mea.stripe_events,mea.invitation_reservations,mea.invitations,mea.user_roles,mea.audit,mea.outbox,mea.desktop_sessions,mea.desktops,mea.containers,mea.tokens,mea.sessions,mea.users,mea.rate_limits,guacamole_entity CASCADE`)
	server := httptest.NewServer(New(st, Config{Origin: "http://localhost:5173", GuacKey: "00000000000000000000000000000000", Cap: 2, IdleSeconds: 1200, GraceSeconds: 300, LifetimeSeconds: 14400}).Handler())
	defer server.Close()
	password := "Only-a-disposable-test-password"
	client := func(name string) *testClient {
		jar, _ := cookiejar.New(nil)
		c := &testClient{t: t, http: &http.Client{Jar: jar}, base: server.URL}
		code, b := c.call("POST", "/auth/register", map[string]string{"Username": name, "Email": name + "@example.test", "Name": name, "Password": password})
		if code != 201 {
			t.Fatalf("register %d %v", code, b)
		}
		c.csrf = b["csrf"].(string)
		return c
	}
	admin, user := client("tantalus"), client("alice")
	var adminID, userID string
	st.DB.QueryRow(ctx, `SELECT id::text FROM mea.users WHERE username='tantalus'`).Scan(&adminID)
	st.DB.QueryRow(ctx, `SELECT id::text FROM mea.users WHERE username='alice'`).Scan(&userID)
	for _, p := range []string{"/admin/overview", "/admin/invitations", "/admin/users", "/admin/audit", "/admin/infra/hosts"} {
		if code, _ := user.call("GET", p, nil); code != 404 {
			t.Fatalf("ordinary user reached %s: %d", p, code)
		}
	}
	// A client-supplied role or flag changes nothing.
	if code, _ := user.call("POST", "/admin/invitations", map[string]any{"Tiers": []string{"power"}, "MaxRedemptions": 1, "ExpiresInDays": 7, "Admin": true}); code != 404 {
		t.Fatal("ordinary user created an invitation")
	}
	if _, e = beta.GrantAdmin(ctx, st.DB, "", adminID, "tantalus@example.test", "test bootstrap"); e != nil {
		t.Fatal(e)
	}
	code, me := admin.call("GET", "/me", nil)
	access := me["access"].(map[string]any)
	if code != 200 || access["admin"] != true || len(access["tiers"].([]any)) != 2 {
		t.Fatalf("admin /me %v", me)
	}
	code, b := admin.call("POST", "/admin/invitations", map[string]any{"Tiers": []string{"power", "eleet"}, "MaxRedemptions": 2, "ExpiresInDays": 7, "Note": "beta"})
	if code != 201 || len(b["code"].(string)) != 27 {
		t.Fatalf("create invitation %d %v", code, b)
	}
	if code, b = admin.call("POST", "/admin/invitations", map[string]any{"Tiers": []string{"gold"}, "MaxRedemptions": 2, "ExpiresInDays": 7}); code != 400 {
		t.Fatalf("invalid tier %d %v", code, b)
	}
	inv := b
	_ = inv
	if code, _ = admin.call("POST", "/admin/users/"+userID+"/admin", map[string]any{"Grant": true, "Reason": "test"}); code != 428 {
		t.Fatalf("role change without step-up: %d", code)
	}
	if code, _ = admin.call("POST", "/admin/reauth", map[string]string{"Password": "wrong-password-value"}); code != 403 {
		t.Fatal("wrong password accepted for step-up")
	}
	if code, _ = admin.call("POST", "/admin/reauth", map[string]string{"Password": password}); code != 200 {
		t.Fatal("step-up failed")
	}
	if code, _ = admin.call("POST", "/admin/users/"+userID+"/entitlements", map[string]any{"Tier": "power", "Grant": true, "Reason": "comp"}); code != 200 {
		t.Fatal("comp grant failed")
	}
	if a, _ := beta.AccessFor(ctx, st.DB, userID); !a.Has("power") || a.Admin {
		t.Fatalf("comp access %+v", a)
	}
	if code, _ = admin.call("POST", "/admin/users/"+adminID+"/admin", map[string]any{"Grant": false, "Reason": "self"}); code != 400 {
		t.Fatal("self revocation allowed")
	}
	req, _ := http.NewRequest("GET", server.URL+"/api/admin/audit", nil)
	if resp, e := admin.http.Do(req); e != nil || resp.StatusCode != 200 {
		t.Fatal("audit unavailable")
	}
	var denied int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM mea.admin_audit WHERE action='admin.denied'`).Scan(&denied)
	if denied < 6 {
		t.Fatalf("denied administrator attempts were not audited: %d", denied)
	}
}

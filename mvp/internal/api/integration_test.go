package api

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/control"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
)

type testClient struct {
	t          *testing.T
	http       *http.Client
	base, csrf string
}

func (c *testClient) call(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, c.base+"/api"+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("X-CSRF-Token", c.csrf)
	resp, e := c.http.Do(req)
	if e != nil {
		c.t.Fatal(e)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if e = json.Unmarshal(raw, &result); e != nil {
		c.t.Fatalf("response %d: %s", resp.StatusCode, raw)
	}
	return resp.StatusCode, result
}
func TestRealPostgresAuthorizationAndLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for real PostgreSQL integration")
	}
	u,parseErr:=url.Parse(dsn);if parseErr!=nil || (u.Hostname()!="localhost"&&u.Hostname()!="127.0.0.1"&&u.Hostname()!="host.docker.internal"&&u.Hostname()!="postgres"){t.Fatal("Destructive test fixtures are restricted to local PostgreSQL")}
	ctx := context.Background()
	st, e := store.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer st.DB.Close()
	schema, e := os.ReadFile("../../schema/guacamole.sql")
	if e != nil {
		t.Fatal(e)
	}
	if e = st.Migrate(ctx, string(schema)); e != nil {
		t.Fatal(e)
	}
	if _, e = st.DB.Exec(ctx, "TRUNCATE mea.audit,mea.outbox,mea.desktop_sessions,mea.desktops,mea.tokens,mea.sessions,mea.users,mea.rate_limits,guacamole_entity CASCADE"); e != nil {
		t.Fatal(e)
	}
	config := Config{Origin: "http://localhost:5173", GuacKey: "00000000000000000000000000000000", Cap: 2, IdleSeconds: 1200, GraceSeconds: 300, LifetimeSeconds: 14400}
	server := httptest.NewServer(New(st, config).Handler())
	defer server.Close()
	newClient := func(name string) *testClient {
		jar, _ := cookiejar.New(nil)
		c := &testClient{t: t, http: &http.Client{Jar: jar}, base: server.URL}
		code, b := c.call("POST", "/auth/register", map[string]string{"Username": name, "Email": name + "@example.test", "Name": name, "Password": "Only-a-disposable-test-password"})
		if code != 201 {
			t.Fatalf("registration %d: %v", code, b)
		}
		c.csrf = b["csrf"].(string)
		return c
	}
	a, b := newClient("alice"), newClient("bob")
	var profiles, identities int
	st.DB.QueryRow(ctx, "SELECT count(*) FROM mea.users").Scan(&profiles)
	st.DB.QueryRow(ctx, "SELECT count(*) FROM guacamole_user").Scan(&identities)
	if profiles != 2 || identities != 2 {
		t.Fatalf("identity mapping %d/%d", profiles, identities)
	}
	var id string
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, r := a.call("POST", "/desktops", map[string]any{})
			if code != 202 && code != 200 {
				t.Errorf("duplicate create %d %v", code, r)
				return
			}
			got := r["desktop"].(map[string]any)["id"].(string)
			mu.Lock()
			defer mu.Unlock()
			if id != "" && id != got {
				t.Error("duplicate desktops")
			}
			id = got
		}()
	}
	wg.Wait()
	for _, path := range []string{"/connect", "/activity", "/disconnect", ""} {
		method := "POST"
		if path == "" {
			method = "DELETE"
		}
		code, _ := b.call(method, "/desktops/"+id+path, map[string]any{"Active": true})
		if code != 404 {
			t.Errorf("cross-user %s access returned %d", path, code)
		}
	}
	old := a.csrf
	a.csrf = "forged"
	code, _ := a.call("DELETE", "/desktops/"+id, nil)
	if code != 403 {
		t.Fatal("CSRF bypass")
	}
	a.csrf = old
	c := newClient("charlie")
	if code, _ = b.call("POST", "/desktops", map[string]any{}); code != 202 {
		t.Fatal("second desktop rejected")
	}
	if code, _ = c.call("POST", "/desktops", map[string]any{}); code != 409 {
		t.Fatal("global quota bypass")
	}
	_, e = st.DB.Exec(ctx, "UPDATE mea.desktops SET state='ready',last_activity=now()-interval '21 minutes' WHERE id=$1", id)
	if e != nil {
		t.Fatal(e)
	}
	ctl := control.Service{Store: st, IdleSeconds: 1200, GraceSeconds: 300}
	plan, e := ctl.Handle(ctx, control.Request{Action: "reconcile"})
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, j := range plan.Jobs {
		if j.ID == id && j.State == "terminating" {
			found = true
		}
	}
	if !found {
		t.Fatal("idle expiration did not schedule termination")
	}
	if _, e = ctl.Handle(ctx, control.Request{Action: "terminated", ID: id}); e != nil {
		t.Fatal(e)
	}
	if code, _ = a.call("POST", "/desktops", map[string]any{}); code != 202 {
		t.Fatal("could not provision fresh desktop")
	}
	_, _ = st.DB.Exec(ctx, "UPDATE mea.sessions SET expires_at=now()-interval '1 second' WHERE user_id=(SELECT id FROM mea.users WHERE username='alice')")
	if code, _ = a.call("GET", "/me", nil); code != 401 {
		t.Fatal("expired session accepted")
	}
	if code, _ = b.call("POST", "/logout", map[string]any{}); code != 200 {
		t.Fatal("logout failed")
	}
	if code, _ = b.call("GET", "/me", nil); code != 401 {
		t.Fatal("revoked session accepted")
	}
	t.Log("PASS: transactional identity mapping, concurrent idempotency, two-user isolation, CSRF, cap, idle termination, fresh lease, expiry, revocation")
}

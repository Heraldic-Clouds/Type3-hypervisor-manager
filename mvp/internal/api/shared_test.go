package api

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/control"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
)

func TestRealPostgresSharedDesktop(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for real PostgreSQL integration")
	}
	u, parseErr := url.Parse(dsn)
	if parseErr != nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "host.docker.internal" && u.Hostname() != "postgres") {
		t.Fatal("Destructive test fixtures are restricted to local PostgreSQL")
	}
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
	if e = st.Migrate(ctx, string(schema)); e != nil {
		t.Fatal("migrations are not idempotent: ", e)
	}
	if _, e = st.DB.Exec(ctx, "TRUNCATE mea.audit,mea.outbox,mea.desktop_sessions,mea.desktops,mea.shared_desktops,mea.tokens,mea.sessions,mea.users,mea.rate_limits,guacamole_entity,guacamole_connection CASCADE"); e != nil {
		t.Fatal(e)
	}
	config := Config{Origin: "http://localhost:5173", GuacKey: "00000000000000000000000000000000", Cap: 1, IdleSeconds: 1200, GraceSeconds: 300, LifetimeSeconds: 14400}
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
	ctl := control.Service{Store: st, IdleSeconds: 1200, GraceSeconds: 300}
	reconcile := func() control.Result {
		t.Helper()
		plan, e := ctl.Handle(ctx, control.Request{Action: "reconcile"})
		if e != nil {
			t.Fatal(e)
		}
		return plan
	}
	vnc := map[string]string{"hostname": "10.0.0.5", "port": "5900", "password": "test-vnc"}
	for _, bad := range []control.Request{
		{Action: "shared-desktop", Kind: "ubuntu", Name: "X", Protocol: "vnc", Parameters: vnc},
		{Action: "shared-desktop", Kind: "xp25-ai", Name: "MEA hijack", Protocol: "vnc", Parameters: vnc},
		{Action: "shared-desktop", Kind: "xp25-ai", Name: "XP25-AI", Protocol: "ssh", Parameters: vnc},
		{Action: "shared-desktop", Kind: "xp25-ai", Name: "XP25-AI", Protocol: "vnc", Parameters: map[string]string{"hostname": "h", "port": "1", "enable-sftp": "true"}},
		{Action: "shared-desktop", Kind: "xp25-ai", Name: "XP25-AI", Protocol: "vnc", Parameters: map[string]string{"hostname": "h"}},
	} {
		if _, e = ctl.Handle(ctx, bad); e == nil {
			t.Fatalf("invalid registration accepted: %+v", bad)
		}
	}
	registered, e := ctl.Handle(ctx, control.Request{Action: "shared-desktop", Kind: "xp25-ai", Name: "XP25-AI", Description: "Shared test machine", Protocol: "vnc", Parameters: vnc})
	if e != nil || registered.ConnectionID == 0 {
		t.Fatal("registration failed: ", e)
	}
	connection := registered.ConnectionID
	again, e := ctl.Handle(ctx, control.Request{Action: "shared-desktop", Kind: "xp25-ai", Name: "XP25-AI", Description: "Shared test machine", Protocol: "vnc", Parameters: vnc})
	if e != nil || again.ConnectionID != connection {
		t.Fatal("re-registration did not update in place: ", e)
	}
	var parameters int
	st.DB.QueryRow(ctx, "SELECT count(*) FROM guacamole_connection_parameter WHERE connection_id=$1", connection).Scan(&parameters)
	if parameters != len(vnc) {
		t.Fatalf("connection has %d parameters", parameters)
	}
	permitted := func(user string) bool {
		var ok bool
		st.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM guacamole_connection_permission p JOIN mea.users u ON u.entity_id=p.entity_id WHERE u.username=$1 AND p.connection_id=$2)`, user, connection).Scan(&ok)
		return ok
	}
	shared := func(c *testClient) map[string]any {
		code, r := c.call("GET", "/desktops", nil)
		if code != 200 {
			t.Fatalf("list %d", code)
		}
		list := r["shared"].([]any)
		if len(list) != 1 {
			t.Fatalf("shared list %v", list)
		}
		return list[0].(map[string]any)
	}
	a, b, c := newClient("alice"), newClient("bob"), newClient("charlie")
	if s := shared(a); s["kind"] != "xp25-ai" || s["available"] != true {
		t.Fatalf("shared desktop not offered: %v", s)
	}
	if code, _ := a.call("POST", "/desktops", map[string]string{"Kind": "missing"}); code != 404 {
		t.Fatal("unknown shared kind accepted")
	}
	code, r := a.call("POST", "/desktops", map[string]string{"Kind": "xp25-ai"})
	if code != 201 {
		t.Fatalf("borrow %d %v", code, r)
	}
	lease := r["desktop"].(map[string]any)
	id := lease["id"].(string)
	if lease["state"] != "ready" || lease["kind"] != "xp25-ai" || !permitted("alice") {
		t.Fatalf("lease not ready and permitted: %v", lease)
	}
	if code, r = a.call("POST", "/desktops", map[string]string{"Kind": "xp25-ai"}); code != 200 || r["desktop"].(map[string]any)["id"] != id {
		t.Fatal("repeated borrow was not idempotent")
	}
	if shared(b)["available"] != false {
		t.Fatal("occupied seat shown as available")
	}
	if code, _ = b.call("POST", "/desktops", map[string]string{"Kind": "xp25-ai"}); code != 409 || permitted("bob") {
		t.Fatal("second user took an occupied seat")
	}
	// Shared seats do not count toward the EC2 cap (1 in this test).
	if code, _ = b.call("POST", "/desktops", map[string]any{}); code != 202 {
		t.Fatal("shared lease consumed EC2 capacity")
	}
	if code, _ = b.call("POST", "/desktops/"+id+"/connect", map[string]any{}); code != 404 {
		t.Fatal("cross-user connect to a shared lease")
	}
	if code, _ = c.call("POST", "/desktops", map[string]any{}); code != 409 {
		t.Fatal("EC2 cap bypass")
	}
	code, r = a.call("POST", "/desktops/"+id+"/connect", map[string]any{})
	if code != 200 || r["connectionId"] != control.ConnectionID(connection) {
		t.Fatalf("connect %d %v", code, r)
	}
	// The unchanged Guacamole lifecycle listener (guacamole/src/.../Lifecycle.java)
	// must admit and keep the tunnel. These are its two statements verbatim.
	tag, e := st.DB.Exec(ctx, "UPDATE mea.desktop_sessions SET tunnel_id=$1,connected_at=now() WHERE id=("+
		"SELECT ds.id FROM mea.desktop_sessions ds JOIN mea.desktops d ON d.id=ds.desktop_id JOIN mea.users u ON u.id=d.user_id JOIN mea.sessions s ON s.hash=ds.web_session_hash "+
		"WHERE u.username=$2 AND NOT u.disabled AND d.state='ready' AND d.expires_at>now() AND ds.closed_at IS NULL AND ds.tunnel_id IS NULL AND ds.created_at>now()-interval '35 seconds' "+
		"AND s.revoked_at IS NULL AND s.expires_at>now() ORDER BY ds.created_at DESC LIMIT 1 FOR UPDATE OF ds SKIP LOCKED)", "tunnel-test", "alice")
	if e != nil || tag.RowsAffected() != 1 {
		t.Fatal("lifecycle listener would refuse the shared tunnel: ", e)
	}
	if code, _ = a.call("POST", "/desktops/"+id+"/activity", map[string]any{"Active": true}); code != 200 {
		t.Fatal("activity rejected")
	}
	var kept bool
	st.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM mea.desktop_sessions ds JOIN mea.desktops d ON d.id=ds.desktop_id JOIN mea.sessions s ON s.hash=ds.web_session_hash JOIN mea.users u ON u.id=d.user_id "+
		"WHERE ds.tunnel_id=$1 AND ds.closed_at IS NULL AND d.state='ready' AND d.expires_at>now() AND d.last_activity>now()-($2 * interval '1 second') "+
		"AND (d.last_heartbeat IS NULL OR d.last_heartbeat>now()-interval '90 seconds') AND NOT u.disabled AND s.revoked_at IS NULL AND s.expires_at>now())", "tunnel-test", 1200).Scan(&kept)
	if !kept {
		t.Fatal("lifecycle listener would close the shared tunnel")
	}
	if code, _ = a.call("POST", "/desktops/"+id+"/disconnect", map[string]any{}); code != 200 {
		t.Fatal("disconnect failed")
	}
	// Ending the lease releases the seat and permission but keeps the machine's connection.
	if code, _ = a.call("DELETE", "/desktops/"+id, nil); code != 202 {
		t.Fatal("end lease failed")
	}
	for _, j := range reconcile().Jobs {
		if j.ID == id {
			t.Fatal("shared lease was handed to the EC2 worker")
		}
	}
	var state string
	var leaseConnection *int
	st.DB.QueryRow(ctx, "SELECT state,connection_id FROM mea.desktops WHERE id=$1", id).Scan(&state, &leaseConnection)
	if state != "terminated" || leaseConnection != nil || permitted("alice") {
		t.Fatalf("lease not finished: %s %v", state, leaseConnection)
	}
	connectionExists := func() bool {
		var ok bool
		st.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM guacamole_connection WHERE connection_id=$1)", connection).Scan(&ok)
		return ok
	}
	if !connectionExists() || shared(c)["available"] != true {
		t.Fatal("shared connection lost or seat not released")
	}
	if _, e = ctl.Handle(ctx, control.Request{Action: "terminated", ID: id}); e != nil || !connectionExists() {
		t.Fatal("a late worker termination deleted the shared connection")
	}
	if _, e = ctl.Handle(ctx, control.Request{Action: "orphans"}); e != nil || !connectionExists() {
		t.Fatal("orphan cleanup deleted the shared connection")
	}
	// A user with a running EC2 desktop must end it first.
	if code, _ = b.call("POST", "/desktops", map[string]string{"Kind": "xp25-ai"}); code != 409 {
		t.Fatal("second desktop for one user")
	}
	// Idle expiry of a shared lease, then a fresh lease by another user.
	code, r = c.call("POST", "/desktops", map[string]string{"Kind": "xp25-ai"})
	if code != 201 {
		t.Fatalf("seat not reusable: %d %v", code, r)
	}
	idC := r["desktop"].(map[string]any)["id"].(string)
	if _, e = st.DB.Exec(ctx, "UPDATE mea.desktops SET last_activity=now()-interval '21 minutes' WHERE id=$1", idC); e != nil {
		t.Fatal(e)
	}
	reconcile()
	st.DB.QueryRow(ctx, "SELECT state FROM mea.desktops WHERE id=$1", idC).Scan(&state)
	if state != "terminated" || permitted("charlie") || !connectionExists() {
		t.Fatal("idle shared lease not ended cleanly")
	}
	// Disabling the shared desktop ends the current lease and hides it.
	if code, _ = a.call("POST", "/desktops", map[string]string{"Kind": "xp25-ai"}); code != 201 {
		t.Fatal("borrow before disable failed")
	}
	off := false
	if _, e = ctl.Handle(ctx, control.Request{Action: "shared-desktop", Kind: "xp25-ai", Name: "XP25-AI", Protocol: "vnc", Parameters: vnc, Enabled: &off}); e != nil {
		t.Fatal(e)
	}
	reconcile()
	if permitted("alice") {
		t.Fatal("disable left a permission")
	}
	if code, r = a.call("GET", "/desktops", nil); code != 200 || len(r["shared"].([]any)) != 0 {
		t.Fatal("disabled shared desktop still listed")
	}
	if code, _ = a.call("POST", "/desktops", map[string]string{"Kind": "xp25-ai"}); code != 404 {
		t.Fatal("disabled shared desktop borrowed")
	}
	t.Log("PASS: registration validation and idempotency, single seat, EC2 cap independence, isolation, end/idle/disable release, connection preserved")
}

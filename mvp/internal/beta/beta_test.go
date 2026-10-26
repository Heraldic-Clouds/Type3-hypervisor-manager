package beta

import (
	"context"
	"errors"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/security"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
)

func TestCodeFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		c := NewCode()
		if len(c) != 27 || c[:4] != "MEA-" {
			t.Fatalf("unexpected code %q", c)
		}
		if seen[c] {
			t.Fatal("duplicate code")
		}
		seen[c] = true
		if CodeHash(c) != CodeHash(" "+string([]byte(c))+" ") || Normalize(c) != Normalize(stringsLower(c)) {
			t.Fatal("normalization is not stable")
		}
	}
}
func stringsLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

func local(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for real PostgreSQL integration")
	}
	u, e := url.Parse(dsn)
	if e != nil || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "postgres") {
		t.Fatal("Destructive test fixtures are restricted to local PostgreSQL")
	}
	ctx := context.Background()
	st, e := store.Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema, e := os.ReadFile("../../schema/guacamole.sql")
	if e != nil {
		t.Fatal(e)
	}
	if e = st.Migrate(ctx, string(schema)); e != nil {
		t.Fatal(e)
	}
	if _, e = st.DB.Exec(ctx, `TRUNCATE mea.admin_audit,mea.entitlements,mea.subscriptions,mea.stripe_customers,mea.stripe_events,mea.invitation_reservations,mea.invitations,mea.user_roles,
 mea.audit,mea.outbox,mea.desktop_sessions,mea.desktops,mea.containers,mea.tokens,mea.sessions,mea.users,mea.rate_limits,guacamole_entity CASCADE`); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(st.DB.Close)
	return st
}
func user(t *testing.T, st *store.Store, name string, verified bool) string {
	t.Helper()
	ctx := context.Background()
	var entity, guac int
	if e := st.DB.QueryRow(ctx, `INSERT INTO guacamole_entity(name,type) VALUES($1,'USER') RETURNING entity_id`, name).Scan(&entity); e != nil {
		t.Fatal(e)
	}
	if e := st.DB.QueryRow(ctx, `INSERT INTO guacamole_user(entity_id,password_hash,password_salt,password_date) VALUES($1,'\x00','\x00',now()) RETURNING user_id`, entity).Scan(&guac); e != nil {
		t.Fatal(e)
	}
	id := security.ID()
	if _, e := st.DB.Exec(ctx, `INSERT INTO mea.users(id,guac_user_id,entity_id,username,email,name,password_hash,verified) VALUES($1,$2,$3,$4,$5,$4,'x',$6)`, id, guac, entity, name, name+"@example.test", verified); e != nil {
		t.Fatal(e)
	}
	return id
}

func TestInvitationsAndEntitlements(t *testing.T) {
	st := local(t)
	ctx := context.Background()
	admin := user(t, st, "tantalus", true)
	alice, bob := user(t, st, "alice", true), user(t, st, "bob", true)
	if _, e := GrantAdmin(ctx, st.DB, "", admin, "tantalus@example.test", "bootstrap"); e != nil {
		t.Fatal(e)
	}
	if changed, e := GrantAdmin(ctx, st.DB, "", admin, "tantalus@example.test", "bootstrap"); e != nil || changed {
		t.Fatalf("grant is not idempotent: %v %v", changed, e)
	}
	unverified := user(t, st, "mallory", false)
	if _, e := GrantAdmin(ctx, st.DB, admin, unverified, "", "test"); e == nil {
		t.Fatal("unverified account promoted")
	}
	if _, e := GrantAdmin(ctx, st.DB, admin, alice, "someone-else@example.test", "test"); e == nil {
		t.Fatal("email mismatch promoted")
	}
	var guacAdmin int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM guacamole_system_permission p JOIN mea.users u ON u.entity_id=p.entity_id WHERE u.id=$1 AND p.permission='ADMINISTER'`, admin).Scan(&guacAdmin)
	if guacAdmin != 1 {
		t.Fatal("Guacamole ADMINISTER not granted")
	}
	a, _ := AccessFor(ctx, st.DB, admin)
	if !a.Admin || !a.Has("power") || !a.Has("eleet") || a.Source != "admin_grant" {
		t.Fatalf("admin access %+v", a)
	}
	if a, _ = AccessFor(ctx, st.DB, alice); a.Admin || len(a.Tiers) != 0 {
		t.Fatalf("ordinary user has access %+v", a)
	}

	power, _, e := Create(ctx, st.DB, admin, NewInvitation{Tiers: []string{"power"}, MaxRedemptions: 1, ExpiresAt: time.Now().Add(time.Hour)})
	if e != nil {
		t.Fatal(e)
	}
	eleet, _, _ := Create(ctx, st.DB, admin, NewInvitation{Tiers: []string{"eleet"}, MaxRedemptions: 3, ExpiresAt: time.Now().Add(time.Hour)})
	either, _, _ := Create(ctx, st.DB, admin, NewInvitation{Tiers: []string{"power", "eleet"}, MaxRedemptions: 1, ExpiresAt: time.Now().Add(time.Hour), Email: "Bob@Example.test"})
	if _, e = Reserve(ctx, st.DB, alice, "alice@example.test", "MEA-00000-00000-00000-00000", "power", time.Minute); !errors.Is(e, ErrInvalidCode) {
		t.Fatal("unknown code accepted")
	}
	if _, e = Reserve(ctx, st.DB, alice, "alice@example.test", power, "eleet", time.Minute); !errors.Is(e, ErrInvalidCode) {
		t.Fatal("Power code unlocked Eleet")
	}
	if _, e = Reserve(ctx, st.DB, alice, "alice@example.test", either, "power", time.Minute); !errors.Is(e, ErrInvalidCode) {
		t.Fatal("email-restricted code accepted for another account")
	}

	// Concurrent checkouts for a single-use code: exactly one reservation.
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	users := []string{alice}
	for i := 0; i < 7; i++ {
		users = append(users, user(t, st, "racer"+string(rune('a'+i)), true))
	}
	for _, u := range users {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			if _, e := Reserve(ctx, st.DB, u, "", stringsLower(power), "power", time.Minute); e == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}(u)
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d concurrent reservations for a single-use code", won)
	}

	// Abandoned checkout: the slot returns once the hold expires.
	res, e := Reserve(ctx, st.DB, bob, "bob@example.test", either, "eleet", time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	if e = AttachCheckout(ctx, st.DB, res, "cs_test_abandoned"); e != nil {
		t.Fatal(e)
	}
	st.DB.Exec(ctx, `UPDATE mea.invitation_reservations SET expires_at=now()-interval '1 second' WHERE id=$1`, res)
	res, e = Reserve(ctx, st.DB, bob, "bob@example.test", either, "eleet", time.Minute)
	if e != nil {
		t.Fatalf("abandoned checkout consumed the invitation: %v", e)
	}
	if e = AttachCheckout(ctx, st.DB, res, "cs_test_paid"); e != nil {
		t.Fatal(e)
	}

	// Payment confirmed: redeem twice (webhook replay) and apply the subscription.
	for i := 0; i < 2; i++ {
		tx, _ := st.DB.Begin(ctx)
		u, tier, e := Redeem(ctx, tx, "cs_test_paid")
		if e != nil || u != bob || tier != "eleet" {
			t.Fatalf("redeem %v %s %s", e, u, tier)
		}
		if e = ApplySubscription(ctx, tx, Subscription{ID: "sub_1", Customer: "cus_1", Price: "price_eleet", Status: "active", UserID: bob, Tier: "eleet", Updated: 10}); e != nil {
			t.Fatal(e)
		}
		tx.Commit(ctx)
	}
	inv, _ := List(ctx, st.DB)
	for _, i := range inv {
		if i.Tiers[0] == "power" && len(i.Tiers) == 2 && (i.Redemptions != 1 || i.Status != "exhausted") {
			t.Fatalf("redemption count %+v", i)
		}
	}
	if _, e = Reserve(ctx, st.DB, bob, "bob@example.test", either, "power", time.Minute); !errors.Is(e, ErrInvalidCode) {
		t.Fatal("exhausted code accepted")
	}
	if a, _ = AccessFor(ctx, st.DB, bob); !a.Has("eleet") || a.Has("power") {
		t.Fatalf("paid access %+v", a)
	}

	apply := func(s Subscription) {
		tx, _ := st.DB.Begin(ctx)
		if e := ApplySubscription(ctx, tx, s); e != nil {
			t.Fatal(e)
		}
		tx.Commit(ctx)
	}
	// Out-of-order stale event must not reactivate or downgrade.
	apply(Subscription{ID: "sub_1", Customer: "cus_1", Price: "price_eleet", Status: "past_due", UserID: bob, Tier: "eleet", Updated: 20})
	if a, _ = AccessFor(ctx, st.DB, bob); a.Has("eleet") {
		t.Fatal("failed payment kept access")
	}
	apply(Subscription{ID: "sub_1", Customer: "cus_1", Price: "price_eleet", Status: "active", UserID: bob, Tier: "eleet", Updated: 15})
	if a, _ = AccessFor(ctx, st.DB, bob); a.Has("eleet") {
		t.Fatal("stale event overwrote newer state")
	}
	apply(Subscription{ID: "sub_1", Customer: "cus_1", Price: "price_power", Status: "active", UserID: bob, Tier: "power", Updated: 30})
	if a, _ = AccessFor(ctx, st.DB, bob); !a.Has("power") || a.Has("eleet") {
		t.Fatalf("plan change %+v", a)
	}
	apply(Subscription{ID: "sub_1", Customer: "cus_1", Price: "price_power", Status: "canceled", UserID: bob, Tier: "power", Updated: 40})
	if a, _ = AccessFor(ctx, st.DB, bob); len(a.Tiers) != 0 {
		t.Fatal("cancellation kept access")
	}

	// Billing sync for an administrator's own (hypothetical) subscription never touches admin_grant.
	apply(Subscription{ID: "sub_admin", Customer: "cus_2", Price: "price_power", Status: "canceled", UserID: admin, Tier: "power", Updated: 50})
	if a, _ = AccessFor(ctx, st.DB, admin); !a.Has("power") || !a.Has("eleet") {
		t.Fatal("billing sync removed administrator access")
	}

	// Expired and revoked codes.
	expired, ei, _ := Create(ctx, st.DB, admin, NewInvitation{Tiers: []string{"eleet"}, MaxRedemptions: 5, ExpiresAt: time.Now().Add(2 * time.Minute)})
	st.DB.Exec(ctx, `UPDATE mea.invitations SET expires_at=now()-interval '1 second' WHERE id=$1`, ei.ID)
	if _, e = Reserve(ctx, st.DB, alice, "", expired, "eleet", time.Minute); !errors.Is(e, ErrInvalidCode) {
		t.Fatal("expired code accepted")
	}
	_, ri, _ := Create(ctx, st.DB, admin, NewInvitation{Tiers: []string{"eleet"}, MaxRedemptions: 5, ExpiresAt: time.Now().Add(time.Hour)})
	if e = Revoke(ctx, st.DB, admin, ri.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = Reserve(ctx, st.DB, alice, "", eleet, "eleet", time.Minute); e != nil {
		t.Fatalf("multi-use code rejected: %v", e)
	}

	if e = RevokeAdmin(ctx, st.DB, admin, admin, "self"); e == nil {
		t.Fatal("self revocation allowed")
	}
	var audits int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM mea.admin_audit`).Scan(&audits)
	if audits < 6 {
		t.Fatalf("expected audited administrative changes, got %d", audits)
	}
}

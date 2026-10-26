package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/security"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
)

func TestSignature(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"invoice.paid","created":1}`)
	now := time.Now()
	h := Sign(body, "whsec_test", now)
	if _, e := Verify(body, h, "whsec_test", now); e != nil {
		t.Fatal(e)
	}
	if _, e := Verify(body, h, "whsec_other", now); e == nil {
		t.Fatal("wrong secret accepted")
	}
	if _, e := Verify(append(body, ' '), h, "whsec_test", now); e == nil {
		t.Fatal("tampered body accepted")
	}
	if _, e := Verify(body, h, "whsec_test", now.Add(6*time.Minute)); e == nil {
		t.Fatal("replayed old signature accepted")
	}
}

func TestClientRefusesLiveKeys(t *testing.T) {
	c := &Client{Key: "sk_live_x"}
	if _, e := c.CreateCustomer(context.Background(), "u", "e", "n"); e == nil || !strings.Contains(e.Error(), "live mode is disabled") {
		t.Fatalf("live key used: %v", e)
	}
}

func TestClientRequests(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Idempotency-Key")+" "+string(b))
		switch {
		case r.URL.Path == "/v1/checkout/sessions":
			fmt.Fprint(w, `{"id":"cs_test_1","url":"https://checkout.stripe.com/c/pay/cs_test_1"}`)
		case strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
			fmt.Fprint(w, `{"id":"sub_1","customer":"cus_1","status":"active","metadata":{"mea_user":"u1"},"latest_invoice":{"status":"paid"},"items":{"data":[{"id":"si_1","current_period_end":1900000000,"price":{"id":"price_eleet"}}]}}`)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"type":"invalid_request_error"}}`)
		}
	}))
	defer srv.Close()
	c := &Client{Key: "sk_test_x", Base: srv.URL, Prices: Prices{"power": "price_power", "eleet": "price_eleet"}}
	id, u, e := c.CreateCheckout(context.Background(), CheckoutParams{Customer: "cus_1", Price: "price_eleet", User: "u1", Tier: "eleet", Reservation: "r1", Success: "https://x/s", Cancel: "https://x/c", ExpiresAt: time.Unix(1800000000, 0)})
	if e != nil || id != "cs_test_1" || !strings.HasPrefix(u, "https://checkout.stripe.com/") {
		t.Fatal(id, u, e)
	}
	q, _ := url.ParseQuery(strings.SplitN(got[0], " ", 4)[3])
	if q.Get("mode") != "subscription" || q.Get("line_items[0][price]") != "price_eleet" || q.Get("allow_promotion_codes") != "false" || !strings.Contains(got[0], "mea-checkout-r1") {
		t.Fatalf("checkout request %s", got[0])
	}
	s, e := c.Subscription(context.Background(), "sub_1")
	if e != nil || s.Tier != "eleet" || s.CurrentPeriodEnd != 1900000000 || s.InvoiceStatus != "paid" || s.UserID != "u1" {
		t.Fatalf("subscription %+v %v", s, e)
	}
}

// fakeStripe keeps subscriptions in memory, standing in for Stripe test mode.
type fakeStripe struct {
	mu        sync.Mutex
	subs      map[string]beta.Subscription
	sessions  map[string]CheckoutParams
	customers int
}

func (f *fakeStripe) CreateCustomer(ctx context.Context, user, email, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.customers++
	return "cus_" + user[:8], nil
}
func (f *fakeStripe) CreateCheckout(ctx context.Context, p CheckoutParams) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "cs_test_" + p.Reservation[:8]
	f.sessions[id] = p
	return id, "https://checkout.stripe.test/" + id, nil
}
func (f *fakeStripe) Portal(ctx context.Context, customer, returnURL string) (string, error) {
	return "https://billing.stripe.test/" + customer, nil
}
func (f *fakeStripe) Subscription(ctx context.Context, id string) (beta.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subs[id]
	if !ok {
		return s, fmt.Errorf("no such subscription")
	}
	s.Updated = time.Now().UnixNano()
	return s, nil
}
func (f *fakeStripe) ChangePrice(ctx context.Context, sub, price, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.subs[sub]
	s.Price = price
	s.Tier = Prices{"power": "price_power", "eleet": "price_eleet"}.Tier(price)
	f.subs[sub] = s
	return nil
}

func TestBillingLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for real PostgreSQL integration")
	}
	if u, _ := url.Parse(dsn); u == nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatal("local PostgreSQL only")
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
	mk := func(name string) (id, cookie, csrf string) {
		var entity, guac int
		st.DB.QueryRow(ctx, `INSERT INTO guacamole_entity(name,type) VALUES($1,'USER') RETURNING entity_id`, name).Scan(&entity)
		st.DB.QueryRow(ctx, `INSERT INTO guacamole_user(entity_id,password_hash,password_salt,password_date) VALUES($1,'\x00','\x00',now()) RETURNING user_id`, entity).Scan(&guac)
		id = security.ID()
		st.DB.Exec(ctx, `INSERT INTO mea.users(id,guac_user_id,entity_id,username,email,name,password_hash,verified) VALUES($1,$2,$3,$4,$5,$4,'x',true)`, id, guac, entity, name, name+"@example.test")
		cookie, csrf = security.Random(32), security.Random(16)
		st.DB.Exec(ctx, `INSERT INTO mea.sessions(hash,user_id,csrf,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, security.Hash(cookie), id, csrf)
		return
	}
	admin, adminCookie, adminCSRF := mk("tantalus")
	if _, e = beta.GrantAdmin(ctx, st.DB, "", admin, "", "test bootstrap"); e != nil {
		t.Fatal(e)
	}
	alice, aliceCookie, aliceCSRF := mk("alice")
	fs := &fakeStripe{subs: map[string]beta.Subscription{}, sessions: map[string]CheckoutParams{}}
	secret := "whsec_local_test"
	// Exercise the same wire path the Lambda uses: Remote -> Dispatch -> DB.
	backend := Remote{Invoke: func(ctx context.Context, c Call) (Reply, error) {
		b, _ := json.Marshal(c)
		var c2 Call
		json.Unmarshal(b, &c2)
		r, e := Dispatch(ctx, DB{st}, c2)
		b, _ = json.Marshal(r)
		var r2 Reply
		json.Unmarshal(b, &r2)
		return r2, e
	}}
	srv := &Server{Backend: backend, Stripe: fs, Prices: Prices{"power": "price_power", "eleet": "price_eleet"}, WebhookSecret: func() string { return secret }, Origin: "https://mea.test"}
	post := func(cookie, csrf, path string, body any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", path, bytes.NewReader(b))
		r.Header.Set("Origin", "https://mea.test")
		r.Header.Set("X-CSRF-Token", csrf)
		r.AddCookie(&http.Cookie{Name: "mea_session", Value: cookie})
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	hook := func(id, kind string, obj any) int {
		o, _ := json.Marshal(obj)
		body := []byte(fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":%s}}`, id, kind, time.Now().Unix(), o))
		r := httptest.NewRequest("POST", "/api/stripe/webhook", bytes.NewReader(body))
		r.Header.Set("Stripe-Signature", Sign(body, secret, time.Now()))
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w.Code
	}
	code, _, _ := beta.Create(ctx, st.DB, admin, beta.NewInvitation{Tiers: []string{"power", "eleet"}, MaxRedemptions: 1, ExpiresAt: time.Now().Add(time.Hour)})

	if c, _ := post(adminCookie, adminCSRF, "/api/billing/checkout", map[string]string{"Tier": "eleet", "Code": code}); c != 409 {
		t.Fatalf("administrator sent to Stripe: %d", c)
	}
	if c, _ := post(aliceCookie, "forged", "/api/billing/checkout", map[string]string{"Tier": "eleet", "Code": code}); c != 403 {
		t.Fatal("CSRF bypass")
	}
	if c, b := post(aliceCookie, aliceCSRF, "/api/billing/checkout", map[string]string{"Tier": "eleet", "Code": "MEA-WRONG"}); c != 403 {
		t.Fatalf("invalid code unlocked checkout %d %v", c, b)
	}
	c, b := post(aliceCookie, aliceCSRF, "/api/billing/checkout", map[string]string{"Tier": "eleet", "Code": code})
	if c != 200 || !strings.HasPrefix(b["url"].(string), "https://checkout.stripe.test/") {
		t.Fatalf("checkout %d %v", c, b)
	}
	var session string
	for id := range fs.sessions {
		session = id
	}
	// Reaching the success page grants nothing.
	if a, _ := beta.AccessFor(ctx, st.DB, alice); len(a.Tiers) != 0 {
		t.Fatal("access before webhook")
	}
	body := []byte(`{"id":"evt_forged","type":"checkout.session.completed","created":1,"data":{"object":{}}}`)
	r := httptest.NewRequest("POST", "/api/stripe/webhook", bytes.NewReader(body))
	r.Header.Set("Stripe-Signature", Sign(body, "whsec_attacker", time.Now()))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("forged webhook accepted")
	}
	customer, _ := DB{st}.Customer(ctx, alice)
	fs.subs["sub_alice"] = beta.Subscription{ID: "sub_alice", Customer: customer, Price: "price_eleet", Status: "active", InvoiceStatus: "paid", UserID: alice, Tier: "eleet"}
	completed := map[string]any{"id": session, "object": "checkout.session", "mode": "subscription", "subscription": "sub_alice"}
	if hook("evt_done", "checkout.session.completed", completed) != 200 || hook("evt_done", "checkout.session.completed", completed) != 200 {
		t.Fatal("completion or replay failed")
	}
	var redemptions int
	st.DB.QueryRow(ctx, `SELECT redemptions FROM mea.invitations`).Scan(&redemptions)
	if redemptions != 1 {
		t.Fatalf("webhook replay changed redemptions to %d", redemptions)
	}
	if a, _ := beta.AccessFor(ctx, st.DB, alice); !a.Has("eleet") {
		t.Fatalf("verified subscription did not unlock Eleet %+v", a)
	}
	fs.subs["sub_alice"] = beta.Subscription{ID: "sub_alice", Customer: customer, Price: "price_eleet", Status: "past_due", InvoiceStatus: "open", UserID: alice, Tier: "eleet"}
	hook("evt_fail", "invoice.payment_failed", map[string]any{"id": "in_1", "object": "invoice", "subscription": "sub_alice"})
	if a, _ := beta.AccessFor(ctx, st.DB, alice); a.Has("eleet") {
		t.Fatal("failed payment kept access")
	}
	fs.subs["sub_alice"] = beta.Subscription{ID: "sub_alice", Customer: customer, Price: "price_eleet", Status: "active", InvoiceStatus: "paid", UserID: alice, Tier: "eleet"}
	hook("evt_paid", "invoice.paid", map[string]any{"id": "in_2", "object": "invoice", "parent": map[string]any{"subscription_details": map[string]any{"subscription": "sub_alice"}}})
	if a, _ := beta.AccessFor(ctx, st.DB, alice); !a.Has("eleet") {
		t.Fatal("renewal did not restore access")
	}
	if c, b := post(aliceCookie, aliceCSRF, "/api/billing/change", map[string]string{"Tier": "power"}); c != 200 || b["tier"] != "power" {
		t.Fatalf("plan change %d %v", c, b)
	}
	if a, _ := beta.AccessFor(ctx, st.DB, alice); !a.Has("power") || a.Has("eleet") {
		t.Fatalf("plan change access %+v", a)
	}
	fs.subs["sub_alice"] = beta.Subscription{ID: "sub_alice", Customer: customer, Price: "price_power", Status: "canceled", UserID: alice, Tier: "power"}
	hook("evt_cancel", "customer.subscription.deleted", map[string]any{"id": "sub_alice", "object": "subscription"})
	if a, _ := beta.AccessFor(ctx, st.DB, alice); len(a.Tiers) != 0 {
		t.Fatal("cancellation kept access")
	}
	if c, b := post(aliceCookie, aliceCSRF, "/api/billing/portal", nil); c != 200 || !strings.Contains(b["url"].(string), customer) {
		t.Fatalf("portal %d %v", c, b)
	}

	// Abandoned checkout releases its hold when Stripe expires the session.
	bob, bobCookie, bobCSRF := mk("bob")
	single, _, _ := beta.Create(ctx, st.DB, admin, beta.NewInvitation{Tiers: []string{"power"}, MaxRedemptions: 1, ExpiresAt: time.Now().Add(time.Hour)})
	if c, _ := post(bobCookie, bobCSRF, "/api/billing/checkout", map[string]string{"Tier": "power", "Code": single}); c != 200 {
		t.Fatal("bob checkout")
	}
	carol, carolCookie, carolCSRF := mk("carol")
	_ = carol
	if c, _ := post(carolCookie, carolCSRF, "/api/billing/checkout", map[string]string{"Tier": "power", "Code": single}); c != 403 {
		t.Fatal("held single-use code accepted twice")
	}
	var bobSession string
	st.DB.QueryRow(ctx, `SELECT checkout_session_id FROM mea.invitation_reservations WHERE user_id=$1 AND state='reserved'`, bob).Scan(&bobSession)
	hook("evt_expired", "checkout.session.expired", map[string]any{"id": bobSession, "object": "checkout.session", "mode": "subscription"})
	if c, b := post(carolCookie, carolCSRF, "/api/billing/checkout", map[string]string{"Tier": "power", "Code": single}); c != 200 {
		t.Fatalf("abandoned checkout consumed the invitation %d %v", c, b)
	}
	var events int
	st.DB.QueryRow(ctx, `SELECT count(*) FROM mea.stripe_events WHERE processed_at IS NOT NULL`).Scan(&events)
	if events < 5 {
		t.Fatalf("processed events %d", events)
	}
}

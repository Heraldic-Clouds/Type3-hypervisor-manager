// Package billing integrates Stripe Billing in subscription mode. It talks to
// the Stripe REST API directly and only ever trusts Stripe's server state,
// fetched with the secret key, never the browser's return from Checkout.
package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
)

type Stripe interface {
	CreateCustomer(ctx context.Context, user, email, name string) (string, error)
	CreateCheckout(ctx context.Context, p CheckoutParams) (id, url string, e error)
	Portal(ctx context.Context, customer, returnURL string) (string, error)
	Subscription(ctx context.Context, id string) (beta.Subscription, error)
	ChangePrice(ctx context.Context, subscription, price, idempotency string) error
}

type CheckoutParams struct {
	Customer, Price, User, Tier, Reservation, Success, Cancel string
	ExpiresAt                                                 time.Time
}

// Prices maps MEA tiers to environment-configured Stripe price IDs.
type Prices map[string]string

func (p Prices) Tier(price string) string {
	for tier, id := range p {
		if id == price {
			return tier
		}
	}
	return ""
}

type Client struct {
	Key    string
	Base   string
	HTTP   *http.Client
	Prices Prices
}

func (c *Client) do(ctx context.Context, method, path string, form url.Values, idempotency string, out any) error {
	if c.Key == "" {
		return errors.New("Stripe is not configured for this deployment")
	}
	if !strings.HasPrefix(c.Key, "sk_test_") && !strings.HasPrefix(c.Key, "rk_test_") {
		// Live billing stays disabled until explicitly configured and approved.
		return errors.New("Stripe live mode is disabled for this deployment")
	}
	base := c.Base
	if base == "" {
		base = "https://api.stripe.com"
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, e := http.NewRequestWithContext(ctx, method, base+path, body)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 15 * time.Second}
	}
	resp, e := h.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if e != nil {
		return e
	}
	if resp.StatusCode >= 300 {
		var se struct {
			Error struct{ Type, Code, Message string }
		}
		_ = json.Unmarshal(raw, &se)
		return fmt.Errorf("stripe %s %s: %d %s %s", method, path, resp.StatusCode, se.Error.Type, se.Error.Code)
	}
	return json.Unmarshal(raw, out)
}

func (c *Client) CreateCustomer(ctx context.Context, user, email, name string) (string, error) {
	var out struct{ ID string }
	f := url.Values{"email": {email}, "name": {name}, "metadata[mea_user]": {user}}
	e := c.do(ctx, "POST", "/v1/customers", f, "mea-customer-"+user, &out)
	return out.ID, e
}

func (c *Client) CreateCheckout(ctx context.Context, p CheckoutParams) (string, string, error) {
	var out struct{ ID, URL string }
	f := url.Values{
		"mode": {"subscription"}, "customer": {p.Customer}, "client_reference_id": {p.User},
		"line_items[0][price]": {p.Price}, "line_items[0][quantity]": {"1"},
		"success_url": {p.Success}, "cancel_url": {p.Cancel}, "expires_at": {strconv.FormatInt(p.ExpiresAt.Unix(), 10)},
		"metadata[mea_user]": {p.User}, "metadata[mea_tier]": {p.Tier}, "metadata[mea_reservation]": {p.Reservation},
		"subscription_data[metadata][mea_user]": {p.User}, "subscription_data[metadata][mea_tier]": {p.Tier},
		"allow_promotion_codes": {"false"},
	}
	e := c.do(ctx, "POST", "/v1/checkout/sessions", f, "mea-checkout-"+p.Reservation, &out)
	return out.ID, out.URL, e
}

func (c *Client) Portal(ctx context.Context, customer, returnURL string) (string, error) {
	var out struct{ URL string }
	e := c.do(ctx, "POST", "/v1/billing_portal/sessions", url.Values{"customer": {customer}, "return_url": {returnURL}}, "", &out)
	return out.URL, e
}

func (c *Client) ChangePrice(ctx context.Context, subscription, price, idempotency string) error {
	s, e := c.raw(ctx, subscription)
	if e != nil {
		return e
	}
	if len(s.Items.Data) != 1 {
		return errors.New("subscription must have exactly one item")
	}
	f := url.Values{"items[0][id]": {s.Items.Data[0].ID}, "items[0][price]": {price}, "proration_behavior": {"create_prorations"}, "payment_behavior": {"pending_if_incomplete"}}
	var out map[string]any
	return c.do(ctx, "POST", "/v1/subscriptions/"+url.PathEscape(subscription), f, idempotency, &out)
}

type rawSub struct {
	ID                string `json:"id"`
	Customer          string `json:"customer"`
	Status            string `json:"status"`
	CancelAtPeriodEnd bool   `json:"cancel_at_period_end"`
	CurrentPeriodEnd  int64  `json:"current_period_end"`
	Metadata          map[string]string
	LatestInvoice     *struct {
		Status string `json:"status"`
	} `json:"latest_invoice"`
	Items struct {
		Data []struct {
			ID               string `json:"id"`
			CurrentPeriodEnd int64  `json:"current_period_end"`
			Price            struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

func (c *Client) raw(ctx context.Context, id string) (rawSub, error) {
	var s rawSub
	if !strings.HasPrefix(id, "sub_") {
		return s, errors.New("invalid subscription id")
	}
	e := c.do(ctx, "GET", "/v1/subscriptions/"+url.PathEscape(id)+"?expand[]=latest_invoice", nil, "", &s)
	return s, e
}

// Subscription returns the authoritative state. The MEA tier is derived from
// the configured price IDs, not from editable metadata.
func (c *Client) Subscription(ctx context.Context, id string) (beta.Subscription, error) {
	s, e := c.raw(ctx, id)
	if e != nil {
		return beta.Subscription{}, e
	}
	if len(s.Items.Data) != 1 {
		return beta.Subscription{}, errors.New("subscription must have exactly one item")
	}
	end := s.CurrentPeriodEnd
	if end == 0 {
		end = s.Items.Data[0].CurrentPeriodEnd
	}
	out := beta.Subscription{ID: s.ID, Customer: s.Customer, Price: s.Items.Data[0].Price.ID, Status: s.Status, CancelAtPeriodEnd: s.CancelAtPeriodEnd, CurrentPeriodEnd: end, UserID: s.Metadata["mea_user"], Updated: time.Now().UnixNano()}
	out.Tier = c.Prices.Tier(out.Price)
	if s.LatestInvoice != nil {
		out.InvoiceStatus = s.LatestInvoice.Status
	}
	return out, nil
}

// Event is the subset of a Stripe event MEA needs.
type Event struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Created int64  `json:"created"`
	Data    struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

var ErrSignature = errors.New("invalid Stripe signature")

// Verify checks the Stripe-Signature header (HMAC-SHA256 over "t.payload")
// against the endpoint secret with a five-minute replay tolerance.
func Verify(payload []byte, header, secret string, now time.Time) (Event, error) {
	var ev Event
	if secret == "" {
		return ev, ErrSignature
	}
	var ts int64
	var sigs [][]byte
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			if b, e := hex.DecodeString(v); e == nil {
				sigs = append(sigs, b)
			}
		}
	}
	if ts == 0 || len(sigs) == 0 {
		return ev, ErrSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return ev, ErrSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	want := mac.Sum(nil)
	for _, s := range sigs {
		if hmac.Equal(s, want) {
			if e := json.Unmarshal(payload, &ev); e != nil || ev.ID == "" {
				return ev, ErrSignature
			}
			return ev, nil
		}
	}
	return ev, ErrSignature
}

// Sign produces a Stripe-Signature header, for tests and the local simulator.
func Sign(payload []byte, secret string, ts time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts.Unix())
	mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", ts.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

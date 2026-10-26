package billing

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/security"
)

type Server struct {
	Backend       Backend
	Stripe        Stripe
	Prices        Prices
	WebhookSecret func() string
	Origin        string
	Now           func() time.Time
}

func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "/api/stripe/webhook" && r.Method == "POST" {
		s.webhook(w, r)
		return
	}
	if r.Method != "POST" || r.Header.Get("Origin") != s.Origin {
		write(w, 403, map[string]string{"error": "Invalid request origin"})
		return
	}
	e := s.customer(w, r, path)
	if e == nil {
		return
	}
	var ue UserError
	if errors.As(e, &ue) {
		write(w, ue.Code, map[string]string{"error": ue.Message})
		return
	}
	slog.Error("billing request failed", "path", path, "error", e)
	write(w, 503, map[string]string{"error": "Billing is temporarily unavailable. Please try again."})
}

func (s *Server) customer(w http.ResponseWriter, r *http.Request, path string) error {
	c, e := r.Cookie("mea_session")
	if e != nil {
		return UserError{401, "Please sign in"}
	}
	a, e := s.Backend.Session(r.Context(), security.Hash(c.Value))
	if e != nil {
		return e
	}
	if r.Header.Get("X-CSRF-Token") != a.CSRF {
		return UserError{403, "Invalid CSRF token"}
	}
	var b struct{ Tier, Code string }
	if path != "/api/billing/portal" {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if d.Decode(&b) != nil {
			return UserError{400, "Invalid request"}
		}
		b.Tier = trimTier(b.Tier)
		if !beta.ValidTier(b.Tier) || s.Prices[b.Tier] == "" {
			return UserError{400, "Choose Power or Eleet"}
		}
	}
	switch path {
	case "/api/billing/checkout":
		return s.checkout(w, r, a, b.Tier, b.Code)
	case "/api/billing/portal":
		customer, e := s.Backend.Customer(r.Context(), a.ID)
		if e != nil {
			return e
		}
		if customer == "" {
			return UserError{404, "No billing account yet"}
		}
		u, e := s.Stripe.Portal(r.Context(), customer, s.Origin+"/plans")
		if e != nil {
			return e
		}
		write(w, 200, map[string]string{"url": u})
		return nil
	case "/api/billing/change":
		return s.change(w, r, a, b.Tier, b.Code)
	}
	return UserError{404, "Not found"}
}

func (s *Server) checkout(w http.ResponseWriter, r *http.Request, a Account, tier, code string) error {
	ctx := r.Context()
	if !a.Verified {
		return UserError{403, "Verify your email before subscribing"}
	}
	if a.Access.Admin {
		return UserError{409, "Administrators already have Power and Eleet without a subscription"}
	}
	if len(a.Access.Tiers) > 0 || (a.Access.Subscription != nil && a.Access.Subscription.Status != "canceled" && a.Access.Subscription.Status != "incomplete_expired") {
		return UserError{409, "You already have a subscription. Use Change plan or Manage billing."}
	}
	ip := r.Header.Get("X-Mea-Source-Ip")
	if ip == "" {
		ip = r.RemoteAddr
	}
	// The backend validates the invitation; nothing the browser sends unlocks Checkout.
	res, e := s.Backend.Reserve(ctx, a.ID, a.Email, code, tier, ip)
	if e != nil {
		return e
	}
	customer, e := s.Backend.Customer(ctx, a.ID)
	if e != nil {
		return e
	}
	if customer == "" {
		if customer, e = s.Stripe.CreateCustomer(ctx, a.ID, a.Email, a.Name); e != nil {
			return e
		}
		if e = s.Backend.SetCustomer(ctx, a.ID, customer); e != nil {
			return e
		}
		if customer, e = s.Backend.Customer(ctx, a.ID); e != nil {
			return e
		}
	}
	id, u, e := s.Stripe.CreateCheckout(ctx, CheckoutParams{Customer: customer, Price: s.Prices[tier], User: a.ID, Tier: tier, Reservation: res,
		Success: s.Origin + "/plans?checkout=success", Cancel: s.Origin + "/plans?checkout=cancelled", ExpiresAt: s.now().Add(30 * time.Minute)})
	if e != nil {
		return e
	}
	if e = s.Backend.Attach(ctx, res, id); e != nil {
		return e
	}
	write(w, 200, map[string]string{"url": u})
	return nil
}

// change switches an active subscription between Power and Eleet. The target
// tier must be covered by an invitation the user already redeemed or a new
// valid code for that tier.
func (s *Server) change(w http.ResponseWriter, r *http.Request, a Account, tier, code string) error {
	ctx := r.Context()
	sub := a.Access.Subscription
	if sub == nil || !beta.Entitled(sub.Status, "") {
		return UserError{409, "You need an active subscription to change plans"}
	}
	if sub.Tier == tier {
		return UserError{409, "You are already on that plan"}
	}
	tiers, e := s.Backend.RedeemedTiers(ctx, a.ID)
	if e != nil {
		return e
	}
	ok := false
	for _, t := range tiers {
		ok = ok || t == tier
	}
	res := ""
	if !ok {
		if strings.TrimSpace(code) == "" {
			return UserError{403, "Changing to this plan needs an invitation code for it."}
		}
		ip := r.Header.Get("X-Mea-Source-Ip")
		if res, e = s.Backend.Reserve(ctx, a.ID, a.Email, code, tier, ip); e != nil {
			return e
		}
		if e = s.Backend.Attach(ctx, res, "change_"+res); e != nil {
			return e
		}
	}
	key := "mea-change-" + sub.ID + "-" + tier + "-" + res
	if e = s.Stripe.ChangePrice(ctx, sub.ID, s.Prices[tier], key); e != nil {
		return e
	}
	fresh, e := s.Stripe.Subscription(ctx, sub.ID)
	if e != nil {
		return e
	}
	if res != "" {
		e = s.Backend.CompleteCheckout(ctx, "change_"+res, fresh)
	} else {
		e = s.Backend.ApplySubscription(ctx, fresh)
	}
	if e != nil {
		return e
	}
	write(w, 200, map[string]any{"tier": fresh.Tier, "status": fresh.Status})
	return nil
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	payload, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 256*1024))
	if e != nil {
		write(w, 400, map[string]string{"error": "invalid payload"})
		return
	}
	ev, e := Verify(payload, r.Header.Get("Stripe-Signature"), s.WebhookSecret(), s.now())
	if e != nil {
		write(w, 400, map[string]string{"error": "invalid signature"})
		return
	}
	done, e := s.Backend.BeginEvent(ctx, ev.ID, ev.Type, ev.Created)
	if e != nil {
		write(w, 503, map[string]string{"error": "retry"})
		return
	}
	if done {
		write(w, 200, map[string]bool{"received": true, "duplicate": true})
		return
	}
	if e = s.process(r, ev); e != nil {
		slog.Error("stripe event failed", "event", ev.ID, "type", ev.Type, "error", e)
		_ = s.Backend.FinishEvent(ctx, ev.ID, e.Error())
		// A 5xx makes Stripe retry with backoff.
		write(w, 500, map[string]string{"error": "processing failed"})
		return
	}
	_ = s.Backend.FinishEvent(ctx, ev.ID, "")
	write(w, 200, map[string]bool{"received": true})
}

func (s *Server) process(r *http.Request, ev Event) error {
	ctx := r.Context()
	var obj struct {
		ID           string `json:"id"`
		Object       string `json:"object"`
		Mode         string `json:"mode"`
		Status       string `json:"status"`
		PaymentState string `json:"payment_status"`
		Subscription any    `json:"subscription"`
		Parent       *struct {
			SubscriptionDetails *struct {
				Subscription string `json:"subscription"`
			} `json:"subscription_details"`
		} `json:"parent"`
	}
	if e := json.Unmarshal(ev.Data.Object, &obj); e != nil {
		return e
	}
	subID := ""
	switch v := obj.Subscription.(type) {
	case string:
		subID = v
	case map[string]any:
		subID, _ = v["id"].(string)
	}
	if subID == "" && obj.Parent != nil && obj.Parent.SubscriptionDetails != nil {
		subID = obj.Parent.SubscriptionDetails.Subscription
	}
	switch ev.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		if obj.Mode != "subscription" || subID == "" {
			return nil
		}
		sub, e := s.Stripe.Subscription(ctx, subID)
		if e != nil {
			return e
		}
		return s.Backend.CompleteCheckout(ctx, obj.ID, sub)
	case "checkout.session.expired":
		return s.Backend.Release(ctx, obj.ID)
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted",
		"customer.subscription.paused", "customer.subscription.resumed":
		subID = obj.ID
		fallthrough
	case "invoice.paid", "invoice.payment_succeeded", "invoice.payment_failed", "invoice.payment_action_required":
		if subID == "" {
			return nil
		}
		// Never trust the event body for entitlement; re-read Stripe.
		sub, e := s.Stripe.Subscription(ctx, subID)
		if e != nil {
			return e
		}
		return s.Backend.ApplySubscription(ctx, sub)
	}
	return nil
}

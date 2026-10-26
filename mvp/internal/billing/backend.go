package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/security"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
	"github.com/jackc/pgx/v5"
)

// Backend is the billing function's view of the database. In AWS the billing
// function has internet access but no database route, so it reaches these
// operations through the VPC control function; locally it calls DB directly.
type Backend interface {
	Session(ctx context.Context, hash string) (Account, error)
	Reserve(ctx context.Context, user, email, code, tier, ip string) (string, error)
	Attach(ctx context.Context, reservation, session string) error
	Customer(ctx context.Context, user string) (string, error)
	SetCustomer(ctx context.Context, user, customer string) error
	BeginEvent(ctx context.Context, id, kind string, created int64) (bool, error)
	FinishEvent(ctx context.Context, id, failure string) error
	CompleteCheckout(ctx context.Context, session string, s beta.Subscription) error
	ApplySubscription(ctx context.Context, s beta.Subscription) error
	Release(ctx context.Context, session string) error
	RedeemedTiers(ctx context.Context, user string) ([]string, error)
}

type Account struct {
	ID, Username, Email, Name, CSRF string
	Verified                        bool
	Access                          beta.Access
}

// UserError is shown to the customer as-is; anything else is a 503.
type UserError struct {
	Code    int
	Message string
}

func (e UserError) Error() string { return e.Message }

type DB struct{ Store *store.Store }

func (d DB) Session(ctx context.Context, hash string) (Account, error) {
	var a Account
	e := d.Store.DB.QueryRow(ctx, `SELECT u.id::text,u.username,u.email,u.name,u.verified,s.csrf FROM mea.sessions s JOIN mea.users u ON u.id=s.user_id
 WHERE s.hash=$1 AND s.revoked_at IS NULL AND s.expires_at>now() AND NOT u.disabled`, hash).Scan(&a.ID, &a.Username, &a.Email, &a.Name, &a.Verified, &a.CSRF)
	if errors.Is(e, pgx.ErrNoRows) {
		return a, UserError{401, "Your session has expired. Please sign in."}
	}
	if e != nil {
		return a, e
	}
	a.Access, e = beta.AccessFor(ctx, d.Store.DB, a.ID)
	return a, e
}

func (d DB) Reserve(ctx context.Context, user, email, code, tier, ip string) (string, error) {
	// Per-account and per-address limits make guessing 100-bit codes pointless
	// and stop enumeration of invitation state.
	if !d.Store.Rate(ctx, "invite-user:"+user, 10, time.Hour) || !d.Store.Rate(ctx, "invite-ip:"+security.Hash(ip), 30, time.Hour) {
		return "", UserError{429, "Too many invitation attempts. Try again later."}
	}
	id, e := beta.Reserve(ctx, d.Store.DB, user, email, code, tier, 35*time.Minute)
	if errors.Is(e, beta.ErrInvalidCode) || errors.Is(e, beta.ErrBusy) {
		_ = beta.Audit(ctx, d.Store.DB, user, "invitation.rejected", "", map[string]string{"tier": tier})
		return "", UserError{403, e.Error()}
	}
	return id, e
}
func (d DB) Attach(ctx context.Context, reservation, session string) error {
	return beta.AttachCheckout(ctx, d.Store.DB, reservation, session)
}
func (d DB) Customer(ctx context.Context, user string) (string, error) {
	var c string
	e := d.Store.DB.QueryRow(ctx, `SELECT customer_id FROM mea.stripe_customers WHERE user_id=$1`, user).Scan(&c)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", nil
	}
	return c, e
}
func (d DB) SetCustomer(ctx context.Context, user, customer string) error {
	_, e := d.Store.DB.Exec(ctx, `INSERT INTO mea.stripe_customers(user_id,customer_id) VALUES($1,$2) ON CONFLICT(user_id) DO NOTHING`, user, customer)
	return e
}
func (d DB) BeginEvent(ctx context.Context, id, kind string, created int64) (bool, error) {
	var processed bool
	e := d.Store.DB.QueryRow(ctx, `INSERT INTO mea.stripe_events(id,type,created) VALUES($1,$2,$3)
 ON CONFLICT(id) DO UPDATE SET received_at=now() RETURNING processed_at IS NOT NULL`, id, kind, created).Scan(&processed)
	return processed, e
}
func (d DB) FinishEvent(ctx context.Context, id, failure string) error {
	if failure != "" {
		_, e := d.Store.DB.Exec(ctx, `UPDATE mea.stripe_events SET error=$2 WHERE id=$1`, id, failure)
		return e
	}
	_, e := d.Store.DB.Exec(ctx, `UPDATE mea.stripe_events SET processed_at=now(),error=NULL WHERE id=$1`, id)
	return e
}

// owned verifies the Stripe customer belongs to the MEA user the subscription
// metadata names, so edited metadata cannot move a subscription to another account.
func owned(ctx context.Context, tx pgx.Tx, s beta.Subscription) error {
	var customer string
	e := tx.QueryRow(ctx, `SELECT customer_id FROM mea.stripe_customers WHERE user_id::text=$1`, s.UserID).Scan(&customer)
	if e != nil || customer != s.Customer {
		return fmt.Errorf("subscription %s customer does not match its MEA account", s.ID)
	}
	return nil
}
func (d DB) CompleteCheckout(ctx context.Context, session string, s beta.Subscription) error {
	tx, e := d.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	user, tier, e := beta.Redeem(ctx, tx, session)
	if e != nil {
		return e
	}
	if user != s.UserID || tier != s.Tier {
		return fmt.Errorf("checkout %s does not match its invitation reservation", session)
	}
	if e = owned(ctx, tx, s); e != nil {
		return e
	}
	if e = beta.ApplySubscription(ctx, tx, s); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (d DB) ApplySubscription(ctx context.Context, s beta.Subscription) error {
	tx, e := d.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if e = owned(ctx, tx, s); e != nil {
		return e
	}
	// Only subscriptions created by a redeemed invitation checkout, or already
	// known, grant service.
	var known bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mea.subscriptions WHERE id=$1)`, s.ID).Scan(&known); e != nil {
		return e
	}
	if !known {
		return nil
	}
	if e = beta.ApplySubscription(ctx, tx, s); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (d DB) Release(ctx context.Context, session string) error {
	return beta.Release(ctx, d.Store.DB, session)
}
func (d DB) RedeemedTiers(ctx context.Context, user string) ([]string, error) {
	return beta.RedeemedTiers(ctx, d.Store.DB, user)
}

// Call is the wire format between the billing and control functions.
type Call struct {
	Op           string            `json:"op"`
	Hash         string            `json:"hash,omitempty"`
	User         string            `json:"user,omitempty"`
	Email        string            `json:"email,omitempty"`
	Code         string            `json:"code,omitempty"`
	Tier         string            `json:"tier,omitempty"`
	IP           string            `json:"ip,omitempty"`
	ID           string            `json:"id,omitempty"`
	Session      string            `json:"session,omitempty"`
	Kind         string            `json:"kind,omitempty"`
	Created      int64             `json:"created,omitempty"`
	Failure      string            `json:"failure,omitempty"`
	Subscription *beta.Subscription `json:"subscription,omitempty"`
}
type Reply struct {
	Account   *Account `json:"account,omitempty"`
	Value     string   `json:"value,omitempty"`
	Values    []string `json:"values,omitempty"`
	Flag      bool     `json:"flag,omitempty"`
	UserError *UserError `json:"userError,omitempty"`
}

// Dispatch runs one Call against a Backend; the control function uses it.
func Dispatch(ctx context.Context, b Backend, c Call) (Reply, error) {
	var r Reply
	var e error
	switch c.Op {
	case "session":
		var a Account
		a, e = b.Session(ctx, c.Hash)
		r.Account = &a
	case "reserve":
		r.Value, e = b.Reserve(ctx, c.User, c.Email, c.Code, c.Tier, c.IP)
	case "attach":
		e = b.Attach(ctx, c.ID, c.Session)
	case "customer":
		r.Value, e = b.Customer(ctx, c.User)
	case "set-customer":
		e = b.SetCustomer(ctx, c.User, c.ID)
	case "begin-event":
		r.Flag, e = b.BeginEvent(ctx, c.ID, c.Kind, c.Created)
	case "finish-event":
		e = b.FinishEvent(ctx, c.ID, c.Failure)
	case "complete":
		if c.Subscription == nil {
			return r, errors.New("subscription required")
		}
		e = b.CompleteCheckout(ctx, c.Session, *c.Subscription)
	case "subscription":
		if c.Subscription == nil {
			return r, errors.New("subscription required")
		}
		e = b.ApplySubscription(ctx, *c.Subscription)
	case "release":
		e = b.Release(ctx, c.Session)
	case "redeemed-tiers":
		r.Values, e = b.RedeemedTiers(ctx, c.User)
	default:
		return r, fmt.Errorf("unknown billing op %q", c.Op)
	}
	var ue UserError
	if errors.As(e, &ue) {
		r.UserError = &ue
		r.Account = nil
		return r, nil
	}
	return r, e
}

// Remote is a Backend that sends Calls through an invoker (the control Lambda).
type Remote struct {
	Invoke func(ctx context.Context, c Call) (Reply, error)
}

func (r Remote) call(ctx context.Context, c Call) (Reply, error) {
	out, e := r.Invoke(ctx, c)
	if e != nil {
		return out, e
	}
	if out.UserError != nil {
		return out, *out.UserError
	}
	return out, nil
}
func (r Remote) Session(ctx context.Context, hash string) (Account, error) {
	out, e := r.call(ctx, Call{Op: "session", Hash: hash})
	if e != nil || out.Account == nil {
		return Account{}, errors.Join(e, errors.New("no account"))
	}
	return *out.Account, nil
}
func (r Remote) Reserve(ctx context.Context, user, email, code, tier, ip string) (string, error) {
	out, e := r.call(ctx, Call{Op: "reserve", User: user, Email: email, Code: code, Tier: tier, IP: ip})
	return out.Value, e
}
func (r Remote) Attach(ctx context.Context, reservation, session string) error {
	_, e := r.call(ctx, Call{Op: "attach", ID: reservation, Session: session})
	return e
}
func (r Remote) Customer(ctx context.Context, user string) (string, error) {
	out, e := r.call(ctx, Call{Op: "customer", User: user})
	return out.Value, e
}
func (r Remote) SetCustomer(ctx context.Context, user, customer string) error {
	_, e := r.call(ctx, Call{Op: "set-customer", User: user, ID: customer})
	return e
}
func (r Remote) BeginEvent(ctx context.Context, id, kind string, created int64) (bool, error) {
	out, e := r.call(ctx, Call{Op: "begin-event", ID: id, Kind: kind, Created: created})
	return out.Flag, e
}
func (r Remote) FinishEvent(ctx context.Context, id, failure string) error {
	_, e := r.call(ctx, Call{Op: "finish-event", ID: id, Failure: failure})
	return e
}
func (r Remote) CompleteCheckout(ctx context.Context, session string, s beta.Subscription) error {
	_, e := r.call(ctx, Call{Op: "complete", Session: session, Subscription: &s})
	return e
}
func (r Remote) ApplySubscription(ctx context.Context, s beta.Subscription) error {
	_, e := r.call(ctx, Call{Op: "subscription", Subscription: &s})
	return e
}
func (r Remote) Release(ctx context.Context, session string) error {
	_, e := r.call(ctx, Call{Op: "release", Session: session})
	return e
}
func (r Remote) RedeemedTiers(ctx context.Context, user string) ([]string, error) {
	out, e := r.call(ctx, Call{Op: "redeemed-tiers", User: user})
	return out.Values, e
}

var _ Backend = DB{}
var _ Backend = Remote{}

// Encode helps the control function return Replies in Lambda payloads.
func Encode(r Reply) json.RawMessage { b, _ := json.Marshal(r); return b }

func trimTier(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

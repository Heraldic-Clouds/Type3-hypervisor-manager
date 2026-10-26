// Package beta implements private-beta invitations, entitlements and
// administrator roles. Invitations authorize eligibility for a paid tier;
// they are not Stripe coupons. Entitlements are the server-side source of
// access: 'stripe' rows follow Stripe, 'admin_grant' rows never do.
package beta

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/security"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is satisfied by *pgxpool.Pool and pgx.Tx.
type DB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}

var Tiers = []string{"power", "eleet"}

func ValidTier(t string) bool { return t == "power" || t == "eleet" }

// ErrInvalidCode is deliberately uniform: unknown, expired, revoked,
// exhausted, wrong-tier and wrong-account codes are indistinguishable.
var ErrInvalidCode = errors.New("This invitation code is not valid for this plan or account")
var ErrBusy = errors.New("A checkout is already in progress for this invitation. Try again in a few minutes.")

// Crockford base32 without I, L, O, U: 20 symbols carry 100 bits.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func NewCode() string {
	b := make([]byte, 20)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	var s strings.Builder
	s.WriteString("MEA")
	for i, c := range b {
		if i%5 == 0 {
			s.WriteByte('-')
		}
		s.WriteByte(alphabet[int(c)%32])
	}
	return s.String()
}

// Normalize accepts lower case, spaces and missing dashes.
func Normalize(code string) string {
	var s strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch {
		case r == 'O':
			s.WriteByte('0')
		case r == 'I' || r == 'L':
			s.WriteByte('1')
		case strings.ContainsRune(alphabet, r):
			s.WriteRune(r)
		}
	}
	return s.String()
}
func CodeHash(code string) string { return security.Hash("mea-invitation:" + Normalize(code)) }

type Invitation struct {
	ID             string     `json:"id"`
	Hint           string     `json:"hint"`
	Tiers          []string   `json:"tiers"`
	MaxRedemptions int        `json:"maxRedemptions"`
	Redemptions    int        `json:"redemptions"`
	Reserved       int        `json:"reserved"`
	Email          *string    `json:"email"`
	Note           string     `json:"note"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	CreatedBy      string     `json:"createdBy"`
	CreatedAt      time.Time  `json:"createdAt"`
	RevokedAt      *time.Time `json:"revokedAt"`
	Status         string     `json:"status"`
}

type NewInvitation struct {
	Tiers          []string
	MaxRedemptions int
	ExpiresAt      time.Time
	Email, Note    string
}

func Audit(ctx context.Context, db DB, actor, action, target string, details any) error {
	b, _ := json.Marshal(details)
	if details == nil {
		b = []byte("{}")
	}
	var a any
	if actor != "" {
		a = actor
	}
	_, e := db.Exec(ctx, `INSERT INTO mea.admin_audit(actor_id,action,target,details) VALUES($1,$2,$3,$4)`, a, action, target, b)
	return e
}

// Create returns the plaintext code exactly once; only its hash is stored.
func Create(ctx context.Context, db DB, actor string, n NewInvitation) (string, Invitation, error) {
	if len(n.Tiers) == 0 || len(n.Tiers) > 2 {
		return "", Invitation{}, errors.New("Choose Power, Eleet or both")
	}
	seen := map[string]bool{}
	for _, t := range n.Tiers {
		if !ValidTier(t) || seen[t] {
			return "", Invitation{}, errors.New("Choose Power, Eleet or both")
		}
		seen[t] = true
	}
	if n.MaxRedemptions < 1 || n.MaxRedemptions > 10000 {
		return "", Invitation{}, errors.New("Redemptions must be between 1 and 10000")
	}
	if !n.ExpiresAt.After(time.Now().Add(time.Minute)) || n.ExpiresAt.After(time.Now().AddDate(1, 0, 0)) {
		return "", Invitation{}, errors.New("Expiry must be within the next year")
	}
	n.Email = strings.ToLower(strings.TrimSpace(n.Email))
	if len(n.Note) > 200 || len(n.Email) > 254 {
		return "", Invitation{}, errors.New("Note or email is too long")
	}
	tx, e := db.Begin(ctx)
	if e != nil {
		return "", Invitation{}, e
	}
	defer tx.Rollback(ctx)
	code := NewCode()
	id := security.ID()
	var email any
	if n.Email != "" {
		email = n.Email
	}
	norm := Normalize(code)
	if _, e = tx.Exec(ctx, `INSERT INTO mea.invitations(id,code_hash,hint,tiers,max_redemptions,email,note,expires_at,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		id, CodeHash(code), norm[len(norm)-4:], n.Tiers, n.MaxRedemptions, email, n.Note, n.ExpiresAt, actor); e != nil {
		return "", Invitation{}, e
	}
	if e = Audit(ctx, tx, actor, "invitation.create", id, map[string]any{"tiers": n.Tiers, "maxRedemptions": n.MaxRedemptions, "expiresAt": n.ExpiresAt, "restricted": n.Email != ""}); e != nil {
		return "", Invitation{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return "", Invitation{}, e
	}
	inv, e := Get(ctx, db, id)
	return code, inv, e
}

const invitationColumns = `i.id::text,i.hint,i.tiers,i.max_redemptions,i.redemptions,
 (SELECT count(*) FROM mea.invitation_reservations r WHERE r.invitation_id=i.id AND r.state='reserved' AND r.expires_at>now())::int,
 i.email,i.note,i.expires_at,i.created_by::text,i.created_at,i.revoked_at`

func scan(row pgx.Row) (Invitation, error) {
	var i Invitation
	e := row.Scan(&i.ID, &i.Hint, &i.Tiers, &i.MaxRedemptions, &i.Redemptions, &i.Reserved, &i.Email, &i.Note, &i.ExpiresAt, &i.CreatedBy, &i.CreatedAt, &i.RevokedAt)
	switch {
	case i.RevokedAt != nil:
		i.Status = "revoked"
	case !i.ExpiresAt.After(time.Now()):
		i.Status = "expired"
	case i.Redemptions >= i.MaxRedemptions:
		i.Status = "exhausted"
	default:
		i.Status = "active"
	}
	return i, e
}
func Get(ctx context.Context, db DB, id string) (Invitation, error) {
	return scan(db.QueryRow(ctx, "SELECT "+invitationColumns+" FROM mea.invitations i WHERE i.id=$1", id))
}
func List(ctx context.Context, db DB) ([]Invitation, error) {
	rows, e := db.Query(ctx, "SELECT "+invitationColumns+" FROM mea.invitations i ORDER BY i.created_at DESC LIMIT 500")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Invitation{}
	for rows.Next() {
		i, e := scan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
func Revoke(ctx context.Context, db DB, actor, id string) error {
	tx, e := db.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	tag, e := tx.Exec(ctx, `UPDATE mea.invitations SET revoked_at=now(),revoked_by=$2 WHERE id=$1 AND revoked_at IS NULL`, id, actor)
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	if e = Audit(ctx, tx, actor, "invitation.revoke", id, nil); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

type Redemption struct {
	ID              string     `json:"id"`
	InvitationID    string     `json:"invitationId"`
	Username        string     `json:"username"`
	Email           string     `json:"email"`
	Tier            string     `json:"tier"`
	State           string     `json:"state"`
	CreatedAt       time.Time  `json:"createdAt"`
	RedeemedAt      *time.Time `json:"redeemedAt"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	CheckoutSession *string    `json:"checkoutSession"`
}

func Redemptions(ctx context.Context, db DB, invitation string) ([]Redemption, error) {
	rows, e := db.Query(ctx, `SELECT r.id::text,r.invitation_id::text,u.username,u.email,r.tier,
 CASE WHEN r.state='reserved' AND r.expires_at<=now() THEN 'released' ELSE r.state END,r.created_at,r.redeemed_at,r.expires_at,r.checkout_session_id
 FROM mea.invitation_reservations r JOIN mea.users u ON u.id=r.user_id WHERE ($1='' OR r.invitation_id::text=$1) ORDER BY r.created_at DESC LIMIT 500`, invitation)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Redemption{}
	for rows.Next() {
		var r Redemption
		if e = rows.Scan(&r.ID, &r.InvitationID, &r.Username, &r.Email, &r.Tier, &r.State, &r.CreatedAt, &r.RedeemedAt, &r.ExpiresAt, &r.CheckoutSession); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Reserve holds one redemption slot for a checkout. Reservations count
// toward the limit until they are redeemed, released or expire, so parallel
// checkouts can never oversubscribe an invitation, and an abandoned checkout
// returns its slot when the reservation expires.
func Reserve(ctx context.Context, db DB, user, email, code, tier string, ttl time.Duration) (string, error) {
	if !ValidTier(tier) {
		return "", ErrInvalidCode
	}
	if len(Normalize(code)) != 23 {
		return "", ErrInvalidCode
	}
	tx, e := db.Begin(ctx)
	if e != nil {
		return "", e
	}
	defer tx.Rollback(ctx)
	var id string
	var tiers []string
	var max, used int
	var restrict *string
	e = tx.QueryRow(ctx, `SELECT id::text,tiers,max_redemptions,redemptions,email FROM mea.invitations
 WHERE code_hash=$1 AND revoked_at IS NULL AND expires_at>now() FOR UPDATE`, CodeHash(code)).Scan(&id, &tiers, &max, &used, &restrict)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", ErrInvalidCode
	}
	if e != nil {
		return "", e
	}
	allowed := false
	for _, t := range tiers {
		allowed = allowed || t == tier
	}
	if !allowed || (restrict != nil && *restrict != strings.ToLower(email)) {
		return "", ErrInvalidCode
	}
	// A user restarting checkout gives up their own earlier hold first.
	if _, e = tx.Exec(ctx, `UPDATE mea.invitation_reservations SET state='released',released_at=now() WHERE user_id=$1 AND state='reserved'`, user); e != nil {
		return "", e
	}
	var held int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM mea.invitation_reservations WHERE invitation_id=$1 AND state='reserved' AND expires_at>now()`, id).Scan(&held); e != nil {
		return "", e
	}
	if used >= max {
		return "", ErrInvalidCode
	}
	if used+held >= max {
		return "", ErrBusy
	}
	res := security.ID()
	if _, e = tx.Exec(ctx, `INSERT INTO mea.invitation_reservations(id,invitation_id,user_id,tier,state,expires_at) VALUES($1,$2,$3,$4,'reserved',now()+$5::interval)`, res, id, user, tier, fmt.Sprintf("%d seconds", int(ttl.Seconds()))); e != nil {
		return "", e
	}
	return res, tx.Commit(ctx)
}

func AttachCheckout(ctx context.Context, db DB, reservation, session string) error {
	tag, e := db.Exec(ctx, `UPDATE mea.invitation_reservations SET checkout_session_id=$2 WHERE id=$1 AND state='reserved' AND checkout_session_id IS NULL`, reservation, session)
	if e == nil && tag.RowsAffected() != 1 {
		return errors.New("reservation is no longer held")
	}
	return e
}

// Redeem consumes the slot when Stripe confirms the checkout. It is
// idempotent: a replayed completion returns the same reservation. A late
// completion for a reservation that already expired still redeems if a slot
// remains, because the customer has paid.
func Redeem(ctx context.Context, tx pgx.Tx, session string) (user, tier string, e error) {
	var id, invitation, state string
	e = tx.QueryRow(ctx, `SELECT id::text,invitation_id::text,user_id::text,tier,state FROM mea.invitation_reservations WHERE checkout_session_id=$1 FOR UPDATE`, session).Scan(&id, &invitation, &user, &tier, &state)
	if e != nil {
		return
	}
	if state == "redeemed" {
		return
	}
	tag, e := tx.Exec(ctx, `UPDATE mea.invitations SET redemptions=redemptions+1 WHERE id=$1 AND redemptions<max_redemptions`, invitation)
	if e != nil {
		return
	}
	if tag.RowsAffected() != 1 {
		return user, tier, errors.New("invitation exhausted before checkout completed")
	}
	_, e = tx.Exec(ctx, `UPDATE mea.invitation_reservations SET state='redeemed',redeemed_at=now() WHERE id=$1`, id)
	return
}

func Release(ctx context.Context, db DB, session string) error {
	_, e := db.Exec(ctx, `UPDATE mea.invitation_reservations SET state='released',released_at=now() WHERE checkout_session_id=$1 AND state='reserved'`, session)
	return e
}
func ReleaseExpired(ctx context.Context, db DB) error {
	_, e := db.Exec(ctx, `UPDATE mea.invitation_reservations SET state='released',released_at=now() WHERE state='reserved' AND expires_at<now()-interval '1 hour'`)
	return e
}

// RedeemedTiers is what a user's completed invitations allow, for plan changes.
func RedeemedTiers(ctx context.Context, db DB, user string) ([]string, error) {
	rows, e := db.Query(ctx, `SELECT DISTINCT unnest(i.tiers) FROM mea.invitation_reservations r JOIN mea.invitations i ON i.id=r.invitation_id WHERE r.user_id=$1 AND r.state='redeemed' AND i.revoked_at IS NULL`, user)
	if e != nil {
		return nil, e
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Access describes what the server will let a user do. Clients receive it
// for display only and never send it back.
type Access struct {
	Admin        bool     `json:"admin"`
	Tiers        []string `json:"tiers"`
	Source       string   `json:"source"`
	Subscription *Sub     `json:"subscription"`
}
type Sub struct {
	Tier              string     `json:"tier"`
	Status            string     `json:"status"`
	CurrentPeriodEnd  *time.Time `json:"currentPeriodEnd"`
	CancelAtPeriodEnd bool       `json:"cancelAtPeriodEnd"`
}

func IsAdmin(ctx context.Context, db DB, user string) (bool, error) {
	var ok bool
	e := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mea.user_roles r JOIN mea.users u ON u.id=r.user_id WHERE r.user_id=$1 AND r.role='admin' AND r.revoked_at IS NULL AND NOT u.disabled AND u.verified)`, user).Scan(&ok)
	return ok, e
}
func AccessFor(ctx context.Context, db DB, user string) (Access, error) {
	a := Access{Tiers: []string{}}
	var e error
	if a.Admin, e = IsAdmin(ctx, db, user); e != nil {
		return a, e
	}
	rows, e := db.Query(ctx, `SELECT tier,source FROM mea.entitlements WHERE user_id=$1 AND status='active' ORDER BY source,tier`, user)
	if e != nil {
		return a, e
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var tier, source string
		if e = rows.Scan(&tier, &source); e != nil {
			return a, e
		}
		// admin_grant only counts while the administrator role is held.
		if source == "admin_grant" && !a.Admin {
			continue
		}
		if a.Source == "" || source == "admin_grant" {
			a.Source = source
		}
		if !seen[tier] {
			seen[tier] = true
			a.Tiers = append(a.Tiers, tier)
		}
	}
	if e = rows.Err(); e != nil {
		return a, e
	}
	var s Sub
	e = db.QueryRow(ctx, `SELECT tier,status,current_period_end,cancel_at_period_end FROM mea.subscriptions WHERE user_id=$1 ORDER BY updated_at DESC LIMIT 1`, user).Scan(&s.Tier, &s.Status, &s.CurrentPeriodEnd, &s.CancelAtPeriodEnd)
	if e == nil {
		a.Subscription = &s
	} else if !errors.Is(e, pgx.ErrNoRows) {
		return a, e
	}
	return a, nil
}
func (a Access) Has(tier string) bool {
	for _, t := range a.Tiers {
		if t == tier {
			return true
		}
	}
	return false
}

// GrantAdmin is the single controlled path for administrator promotion. It
// is idempotent and grants MEA admin, Guacamole ADMINISTER and admin_grant
// entitlements for both paid tiers in one transaction.
func GrantAdmin(ctx context.Context, db DB, actor, user, expectEmail, reason string) (bool, error) {
	if strings.TrimSpace(reason) == "" {
		return false, errors.New("a reason is required")
	}
	tx, e := db.Begin(ctx)
	if e != nil {
		return false, e
	}
	defer tx.Rollback(ctx)
	var email string
	var verified, disabled bool
	var entity int
	e = tx.QueryRow(ctx, `SELECT email,verified,disabled,entity_id FROM mea.users WHERE id=$1 FOR UPDATE`, user).Scan(&email, &verified, &disabled, &entity)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, errors.New("account not found")
	}
	if e != nil {
		return false, e
	}
	if expectEmail != "" && !strings.EqualFold(email, expectEmail) {
		return false, fmt.Errorf("identity mismatch: account email does not match the expected email")
	}
	if !verified || disabled {
		return false, errors.New("identity mismatch: account is not verified or is disabled")
	}
	var a any
	if actor != "" {
		a = actor
	}
	tag, e := tx.Exec(ctx, `INSERT INTO mea.user_roles(id,user_id,role,reason,granted_by) VALUES($1,$2,'admin',$3,$4) ON CONFLICT (user_id,role) WHERE revoked_at IS NULL DO NOTHING`, security.ID(), user, reason, a)
	if e != nil {
		return false, e
	}
	changed := tag.RowsAffected() == 1
	if _, e = tx.Exec(ctx, `INSERT INTO guacamole_system_permission(entity_id,permission) VALUES($1,'ADMINISTER') ON CONFLICT DO NOTHING`, entity); e != nil {
		return false, e
	}
	for _, t := range Tiers {
		if _, e = tx.Exec(ctx, `INSERT INTO mea.entitlements(id,user_id,tier,source,status,reason,granted_by) VALUES($1,$2,$3,'admin_grant','active',$4,$5)
 ON CONFLICT (user_id,source,tier) WHERE status<>'revoked' DO UPDATE SET status='active',updated_at=now()`, security.ID(), user, t, reason, a); e != nil {
			return false, e
		}
	}
	if e = Audit(ctx, tx, actor, "admin.grant", user, map[string]any{"reason": reason, "changed": changed}); e != nil {
		return false, e
	}
	return changed, tx.Commit(ctx)
}

func RevokeAdmin(ctx context.Context, db DB, actor, user, reason string) error {
	if actor == user {
		return errors.New("administrators cannot revoke their own role")
	}
	tx, e := db.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var remaining int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM mea.user_roles WHERE role='admin' AND revoked_at IS NULL AND user_id<>$1`, user).Scan(&remaining); e != nil {
		return e
	}
	if remaining == 0 {
		return errors.New("the last administrator cannot be revoked")
	}
	if _, e = tx.Exec(ctx, `UPDATE mea.user_roles SET revoked_at=now(),revoked_by=$2 WHERE user_id=$1 AND role='admin' AND revoked_at IS NULL`, user, actor); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `DELETE FROM guacamole_system_permission WHERE permission='ADMINISTER' AND entity_id=(SELECT entity_id FROM mea.users WHERE id=$1)`, user); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `UPDATE mea.entitlements SET status='revoked',updated_at=now() WHERE user_id=$1 AND source='admin_grant' AND status<>'revoked'`, user); e != nil {
		return e
	}
	if e = Audit(ctx, tx, actor, "admin.revoke", user, map[string]any{"reason": reason}); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// Subscription is Stripe's authoritative subscription state, fetched by the
// billing function from the Stripe API (never taken from the browser).
type Subscription struct {
	ID, Customer, Price, Status, InvoiceStatus string
	UserID, Tier                               string
	CurrentPeriodEnd                           int64
	CancelAtPeriodEnd                          bool
	Updated                                    int64
}

// Entitled reports whether a Stripe status grants service. past_due keeps
// access only while Stripe retries the first failure; unpaid, incomplete and
// canceled never grant access.
func Entitled(status, invoice string) bool {
	return status == "active" || status == "trialing"
}

// ApplySubscription upserts the subscription and reconciles only the
// 'stripe' entitlements. Older Stripe snapshots never overwrite newer ones.
func ApplySubscription(ctx context.Context, tx pgx.Tx, s Subscription) error {
	if !ValidTier(s.Tier) {
		return fmt.Errorf("subscription %s has no MEA tier", s.ID)
	}
	var end any
	if s.CurrentPeriodEnd > 0 {
		end = time.Unix(s.CurrentPeriodEnd, 0)
	}
	tag, e := tx.Exec(ctx, `INSERT INTO mea.subscriptions(id,user_id,customer_id,tier,price_id,status,current_period_end,cancel_at_period_end,latest_invoice_status,stripe_updated)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10)
 ON CONFLICT(id) DO UPDATE SET tier=EXCLUDED.tier,price_id=EXCLUDED.price_id,status=EXCLUDED.status,current_period_end=EXCLUDED.current_period_end,
 cancel_at_period_end=EXCLUDED.cancel_at_period_end,latest_invoice_status=COALESCE(EXCLUDED.latest_invoice_status,mea.subscriptions.latest_invoice_status),
 stripe_updated=EXCLUDED.stripe_updated,updated_at=now() WHERE mea.subscriptions.stripe_updated<=EXCLUDED.stripe_updated AND mea.subscriptions.user_id=EXCLUDED.user_id`,
		s.ID, s.UserID, s.Customer, s.Tier, s.Price, s.Status, end, s.CancelAtPeriodEnd, s.InvoiceStatus, s.Updated)
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	status := "suspended"
	if Entitled(s.Status, s.InvoiceStatus) {
		status = "active"
	} else if s.Status == "canceled" || s.Status == "incomplete_expired" {
		status = "revoked"
	}
	// A plan change retires the other tier's Stripe entitlement for this subscription.
	if _, e = tx.Exec(ctx, `UPDATE mea.entitlements SET status='revoked',updated_at=now() WHERE subscription_id=$1 AND tier<>$2 AND status<>'revoked'`, s.ID, s.Tier); e != nil {
		return e
	}
	if status == "revoked" {
		_, e = tx.Exec(ctx, `UPDATE mea.entitlements SET status='revoked',updated_at=now() WHERE subscription_id=$1 AND status<>'revoked'`, s.ID)
		return e
	}
	tag, e = tx.Exec(ctx, `UPDATE mea.entitlements SET status=$3,updated_at=now() WHERE subscription_id=$1 AND tier=$2 AND status<>'revoked'`, s.ID, s.Tier, status)
	if e != nil || tag.RowsAffected() > 0 {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO mea.entitlements(id,user_id,tier,source,subscription_id,status,reason) VALUES($1,$2,$3,'stripe',$4,$5,'Stripe subscription')
 ON CONFLICT (user_id,source,tier) WHERE status<>'revoked' DO UPDATE SET subscription_id=EXCLUDED.subscription_id,status=EXCLUDED.status,updated_at=now()`, security.ID(), s.UserID, s.Tier, s.ID, status)
	return e
}

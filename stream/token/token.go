// Package token implements MEA Stream single-session authorization tokens
// (docs/PROTOCOL.md §2.1). Tokens are Ed25519-signed so that only the MEA API,
// which holds the private key, can mint them; gateways and agents verify with
// the public key.
package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	Version     = 1
	MaxLifetime = 120 * time.Second
	ClockSkew   = 5 * time.Second
)

var (
	ErrMalformed   = errors.New("token: malformed")
	ErrSignature   = errors.New("token: bad signature")
	ErrExpired     = errors.New("token: expired")
	ErrNotYetValid = errors.New("token: not yet valid")
	ErrReplayed    = errors.New("token: already used")
	ErrWrongHost   = errors.New("token: issued for another host")
	ErrWrongDesk   = errors.New("token: issued for another desktop")
	ErrEntitlement = errors.New("token: entitlement does not allow streaming")
	ErrLifetime    = errors.New("token: lifetime too long")
)

// Claims is the token payload.
type Claims struct {
	V           int    `json:"v"`
	ID          string `json:"jti"`
	User        string `json:"sub"`
	Desktop     string `json:"dsk"`
	Session     string `json:"sid"`
	Host        string `json:"host"`
	Entitlement string `json:"ent"`
	Profile     string `json:"prof"`
	IssuedAt    int64  `json:"iat"`
	ExpiresAt   int64  `json:"exp"`
}

// Entitled reports whether an entitlement may open a gaming stream. Power and
// Newbie users never get GPU streams.
func Entitled(ent string) bool { return ent == "eleet" || ent == "admin" }

// NewID returns a random 128-bit identifier in hex.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

var enc = base64.RawURLEncoding

// Mint signs claims. Missing ID, IssuedAt and Version are filled in; ExpiresAt
// defaults to IssuedAt+60s.
func Mint(key ed25519.PrivateKey, c Claims, now time.Time) (string, Claims, error) {
	if c.V == 0 {
		c.V = Version
	}
	if c.ID == "" {
		c.ID = NewID()
	}
	if c.IssuedAt == 0 {
		c.IssuedAt = now.Unix()
	}
	if c.ExpiresAt == 0 {
		c.ExpiresAt = c.IssuedAt + 60
	}
	if time.Duration(c.ExpiresAt-c.IssuedAt)*time.Second > MaxLifetime {
		return "", c, ErrLifetime
	}
	if !Entitled(c.Entitlement) {
		return "", c, ErrEntitlement
	}
	if c.User == "" || c.Desktop == "" || c.Session == "" || c.Host == "" {
		return "", c, ErrMalformed
	}
	p, err := json.Marshal(c)
	if err != nil {
		return "", c, err
	}
	sig := ed25519.Sign(key, p)
	return enc.EncodeToString(p) + "." + enc.EncodeToString(sig), c, nil
}

// Parse checks the signature, version, lifetime and entitlement, but not
// single use or host/desktop binding; use a Verifier for that.
func Parse(pub ed25519.PublicKey, tok string, now time.Time) (Claims, error) {
	var c Claims
	p64, s64, ok := strings.Cut(tok, ".")
	if !ok || len(tok) > 4096 {
		return c, ErrMalformed
	}
	p, err := enc.DecodeString(p64)
	if err != nil {
		return c, ErrMalformed
	}
	sig, err := enc.DecodeString(s64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return c, ErrMalformed
	}
	if !ed25519.Verify(pub, p, sig) {
		return c, ErrSignature
	}
	if err := json.Unmarshal(p, &c); err != nil || c.V != Version || c.ID == "" {
		return c, ErrMalformed
	}
	if time.Duration(c.ExpiresAt-c.IssuedAt)*time.Second > MaxLifetime {
		return c, ErrLifetime
	}
	if now.Add(ClockSkew).Unix() < c.IssuedAt {
		return c, ErrNotYetValid
	}
	if now.Add(-ClockSkew).Unix() >= c.ExpiresAt {
		return c, ErrExpired
	}
	if !Entitled(c.Entitlement) {
		return c, ErrEntitlement
	}
	return c, nil
}

// Verifier enforces single use and binding to one host (gateway) or one
// desktop (agent).
type Verifier struct {
	Public  ed25519.PublicKey
	Host    string // if set, claims.Host must match
	Desktop string // if set, claims.Desktop must match
	Now     func() time.Time

	mu   sync.Mutex
	used map[string]int64 // jti -> exp
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// Verify parses tok, checks bindings and consumes its jti.
func (v *Verifier) Verify(tok string) (Claims, error) {
	now := v.now()
	c, err := Parse(v.Public, tok, now)
	if err != nil {
		return c, err
	}
	if v.Host != "" && c.Host != v.Host {
		return c, ErrWrongHost
	}
	if v.Desktop != "" && c.Desktop != v.Desktop {
		return c, ErrWrongDesk
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.used == nil {
		v.used = map[string]int64{}
	}
	cut := now.Add(-ClockSkew).Unix()
	for id, exp := range v.used {
		if exp < cut {
			delete(v.used, id)
		}
	}
	if _, dup := v.used[c.ID]; dup {
		return c, ErrReplayed
	}
	v.used[c.ID] = c.ExpiresAt
	return c, nil
}

// ParseKeys decodes base64 (std or url) Ed25519 keys as stored in config.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := decodeAny(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("token: bad public key")
	}
	return ed25519.PublicKey(b), nil
}

// ParsePrivateKey accepts a 32-byte seed or a 64-byte private key.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := decodeAny(s)
	if err != nil {
		return nil, errors.New("token: bad private key")
	}
	switch len(b) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	}
	return nil, errors.New("token: bad private key")
}

func decodeAny(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, e := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := e.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, ErrMalformed
}

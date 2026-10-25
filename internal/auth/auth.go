// Package auth authenticates API callers with bearer API keys and enforces
// role boundaries. Only SHA-256 hashes of keys are configured or stored.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// Role is a permission level. Higher roles include lower ones.
type Role int

const (
	RoleNone Role = iota
	// RoleViewer can read hosts, containers, images, operations and metrics.
	RoleViewer
	// RoleOperator can additionally manage container lifecycles.
	RoleOperator
	// RoleAdmin can additionally register/unregister hosts, manage images,
	// force-stop or force-delete containers and run reconciliation.
	RoleAdmin
)

func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleOperator:
		return "operator"
	case RoleAdmin:
		return "admin"
	default:
		return "none"
	}
}

// ParseRole parses a role name.
func ParseRole(s string) (Role, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "viewer":
		return RoleViewer, nil
	case "operator":
		return RoleOperator, nil
	case "admin":
		return RoleAdmin, nil
	}
	return RoleNone, fmt.Errorf("unknown role %q (want viewer, operator or admin)", s)
}

// Principal is an authenticated caller.
type Principal struct {
	Name string
	Role Role
}

// Key is a configured API key (hash only).
type Key struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	SHA256 string `json:"sha256"`
}

type keyEntry struct {
	name string
	role Role
	hash [32]byte
}

// Authenticator checks API keys.
type Authenticator struct {
	keys []keyEntry
}

var keyNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// New builds an Authenticator from keys.
func New(keys []Key) (*Authenticator, error) {
	a := &Authenticator{}
	seen := map[string]bool{}
	for _, k := range keys {
		if !keyNameRe.MatchString(k.Name) {
			return nil, fmt.Errorf("api key name %q is invalid", k.Name)
		}
		if seen[k.Name] {
			return nil, fmt.Errorf("api key name %q is duplicated", k.Name)
		}
		seen[k.Name] = true
		role, err := ParseRole(k.Role)
		if err != nil {
			return nil, fmt.Errorf("api key %q: %w", k.Name, err)
		}
		raw, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(k.SHA256)))
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("api key %q: sha256 must be 64 hex characters (the hash, never the key itself)", k.Name)
		}
		var h [32]byte
		copy(h[:], raw)
		a.keys = append(a.keys, keyEntry{name: k.Name, role: role, hash: h})
	}
	return a, nil
}

// ParseKeySpecs parses "name:role:sha256hex" entries.
func ParseKeySpecs(specs []string) ([]Key, error) {
	var out []Key
	for _, s := range specs {
		parts := strings.Split(s, ":")
		if len(parts) != 3 {
			return nil, errors.New("TYPE3_API_KEYS entries must be name:role:sha256hex")
		}
		out = append(out, Key{Name: parts[0], Role: parts[1], SHA256: parts[2]})
	}
	return out, nil
}

// LoadKeyFile reads a JSON array of Key from path.
func LoadKeyFile(path string) ([]Key, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read api keys file: %w", err)
	}
	var keys []Key
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, fmt.Errorf("parse api keys file %s: %w", path, err)
	}
	return keys, nil
}

// Count returns the number of configured keys.
func (a *Authenticator) Count() int { return len(a.keys) }

// Authenticate checks a raw key. Every configured key is compared in
// constant time so timing does not reveal which (if any) matched.
func (a *Authenticator) Authenticate(raw string) (Principal, bool) {
	if raw == "" {
		return Principal{}, false
	}
	sum := sha256.Sum256([]byte(raw))
	var found Principal
	ok := false
	for _, k := range a.keys {
		if subtle.ConstantTimeCompare(sum[:], k.hash[:]) == 1 {
			found = Principal{Name: k.name, Role: k.role}
			ok = true
		}
	}
	return found, ok
}

// TokenFromRequest extracts the bearer token (or X-API-Key header).
func TokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, tok, ok := strings.Cut(h, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(tok)
		}
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

// HashKey returns the hex SHA-256 of a raw key.
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// GenerateKey returns a new random API key.
func GenerateKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "t3_" + base64.RawURLEncoding.EncodeToString(b), nil
}

type ctxKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal in ctx.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

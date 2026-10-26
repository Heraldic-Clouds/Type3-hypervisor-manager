// Package streamtoken mints MEA Stream session tokens (stream/docs/PROTOCOL.md
// §2.1). It mirrors stream/token so the API image does not depend on the
// stream module; TestGoldenVector in both packages pins the wire format.
package streamtoken

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

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

const Lifetime = 60 * time.Second

func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Mint signs c. ID, IssuedAt and ExpiresAt are filled in when empty.
func Mint(key ed25519.PrivateKey, c Claims, now time.Time) (string, error) {
	if c.Entitlement != "eleet" && c.Entitlement != "admin" {
		return "", errors.New("streamtoken: entitlement does not allow streaming")
	}
	if c.User == "" || c.Desktop == "" || c.Session == "" || c.Host == "" {
		return "", errors.New("streamtoken: missing claims")
	}
	c.V = 1
	if c.ID == "" {
		c.ID = NewID()
	}
	if c.IssuedAt == 0 {
		c.IssuedAt = now.Unix()
	}
	if c.ExpiresAt == 0 {
		c.ExpiresAt = c.IssuedAt + int64(Lifetime/time.Second)
	}
	p, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(p) + "." + enc.EncodeToString(ed25519.Sign(key, p)), nil
}

// ParsePrivateKey accepts a base64 32-byte seed or 64-byte key.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	s = strings.TrimSpace(s)
	for _, e := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		b, err := e.DecodeString(s)
		if err != nil {
			continue
		}
		switch len(b) {
		case ed25519.SeedSize:
			return ed25519.NewKeyFromSeed(b), nil
		case ed25519.PrivateKeySize:
			return ed25519.PrivateKey(b), nil
		}
	}
	return nil, errors.New("streamtoken: bad private key")
}

// ICEServer is the RTCIceServer shape the browser expects.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// TURNCredentials implements the coturn "TURN REST API" scheme
// (use-auth-secret): username = expiry:user, password = b64(hmac-sha1).
func TURNCredentials(secret, user string, ttl time.Duration, now time.Time) (string, string) {
	u := strconv.FormatInt(now.Add(ttl).Unix(), 10) + ":" + user
	m := hmac.New(sha1.New, []byte(secret))
	m.Write([]byte(u))
	return u, base64.StdEncoding.EncodeToString(m.Sum(nil))
}

package token

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"
)

func keys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func claims() Claims {
	return Claims{User: "u1", Desktop: "d1", Session: "s1", Host: "h1", Entitlement: "eleet", Profile: "balanced"}
}

func TestMintVerify(t *testing.T) {
	pub, priv := keys(t)
	now := time.Unix(1_800_000_000, 0)
	tok, c, err := Mint(priv, claims(), now)
	if err != nil {
		t.Fatal(err)
	}
	v := &Verifier{Public: pub, Host: "h1", Now: func() time.Time { return now.Add(10 * time.Second) }}
	got, err := v.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got != c {
		t.Fatalf("claims mismatch: %+v vs %+v", got, c)
	}
	if _, err := v.Verify(tok); !errors.Is(err, ErrReplayed) {
		t.Fatalf("replay: %v", err)
	}
}

func TestRejections(t *testing.T) {
	pub, priv := keys(t)
	otherPub, _ := keys(t)
	now := time.Unix(1_800_000_000, 0)
	tok, _, _ := Mint(priv, claims(), now)

	cases := []struct {
		name string
		v    *Verifier
		tok  string
		want error
	}{
		{"expired", &Verifier{Public: pub, Now: func() time.Time { return now.Add(70 * time.Second) }}, tok, ErrExpired},
		{"future", &Verifier{Public: pub, Now: func() time.Time { return now.Add(-time.Minute) }}, tok, ErrNotYetValid},
		{"wrong key", &Verifier{Public: otherPub, Now: func() time.Time { return now }}, tok, ErrSignature},
		{"wrong host", &Verifier{Public: pub, Host: "h2", Now: func() time.Time { return now }}, tok, ErrWrongHost},
		{"wrong desktop", &Verifier{Public: pub, Desktop: "d2", Now: func() time.Time { return now }}, tok, ErrWrongDesk},
		{"garbage", &Verifier{Public: pub, Now: func() time.Time { return now }}, "abc", ErrMalformed},
		{"tampered", &Verifier{Public: pub, Now: func() time.Time { return now }}, tamper(tok), nil},
	}
	for _, tc := range cases {
		_, err := tc.v.Verify(tc.tok)
		if tc.want == nil {
			if err == nil {
				t.Errorf("%s: accepted", tc.name)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v want %v", tc.name, err, tc.want)
		}
	}
}

// tamper flips one payload character so the signature no longer matches.
func tamper(tok string) string {
	b := []byte(tok)
	if b[10] == 'A' {
		b[10] = 'B'
	} else {
		b[10] = 'A'
	}
	return string(b)
}

func TestMintPolicy(t *testing.T) {
	_, priv := keys(t)
	now := time.Now()
	c := claims()
	c.Entitlement = "power"
	if _, _, err := Mint(priv, c, now); !errors.Is(err, ErrEntitlement) {
		t.Fatalf("power user got a token: %v", err)
	}
	c = claims()
	c.ExpiresAt = now.Unix() + 3600
	c.IssuedAt = now.Unix()
	if _, _, err := Mint(priv, c, now); !errors.Is(err, ErrLifetime) {
		t.Fatalf("long token: %v", err)
	}
	c = claims()
	c.Desktop = ""
	if _, _, err := Mint(priv, c, now); !errors.Is(err, ErrMalformed) {
		t.Fatalf("missing desktop: %v", err)
	}
}

func TestKeyParsing(t *testing.T) {
	if _, err := ParsePrivateKey(strings.Repeat("A", 43)); err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePublicKey("nope"); err == nil {
		t.Fatal("bad key accepted")
	}
}

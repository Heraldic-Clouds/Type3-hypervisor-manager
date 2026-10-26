package streamtoken

import (
	"crypto/ed25519"
	"testing"
	"time"
)

// GoldenToken is shared with stream/token's TestGoldenVector: both packages
// must produce and accept exactly this token.
const GoldenToken = "eyJ2IjoxLCJqdGkiOiIwMDExMjIzMzQ0NTU2Njc3ODg5OWFhYmJjY2RkZWVmZiIsInN1YiI6InVzZXItMSIsImRzayI6ImRlc2stMSIsInNpZCI6InNlc3MtMSIsImhvc3QiOiJob3N0LTEiLCJlbnQiOiJlbGVldCIsInByb2YiOiJiYWxhbmNlZCIsImlhdCI6MTgwMDAwMDAwMCwiZXhwIjoxODAwMDAwMDYwfQ.CiUvw17ZhtseAxGLr_oV9WbnARdZdtTYEwdOgbuzoTvwNn8xRYKOrfbYCK4rlKkHPmcK9Bd2HNvqbGT-R_DOBg"

func goldenKey() ed25519.PrivateKey {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func TestGoldenVector(t *testing.T) {
	tok, err := Mint(goldenKey(), Claims{
		ID: "00112233445566778899aabbccddeeff", User: "user-1", Desktop: "desk-1", Session: "sess-1",
		Host: "host-1", Entitlement: "eleet", Profile: "balanced", IssuedAt: 1_800_000_000, ExpiresAt: 1_800_000_060,
	}, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if tok != GoldenToken {
		t.Fatalf("wire format changed:\n%s", tok)
	}
}

func TestPolicy(t *testing.T) {
	if _, err := Mint(goldenKey(), Claims{User: "u", Desktop: "d", Session: "s", Host: "h", Entitlement: "power"}, time.Now()); err == nil {
		t.Fatal("power user received a GPU stream token")
	}
	u, p := TURNCredentials("secret", "alice", time.Minute, time.Unix(1000, 0))
	if u != "1060:alice" || p != "hjRtn0ot3SN3N/ZTliyVxIvRyZs=" {
		t.Fatal(u, p)
	}
}

package auth

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthenticateAndRoles(t *testing.T) {
	key, err := GenerateKey()
	if err != nil || !strings.HasPrefix(key, "t3_") {
		t.Fatalf("GenerateKey = %q %v", key, err)
	}
	a, err := New([]Key{{Name: "ci", Role: "operator", SHA256: HashKey(key)}})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := a.Authenticate(key)
	if !ok || p.Name != "ci" || p.Role != RoleOperator {
		t.Fatalf("Authenticate = %+v %v", p, ok)
	}
	if _, ok := a.Authenticate(key + "x"); ok {
		t.Fatal("wrong key accepted")
	}
	if _, ok := a.Authenticate(""); ok {
		t.Fatal("empty key accepted")
	}
	if !(RoleAdmin > RoleOperator && RoleOperator > RoleViewer) {
		t.Fatal("role ordering")
	}
}

func TestNewRejectsBadKeys(t *testing.T) {
	for _, k := range []Key{
		{Name: "a", Role: "root", SHA256: HashKey("x")},
		{Name: "a", Role: "admin", SHA256: "t3_plaintextkey"},
		{Name: "bad name", Role: "admin", SHA256: HashKey("x")},
	} {
		if _, err := New([]Key{k}); err == nil {
			t.Errorf("accepted %+v", k)
		}
	}
	if _, err := New([]Key{{Name: "a", Role: "admin", SHA256: HashKey("x")}, {Name: "a", Role: "viewer", SHA256: HashKey("y")}}); err == nil {
		t.Error("duplicate names accepted")
	}
}

func TestParseKeySpecsAndToken(t *testing.T) {
	keys, err := ParseKeySpecs([]string{"admin:admin:" + HashKey("k")})
	if err != nil || len(keys) != 1 || keys[0].Role != "admin" {
		t.Fatalf("ParseKeySpecs = %v %v", keys, err)
	}
	if _, err := ParseKeySpecs([]string{"admin:admin"}); err == nil {
		t.Fatal("short spec accepted")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer abc")
	if TokenFromRequest(r) != "abc" {
		t.Fatal("bearer token not read")
	}
	r.Header.Set("Authorization", "Basic abc")
	if TokenFromRequest(r) != "" {
		t.Fatal("basic auth must not be accepted as a token")
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", "xyz")
	if TokenFromRequest(r) != "xyz" {
		t.Fatal("X-API-Key not read")
	}
}

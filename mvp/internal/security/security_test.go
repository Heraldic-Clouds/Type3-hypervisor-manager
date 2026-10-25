package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestArgon2(t *testing.T) {
	h, e := Password("a-realistic-test-password")
	if e != nil {
		t.Fatal(e)
	}
	if !Verify(h, "a-realistic-test-password") || Verify(h, "wrong-password") {
		t.Fatal("password verification mismatch")
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatal("unsafe parameters")
	}
	if Verify("$argon2id$v=19$m=999999999,t=99,p=99$a$b", "x") {
		t.Fatal("accepted attacker-controlled cost")
	}
	if _, e = Password("short"); e == nil {
		t.Fatal("accepted short password")
	}
}
func TestGuacamoleWireContract(t *testing.T) {
	now := time.Unix(1700000000, 0)
	key := make([]byte, 16)
	token, e := Guacamole(strings.Repeat("00", 16), "alice", "unique", now)
	if e != nil {
		t.Fatal(e)
	}
	data, _ := base64.StdEncoding.DecodeString(token)
	block, _ := aes.NewCipher(key)
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, make([]byte, 16)).CryptBlocks(plain, data)
	plain = plain[:len(plain)-int(plain[len(plain)-1])]
	mac := hmac.New(sha256.New, key)
	mac.Write(plain[32:])
	if !hmac.Equal(mac.Sum(nil), plain[:32]) {
		t.Fatal("invalid HMAC")
	}
	var payload map[string]any
	if e = json.Unmarshal(plain[32:], &payload); e != nil {
		t.Fatal(e)
	}
	if payload["username"] != "alice" || payload["singleUse"] != true || int64(payload["expires"].(float64)) != now.Add(30*time.Second).UnixMilli() {
		t.Fatal("incorrect token scope")
	}
	if len(payload["connections"].(map[string]any)) != 0 {
		t.Fatal("bypassed JDBC connection ownership")
	}
	if _, ok := payload["nonce"]; ok {
		t.Fatal("unknown properties break strict upstream JSON parser")
	}
}

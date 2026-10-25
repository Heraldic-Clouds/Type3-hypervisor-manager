package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

func Random(n int) string {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func ID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func Hash(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func Password(s string) (string, error) {
	if len(s) < 12 || len(s) > 256 {
		return "", errors.New("Use a password between 12 and 256 characters")
	}
	salt := make([]byte, 16)
	if _, e := rand.Read(salt); e != nil {
		return "", e
	}
	key := argon2.IDKey([]byte(s), salt, 3, 64*1024, 2, 32)
	return "$argon2id$v=19$m=65536,t=3,p=2$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}
func Verify(encoded, password string) bool {
	p := strings.Split(encoded, "$")
	if len(p) != 6 || p[1] != "argon2id" || p[2] != "v=19" || p[3] != "m=65536,t=3,p=2" || len(password) > 256 {
		return false
	}
	salt, e := base64.RawStdEncoding.DecodeString(p[4])
	if e != nil || len(salt) != 16 {
		return false
	}
	want, e := base64.RawStdEncoding.DecodeString(p[5])
	if e != nil || len(want) != 32 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return subtle.ConstantTimeCompare(want, got) == 1
}

// Guacamole's documented wire format: HMAC-SHA256 || JSON, AES-128-CBC,
// PKCS#7 padding, zero IV. JDBC supplies connections; no RDP secrets in JSON.
func Guacamole(keyHex, username, nonce string, now time.Time) (string, error) {
	key, e := hex.DecodeString(keyHex)
	if e != nil || len(key) != 16 {
		return "", errors.New("invalid Guacamole bridge key")
	}
	b, e := json.Marshal(map[string]any{"username": username, "expires": now.Add(30 * time.Second).UnixMilli(), "singleUse": true, "connections": map[string]any{}})
	if e != nil {
		return "", e
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(b)
	plain := append(mac.Sum(nil), b...)
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	for i := 0; i < pad; i++ {
		plain = append(plain, byte(pad))
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return "", e
	}
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(out, plain)
	return base64.StdEncoding.EncodeToString(out), nil
}

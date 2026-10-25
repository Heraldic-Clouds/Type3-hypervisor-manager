package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedaction(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "debug", "json")
	log.Info("x",
		"SecretKey", "AKIAEXAMPLE", "aws_session_token", "tok", "client_key", "k",
		"pem", "-----BEGIN EC PRIVATE KEY-----\nabc", "header", "Bearer t3_abc", "host", "node1")
	out := buf.String()
	for _, leak := range []string{"AKIAEXAMPLE", "tok\"", "PRIVATE KEY", "t3_abc"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaked %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, `"host":"node1"`) {
		t.Errorf("non-secret attribute was redacted: %s", out)
	}
}

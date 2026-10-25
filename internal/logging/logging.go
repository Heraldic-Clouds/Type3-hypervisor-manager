// Package logging builds the structured slog logger used across Type3. A
// ReplaceAttr hook redacts attributes whose keys look like secrets and any
// value that looks like PEM key material, so a careless log call cannot leak
// credentials.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

const redacted = "[REDACTED]"

var secretKeyFragments = []string{
	"secret", "password", "passwd", "token", "apikey", "api_key", "authorization",
	"private", "client_key", "tls_key", "credential", "cookie",
}

// New returns a logger writing to w in the given format ("json" or "text").
func New(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: ParseLevel(level), ReplaceAttr: Redact}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

// ParseLevel maps a level name to a slog level, defaulting to info.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Redact is a slog ReplaceAttr function that masks secret-looking attributes.
func Redact(_ []string, a slog.Attr) slog.Attr {
	if IsSecretKey(a.Key) {
		return slog.String(a.Key, redacted)
	}
	if a.Value.Kind() == slog.KindString && LooksLikeSecretValue(a.Value.String()) {
		return slog.String(a.Key, redacted)
	}
	return a
}

// IsSecretKey reports whether an attribute key names secret material.
func IsSecretKey(key string) bool {
	k := strings.ToLower(key)
	for _, frag := range secretKeyFragments {
		if strings.Contains(k, frag) {
			return true
		}
	}
	return false
}

// LooksLikeSecretValue catches PEM private keys and bearer tokens.
func LooksLikeSecretValue(v string) bool {
	return strings.Contains(v, "PRIVATE KEY-----") || strings.HasPrefix(strings.ToLower(v), "bearer ")
}

// Discard returns a logger that drops everything, for tests.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

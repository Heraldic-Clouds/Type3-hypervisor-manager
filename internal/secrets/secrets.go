// Package secrets resolves secret references (certificates, keys) into their
// contents in memory. Supported forms:
//
//	/abs/or/relative/path   file on disk
//	file:///path            file on disk
//	env://VAR_NAME          contents of an environment variable
//	s3://bucket/object/key  S3 object, fetched with AWS SDK v2 default credentials
//
// Resolved values are never written to disk and never logged. The s3 form
// keeps the original prototype's "certificates live in S3" behaviour without
// static credentials or /tmp files.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxSecretBytes bounds the size of a resolved secret.
const maxSecretBytes = 1 << 20

// ObjectFetcher fetches an object from an object store.
type ObjectFetcher interface {
	Fetch(ctx context.Context, bucket, key string) ([]byte, error)
}

// Resolver turns references into secret contents.
type Resolver struct {
	// S3 is used for s3:// references. Nil disables them.
	S3 ObjectFetcher
}

// Resolve returns the contents a reference points at. An empty reference
// resolves to an empty string.
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		return "", nil
	case strings.HasPrefix(ref, "env://"):
		name := strings.TrimPrefix(ref, "env://")
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("secret reference env://%s: variable not set", name)
		}
		return v, nil
	case strings.HasPrefix(ref, "s3://"):
		if r.S3 == nil {
			return "", errors.New("secret reference uses s3:// but S3 access is not configured")
		}
		bucket, key, ok := strings.Cut(strings.TrimPrefix(ref, "s3://"), "/")
		if !ok || bucket == "" || key == "" {
			return "", fmt.Errorf("secret reference %q: want s3://bucket/key", ref)
		}
		b, err := r.S3.Fetch(ctx, bucket, key)
		if err != nil {
			return "", fmt.Errorf("fetch s3://%s/%s: %w", bucket, key, err)
		}
		return string(b), nil
	case strings.HasPrefix(ref, "file://"):
		return readFile(strings.TrimPrefix(ref, "file://"))
	case strings.Contains(ref, "://"):
		return "", fmt.Errorf("secret reference %q: unsupported scheme", Describe(ref))
	default:
		return readFile(ref)
	}
}

func readFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("read secret file %s: %w", path, err)
	}
	if len(b) > maxSecretBytes {
		return "", fmt.Errorf("secret file %s exceeds %d bytes", path, maxSecretBytes)
	}
	return string(b), nil
}

// Describe returns a log-safe description of a reference (its scheme and
// location, never contents).
func Describe(ref string) string {
	if ref == "" {
		return ""
	}
	if i := strings.Index(ref, "://"); i >= 0 {
		return ref[:i+3] + "…"
	}
	return "file"
}

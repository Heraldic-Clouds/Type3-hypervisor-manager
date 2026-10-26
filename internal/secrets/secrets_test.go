package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fakeS3 struct{ bucket, key string }

func (f *fakeS3) Fetch(_ context.Context, bucket, key string) ([]byte, error) {
	f.bucket, f.key = bucket, key
	if key == "missing" {
		return nil, errors.New("NoSuchKey")
	}
	return []byte("from-s3"), nil
}

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.pem")
	if err := os.WriteFile(p, []byte("file-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPE3_TEST_SECRET", "env-data")
	s3 := &fakeS3{}
	r := &Resolver{S3: s3}
	ctx := context.Background()
	for ref, want := range map[string]string{
		"": "", p: "file-data", "file://" + p: "file-data", "env://TYPE3_TEST_SECRET": "env-data",
		"s3://lxd-certs/hosts/client.crt": "from-s3",
	} {
		got, err := r.Resolve(ctx, ref)
		if err != nil || got != want {
			t.Errorf("Resolve(%q) = %q %v, want %q", ref, got, err, want)
		}
	}
	if s3.bucket != "lxd-certs" || s3.key != "hosts/client.crt" {
		t.Errorf("s3 called with %s/%s", s3.bucket, s3.key)
	}
	for _, bad := range []string{"env://NOPE_NOT_SET", "s3://bucket-only", "s3://b/missing", "http://x/y", filepath.Join(dir, "nope")} {
		if _, err := r.Resolve(ctx, bad); err == nil {
			t.Errorf("Resolve(%q) succeeded", bad)
		}
	}
	if _, err := (&Resolver{}).Resolve(ctx, "s3://b/k"); err == nil {
		t.Error("s3 without fetcher succeeded")
	}
}

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsAreSafe(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsLoopback() {
		t.Errorf("default listen %s should be loopback", cfg.ListenAddr)
	}
	if cfg.AllowPublicEndpoints {
		t.Error("public LXD endpoints must be off by default")
	}
	if cfg.OperationTimeout != 15*time.Minute || !cfg.IdmapIsolated || !cfg.NICPortIsolation {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestDotenvAndValidation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	_ = os.WriteFile(p, []byte("# comment\nexport TYPE3_LISTEN_ADDR=\"127.0.0.1:9999\"\nTYPE3_OPERATION_TIMEOUT=2m\n"), 0o600)
	t.Setenv("TYPE3_OPERATION_TIMEOUT", "3m") // env wins over .env
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:9999" || cfg.OperationTimeout != 3*time.Minute {
		t.Fatalf("cfg = %s %s", cfg.ListenAddr, cfg.OperationTimeout)
	}
	os.Unsetenv("TYPE3_LISTEN_ADDR")

	t.Setenv("TYPE3_LXD_CLIENT_CERT", "/x.crt")
	if _, err := Load(""); err == nil {
		t.Fatal("cert without key accepted")
	}
	t.Setenv("TYPE3_LXD_CLIENT_CERT", "")
	t.Setenv("TYPE3_HEALTH_INTERVAL", "soon")
	if _, err := Load(""); err == nil {
		t.Fatal("bad duration accepted")
	}
}

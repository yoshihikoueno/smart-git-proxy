package config

import (
	"os"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadArgs([]string{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Fatalf("listen addr default mismatch: %s", cfg.ListenAddr)
	}
	if cfg.MirrorDir == "" {
		t.Fatalf("mirror dir default empty")
	}
	if cfg.SyncStaleAfter != 2*time.Second {
		t.Fatalf("sync stale after default mismatch: %v", cfg.SyncStaleAfter)
	}
}

func TestStaticAuthRequiresToken(t *testing.T) {
	clearEnv(t)
	_, err := LoadArgs([]string{"-auth-mode=static"})
	if err == nil {
		t.Fatalf("expected error when static token missing")
	}
}

func TestEnvOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("SYNC_STALE_AFTER", "5s")
	cfg, err := LoadArgs([]string{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SyncStaleAfter != 5*time.Second {
		t.Fatalf("expected sync stale after override, got %v", cfg.SyncStaleAfter)
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LISTEN_ADDR", "MIRROR_DIR", "SYNC_STALE_AFTER", "ALLOWED_UPSTREAMS", "LOG_LEVEL",
		"AUTH_MODE", "STATIC_TOKEN",
	} {
		_ = os.Unsetenv(k)
	}
}

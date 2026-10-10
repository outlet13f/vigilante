package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vigilante/internal/store/pgtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Stop()
	os.Exit(code)
}

func TestStoreCommand(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	t.Setenv("VGL_TEST_DSN", dsn)
	cfg := filepath.Join(t.TempDir(), "v.yaml")
	if err := os.WriteFile(cfg, []byte(`version: v1
server:
  state: {backend: postgres, dsn_env: VGL_TEST_DSN, auto_migrate: false}
targets: []
executors: {}
services: []
`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Fresh database: everything is pending (exit 4 tells scripts so).
	if code, err := cmdStore(ctx, []string{"status", "-c", cfg}); err != nil || code != 4 {
		t.Fatalf("status before migrate: %d %v", code, err)
	}
	if code, err := cmdStore(ctx, []string{"migrate", "-c", cfg}); err != nil || code != 0 {
		t.Fatalf("migrate: %d %v", code, err)
	}
	if code, err := cmdStore(ctx, []string{"status", "-c", cfg}); err != nil || code != 0 {
		t.Fatalf("status after migrate: %d %v", code, err)
	}
	// A downgrade needs an explicit confirmation.
	if _, err := cmdStore(ctx, []string{"migrate", "-c", cfg, "--down-to", "1"}); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("down without --yes: %v", err)
	}
	// The file store has no schema.
	fileCfg := filepath.Join(t.TempDir(), "f.yaml")
	_ = os.WriteFile(fileCfg, []byte("version: v1\ntargets: []\nexecutors: {}\nservices: []\n"), 0o600)
	if _, err := cmdStore(ctx, []string{"status", "-c", fileCfg}); err == nil || !strings.Contains(err.Error(), "no schema") {
		t.Fatalf("file store: %v", err)
	}
}

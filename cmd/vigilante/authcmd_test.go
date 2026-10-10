package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vigilante/internal/config"
	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/orchestrator"
)

// With authentication on the API, privileged commands that act on the store
// directly need --break-glass, which is audited.
func TestLocalCLIRestricted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "v.yaml")
	journalPath := filepath.ToSlash(filepath.Join(dir, "journal.jsonl"))
	write := func(localCLI string) {
		t.Helper()
		raw := "version: v1\nserver: {journal_path: " + journalPath + "}\n" +
			"auth:\n  service_accounts: [{name: ci, token_sha256: " + strings.Repeat("ab", 32) + ", roles: [{role: deployer}]}]\n"
		if localCLI != "" {
			raw += "  local_cli: " + localCLI + "\n"
		}
		raw += "targets: []\nexecutors: {}\nservices: []\n"
		if err := os.WriteFile(cfg, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("")
	if _, err := cmdCircuit(ctx, []string{"trip", "-c", cfg}); err == nil || !strings.Contains(err.Error(), "--break-glass") {
		t.Fatalf("restricted trip: %v", err)
	}
	if code, err := cmdCircuit(ctx, []string{"status", "-c", cfg}); err != nil || code != 0 {
		t.Fatalf("status stays allowed: %d %v", code, err)
	}
	if code, err := cmdCircuit(ctx, []string{"reset", "-c", cfg, "--break-glass", "server down, INC-42"}); err != nil || code != 0 {
		t.Fatalf("break-glass reset: %d %v", code, err)
	}
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var a journal.Entry
		if json.Unmarshal([]byte(line), &a) == nil && a.Kind == journal.KindAudit &&
			a.Action == "breakglass.circuit.reset" && a.Reason == "server down, INC-42" && strings.HasPrefix(a.Actor, "cli:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("break-glass not audited:\n%s", raw)
	}

	write("full")
	if code, err := cmdCircuit(ctx, []string{"trip", "-c", cfg}); err != nil || code != 3 { // 3: circuit OPEN
		t.Fatalf("local_cli: full: %d %v", code, err)
	}
}

func TestLocalFourEyes(t *testing.T) {
	e := &orchestrator.Engine{Cfg: &config.Config{Auth: config.Auth{FourEyes: true}}}
	me := cliActor()
	if err := localFourEyes(e, &common{}, &model.Deployment{CreatedBy: me}); err == nil || !strings.Contains(err.Error(), "four-eyes") {
		t.Fatalf("creator deciding: %v", err)
	}
	if err := localFourEyes(e, &common{}, &model.Deployment{CreatedBy: "sa:ci", RollbackRequestedBy: me}); err == nil {
		t.Fatal("requester deciding")
	}
	if err := localFourEyes(e, &common{}, &model.Deployment{CreatedBy: "sa:ci"}); err != nil {
		t.Fatalf("someone else: %v", err)
	}
	if err := localFourEyes(e, &common{breakGlass: "INC-7"}, &model.Deployment{CreatedBy: me}); err != nil {
		t.Fatalf("break-glass: %v", err)
	}
	e.Cfg.Auth.FourEyes = false
	if err := localFourEyes(e, &common{}, &model.Deployment{CreatedBy: me}); err != nil {
		t.Fatalf("four_eyes off: %v", err)
	}
}

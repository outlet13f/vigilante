package main

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readZip(t *testing.T, path string) (names map[string]bool, all string) {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	names = map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		all += string(b)
	}
	return names, all
}

func TestSupportBundle(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, "state", "j.jsonl")
	logPath := filepath.Join(dir, "server.log")
	_ = os.WriteFile(logPath, []byte("time=... msg=login token=vgl_abcdefghijklmnop\n"), 0o600)
	run := func(name, cfg string) (map[string]bool, string) {
		cfgPath := filepath.Join(dir, name+".yaml")
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, name+".zip")
		code, err := cmdSupportBundle(context.Background(), []string{"-c", cfgPath, "--out", out, "--no-doctor", "--log", logPath})
		if err != nil || code != 0 {
			t.Fatalf("%s: %d %v", name, code, err)
		}
		// An existing bundle is never overwritten.
		if _, err := cmdSupportBundle(context.Background(), []string{"-c", cfgPath, "--out", out, "--no-doctor"}); err == nil {
			t.Fatalf("%s: overwrote an existing bundle", name)
		}
		return readZip(t, out)
	}
	head := "version: v1\nserver: {journal_path: " + filepath.ToSlash(journal) + ", state: {backend: file, dsn: \"postgres://v:dsn-secret-pw@db/v\"}}\n"
	valid := head + `targets: [{name: a}]
executors: {x: {type: webhook, webhook: {url: "http://127.0.0.1:1/rollback", headers: {Authorization: "Bearer topsecretvalue1"}}}}
services: []
`
	// A config that does not load (unknown key) is still collected, redacted.
	broken := valid + "credentials:\n  lb: {type: basic, user: admin, password: plain-secret-pw}\n"

	for name, cfg := range map[string]string{"valid": valid, "broken": broken} {
		names, all := run(name, cfg)
		for _, want := range []string{"MANIFEST.txt", "version.txt", "env.txt", "config/vigilante.yaml", "config/validate.txt", "logs/" + strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(strings.TrimLeft(logPath, "/\\"))} {
			if !names[want] {
				t.Errorf("%s: missing %s (have %v)", name, want, names)
			}
		}
		for _, secret := range []string{"plain-secret-pw", "dsn-secret-pw", "topsecretvalue1", "abcdefghijklmnop"} {
			if strings.Contains(all, secret) {
				t.Errorf("%s: secret %q is in the bundle", name, secret)
			}
		}
		switch name {
		case "valid":
			if !strings.Contains(all, "OK: 1 targets") || !strings.Contains(all, "doctor checks (--no-doctor)") || !strings.Contains(all, "state store: journal") {
				t.Errorf("valid: manifest should list what was not collected:\n%s", all)
			}
		case "broken":
			if !strings.Contains(all, "INVALID:") {
				t.Errorf("broken: validation result missing:\n%s", all)
			}
		}
	}
	// The bundle never creates state.
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("support-bundle created the journal: %v", err)
	}
}

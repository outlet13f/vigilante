package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFeedbackAndPilotReport(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "v.yaml")
	if err := os.WriteFile(cfg, []byte(`version: v1
server: {journal_path: `+filepath.ToSlash(filepath.Join(dir, "j.jsonl"))+`}
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true", on: local}}}
services:
  - name: svc
    targets: [a]
    probes: [{id: h, type: tcp, interval: 50ms, timeout: 50ms, tcp: {address: "127.0.0.1:1"}}]
    rules: [{name: down, when: {metric: h.consecutive_failures, op: ">=", value: 3}}]
    phases: {canary: {observation_window: 3s, eval_interval: 100ms}}
    rollback: {executor: x, mode: auto}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A canary whose probe never answers: FAIL, rolled back (exit 2).
	if code, err := run(ctx, "watch", []string{"-c", cfg, "--id", "p1", "--service", "svc", "--version", "v2", "--previous", "v1", "--phase", "canary"}); code != 2 {
		t.Fatalf("watch: %d %v", code, err)
	}
	if _, err := run(ctx, "feedback", []string{"-c", cfg, "--id", "p1", "--outcome", "false_negative"}); err == nil {
		t.Fatal("false_negative accepted for a failed deployment")
	}
	if code, err := run(ctx, "feedback", []string{"-c", cfg, "--id", "p1", "--outcome", "false_positive", "--note", "port closed by a firewall change"}); code != 0 {
		t.Fatalf("feedback: %d %v", code, err)
	}
	out := filepath.Join(dir, "report.json")
	code, err := run(ctx, "pilot", []string{"report", "-c", cfg, "--json", "--out", out})
	if err != nil || code != 4 {
		t.Fatalf("one deployment with a false positive must not meet the gate: %d %v", code, err)
	}
	raw, _ := os.ReadFile(out)
	var r struct {
		Deployments int
		Verdicts    struct{ Fail int }
		Feedback    struct {
			FalsePositive int `json:"false_positive"`
		}
		Rollbacks  struct{ Completed int }
		GatePassed bool `json:"gate_passed"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if r.Deployments != 1 || r.Verdicts.Fail != 1 || r.Feedback.FalsePositive != 1 || r.Rollbacks.Completed != 1 || r.GatePassed {
		t.Fatalf("report: %s", raw)
	}
}

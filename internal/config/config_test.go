package config

import (
	"strings"
	"testing"
)

func TestReferenceConfigValidates(t *testing.T) {
	c, err := Load("../../examples/config/vigilante.yaml")
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := c.Service("order-api")
	if got := svc.PhaseTargets("canary"); len(got) != 1 || got[0] != "order-bm-01" {
		t.Fatalf("canary targets %v", got)
	}
	if got := svc.PhaseTargets("rolling"); len(got) != 2 {
		t.Fatalf("rolling (50%%, cumulative) targets %v", got)
	}
	if got := svc.PhaseTargets("full"); len(got) != 4 {
		t.Fatalf("full targets %v", got)
	}
	if got := svc.Controls("canary"); len(got) != 3 {
		t.Fatalf("auto controls %v", got)
	}
}

func TestValidationCatchesBrokenReferences(t *testing.T) {
	_, err := Parse([]byte(`
version: v1
targets:
  - {name: a, connection: {type: ssh, credential: nope}}
executors:
  x: {type: symlink, symlink: {link: /l}}
services:
  - name: s
    targets: [a, ghost]
    probes: [{id: h, type: http, http: {url: "http://x"}}]
    rules:
      - name: r
        when: {metric: missing.up, op: "~", value: 1}
      - name: r2
        when: {metric: h.up, value: 1, baseline: {increase_pct: 10}}
    phases:
      canary: {targets: [zzz], observation_window: 1s, warmup: 5s}
    rollback:
      executor: nope
      plan: [{action: traffic.drain}]
`))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{
		`unknown credential "nope"`, `ssh connection needs address`, `symlink.link and symlink.releases_dir`,
		`unknown target "ghost"`, `metric "missing.up"`, `unknown op "~"`, `exactly one of value or baseline`,
		`target "zzz" is not a service target`, `warmup must be shorter`, `unknown rollback executor "nope"`,
		`traffic.drain needs rollback.traffic`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing error %q in:\n%v", want, err)
		}
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	_, err := Parse([]byte("version: v1\ntargetz: []\n"))
	if err == nil || !strings.Contains(err.Error(), "targetz") {
		t.Fatalf("typo in a key must fail loudly: %v", err)
	}
}

func TestServiceWithoutRollbackRuleRejected(t *testing.T) {
	base := `
version: v1
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true"}}}
services:
  - name: s
    targets: [a]
    probes: [{id: h, type: tcp, tcp: {address: "x:1"}}]
    rollback: {executor: x}
`
	_, err := Parse([]byte(base))
	if err == nil || !strings.Contains(err.Error(), "at least one rule with action: rollback") {
		t.Fatalf("service without rules must be rejected, got %v", err)
	}
	_, err = Parse([]byte(base + `    rules: [{name: warn, action: notify, when: {metric: h.up, op: "==", value: 0}}]
`))
	if err == nil || !strings.Contains(err.Error(), "at least one rule with action: rollback") {
		t.Fatalf("notify-only service must be rejected, got %v", err)
	}
	_, err = Parse([]byte(base + `    rules:
      - {name: down, when: {metric: h.up, op: "==", value: 0}}
      - {name: warn, action: notify, when: {metric: h.up, op: "==", value: 0}}
    phases:
      canary: {rules: [warn]}
`))
	if err == nil || !strings.Contains(err.Error(), "could never fail") {
		t.Fatalf("phase selecting only notify rules must be rejected, got %v", err)
	}
}

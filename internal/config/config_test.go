package config

import (
	"strings"
	"testing"
	"time"
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

func TestStateAndHAValidation(t *testing.T) {
	base := `
version: v1
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true"}}}
services:
  - name: s
    targets: [a]
    probes: [{id: h, type: tcp, tcp: {address: "x:1"}}]
    rules: [{name: down, when: {metric: h.up, op: "==", value: 0}}]
    rollback: {executor: x}
server:
`
	cases := map[string]string{
		"  ha: {enabled: true, advertise_url: http://n1}\n":                                                   "requires server.state.backend: postgres",
		"  state: {backend: postgres}\n":                                                                      "postgres needs dsn, dsn_env or dsn_ref",
		"  state: {backend: etcd}\n":                                                                          "must be file|postgres",
		"  state: {backend: postgres, dsn_env: PG}\n  ha: {enabled: true}\n":                                  "advertise_url required",
		"  state: {backend: postgres, dsn_env: PG}\n  ha: {enabled: true, advertise_url: u, lease_ttl: 1s}\n": "at least 3s",
	}
	for tail, want := range cases {
		if _, err := Parse([]byte(base + tail)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", strings.TrimSpace(tail), err, want)
		}
	}
	c, err := Parse([]byte(base + "  state: {backend: postgres, dsn_env: PG}\n  ha: {enabled: true, advertise_url: http://n1:8088}\n"))
	if err != nil || c.Server.HA.LeaseTTL != 15*time.Second {
		t.Fatalf("valid HA config: %v %+v", err, c)
	}
}

func TestAuthValidation(t *testing.T) {
	base := `
version: v1
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true"}}}
services:
  - name: s
    targets: [a]
    probes: [{id: h, type: tcp, tcp: {address: "x:1"}}]
    rules: [{name: down, when: {metric: h.up, op: "==", value: 0}}]
    rollback: {executor: x}
auth:
`
	h := strings.Repeat("ab", 32)
	cases := map[string]string{
		"  service_accounts: [{name: ci, token_sha256: nothex, roles: [{role: deployer}]}]\n":                         "64 hex",
		"  service_accounts: [{name: ci, token_sha256: " + h + ", roles: [{role: root}]}]\n":                          `unknown role "root"`,
		"  service_accounts: [{name: ci, token_sha256: " + h + ", roles: [{role: deployer, scope: service=nope}]}]\n": "unknown service",
		"  service_accounts: [{name: ci, token_sha256: " + h + ", roles: [{role: deployer, scope: env=prod}]}]\n":     "must be *, team=",
		"  service_accounts: [{name: ci, token_sha256: " + h + ", expires: soon, roles: [{role: admin}]}]\n":          "YYYY-MM-DD",
		"  service_accounts: [{name: ci, token_sha256: " + h + "}]\n":                                                 "no roles",
		"  role_bindings: [{group: sre, role: operator}]\n":                                                           "auth.oidc is required",
		"  oidc: {issuer: https://idp}\n":                                                "issuer and audience",
		"  oidc: {issuer: https://idp, audience: v}\n  role_bindings: [{role: admin}]\n": "exactly one of group or user",
	}
	for tail, want := range cases {
		if _, err := Parse([]byte(base + tail)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", strings.TrimSpace(tail), err, want)
		}
	}
	if _, err := Parse([]byte(base + "  four_eyes: true\n  service_accounts: [{name: ci, token_sha256: " + h + ", expires: 2027-01-31, roles: [{role: deployer, scope: team=payments}]}]\n")); err != nil {
		t.Fatalf("valid auth block: %v", err)
	}
}

func TestSecretsValidation(t *testing.T) {
	base := `
version: v1
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true"}}}
services:
  - name: s
    targets: [a]
    probes: [{id: h, type: tcp, tcp: {address: "x:1"}}]
    rules: [{name: down, when: {metric: h.up, op: "==", value: 0}}]
    rollback: {executor: x}
`
	vault := "secrets: {vault: {address: https://vault:8200}}\n"
	cases := map[string]string{
		"credentials: {f5: {type: basic, password_ref: secret/f5}}\n":                        "must be vault:<mount>/<path>#<key>",
		"credentials: {f5: {type: basic, password_ref: \"vault:secret#pw\"}}\n" + vault:      "must be vault:",
		"credentials: {f5: {type: basic, password_ref: \"vault:secret/prod/f5\"}}\n" + vault: "must be vault:",
		"credentials: {f5: {type: basic, password_ref: \"vault:secret/prod/f5#pw\"}}\n":      "secrets.vault: required",
		"credentials: {k: {type: ssh, user: u, ssh_ca: {mount: ssh}}}\n" + vault:             "mount and role are required",
		"credentials: {k: {type: basic, ssh_ca: {mount: ssh, role: r}}}\n" + vault:           "only for type ssh",
		"secrets: {vault: {auth: token}}\n":                                                  "address required",
		"secrets: {vault: {address: a, auth: approle}}\n":                                    "role_id_env and secret_id_env",
		"secrets: {vault: {address: a, auth: kubernetes}}\n":                                 "k8s_role",
		"secrets: {vault: {address: a, auth: ldap}}\n":                                       "token, approle or kubernetes",
		"server: {state: {backend: postgres, dsn_ref: \"vault:db/creds\"}}\n" + vault:        "server.state.dsn_ref",
	}
	for tail, want := range cases {
		if _, err := Parse([]byte(base + tail)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", strings.TrimSpace(tail), err, want)
		}
	}
	ok := base + `credentials:
  f5: {type: basic, user: admin, password_ref: "vault:secret/prod/f5#password"}
  k: {type: ssh, user: deploy, ssh_ca: {mount: ssh-client-signer, role: ops, ttl: 15m}}
  tok: {type: token, token_ref: "file:/run/secrets/token"}
secrets: {cache_ttl: 2m, vault: {address: https://vault:8200, auth: approle, role_id_env: R, secret_id_env: S}}
`
	c, err := Parse([]byte(ok))
	if err != nil {
		t.Fatalf("valid secrets block: %v", err)
	}
	if c.Credentials["k"].SSHCA.TTL != 15*time.Minute || c.Secrets.CacheTTL != 2*time.Minute {
		t.Fatalf("parsed: %+v %+v", c.Credentials["k"].SSHCA, c.Secrets)
	}
	for ref, want := range map[string]bool{"env:X": true, "env:": false, "file:/a": true, "vault:kv/a/b#k": true, "vault:kv#k": false, "x:y": false} {
		if ValidRef(ref) != want {
			t.Errorf("ValidRef(%q) != %v", ref, want)
		}
	}
}

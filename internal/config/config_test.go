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

	// Replicas sharing one config (a ConfigMap) take their own address and
	// node ID from the environment.
	t.Setenv("VIGILANTE_HA_ADVERTISE_URL", "http://10.0.0.7:8088")
	t.Setenv("VIGILANTE_HA_NODE_ID", "vigilante-0")
	c, err = Parse([]byte(base + "  state: {backend: postgres, dsn_env: PG}\n  ha: {enabled: true, advertise_url: http://ignored:8088}\n"))
	if err != nil || c.Server.HA.AdvertiseURL != "http://10.0.0.7:8088" || c.Server.HA.NodeID != "vigilante-0" {
		t.Fatalf("environment overrides: %v %+v", err, c.Server.HA)
	}
	if _, err := Parse([]byte(base + "  state: {backend: postgres, dsn_env: PG}\n  ha: {enabled: true}\n")); err != nil {
		t.Fatalf("advertise_url from the environment alone: %v", err)
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

func TestOpenStackValidation(t *testing.T) {
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
	ac := `  os: {type: openstack, auth_url: "https://keystone/v3", application_credential_id: ac, application_credential_secret_ref: "env:S"}` + "\n"
	cases := map[string]string{
		"credentials:\n  os: {type: openstack, application_credential_id: ac, application_credential_secret_ref: \"env:S\"}\n":                                            "needs auth_url",
		"credentials:\n  os: {type: openstack, auth_url: u, application_credential_id: ac}\n":                                                                             "application_credential_secret_ref",
		"credentials:\n  os: {type: openstack, auth_url: u}\n":                                                                                                            "application_credential_id (recommended)",
		"credentials:\n  os: {type: openstack, auth_url: u, user: d, password_ref: \"env:P\"}\n":                                                                          "project_name or project_id",
		"credentials:\n  os: {type: openstack, auth_url: u, user: d, project_name: p}\n":                                                                                  "password_ref or password_env",
		"credentials:\n" + ac + "  os2: {type: openstack, auth_url: u, interface: private, application_credential_id: a, application_credential_secret_ref: \"env:S\"}\n": "interface must be",
	}
	for tail, want := range cases {
		if _, err := Parse([]byte(base + tail)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", strings.TrimSpace(tail), err, want)
		}
	}
	withExec := func(execYAML, trafficYAML string) string {
		return strings.Replace(base, "executors: {x: {type: exec, exec: {rollback: \"true\"}}}",
			"executors:\n  x: {type: exec, exec: {rollback: \"true\"}}\n  "+execYAML+"\n"+trafficYAML, 1) +
			"credentials:\n" + ac + "  b: {type: basic, user: u, password_env: P}\n"
	}
	for cfg, want := range map[string]string{
		withExec("vm: {type: openstack, openstack: {credential: b}}", ""):                                                           "must be of type openstack",
		withExec("vm: {type: openstack, openstack: {}}", ""):                                                                        "credential required",
		withExec("vm: {type: openstack, openstack: {credential: os, mode: snapshot}}", ""):                                          "mode must be auto, volume or image",
		withExec("vm: {type: openstack, openstack: {credential: os}}", "traffic: {lb: {type: octavia, octavia: {credential: os}}}"): "octavia.pool_id and octavia.member_port",
	} {
		if _, err := Parse([]byte(cfg)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want %q", err, want)
		}
	}
	c, err := Parse([]byte(withExec("vm: {type: openstack, openstack: {credential: os}}",
		"traffic: {lb: {type: octavia, octavia: {credential: os, pool_id: p, member_port: 8080}}}")))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Executors["vm"].OpenStack
	if o.Mode != "auto" || o.ServerID != "{{.Labels.openstack_server_id}}" || !*o.PowerOn || *o.KeepSnapshots != 3 || o.RevertTimeout != 15*time.Minute {
		t.Fatalf("defaults: %+v", o)
	}
	if oc := c.Traffic["lb"].Octavia; oc.MemberAddress != "{{.Address}}" || oc.WaitTimeout != 5*time.Minute {
		t.Fatalf("octavia defaults: %+v", oc)
	}
}

func TestRollbackModeValidationAndWarning(t *testing.T) {
	svc := func(rb string) string {
		return `
version: v1
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true"}}}
services:
  - name: s
    targets: [a]
    probes: [{id: h, type: tcp, tcp: {address: "x:1"}}]
    rules: [{name: down, when: {metric: h.up, op: "==", value: 0}}]
    rollback: {executor: x` + rb + `}
`
	}
	for rb, want := range map[string]string{
		", mode: manual": "rollback.mode must be auto or approve",
		", mode: approve, approval: {on_timeout: x}":     "on_timeout must be hold or rollback",
		", mode: approve, approval: {drain_first: true}": "drain_first needs rollback.traffic",
		", mode: approve, approval: {timeout: 10s}":      "at least 1m",
	} {
		if _, err := Parse([]byte(svc(rb))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", rb, err, want)
		}
	}
	c, err := Parse([]byte(svc("")))
	if err != nil {
		t.Fatal(err)
	}
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "rollback.mode is not set") || c.Services[0].Rollback.RollbackMode() != "auto" {
		t.Fatalf("unset mode: %v", w)
	}
	c, _ = Parse([]byte(svc(", mode: approve")))
	if len(c.Warnings()) != 0 || c.Services[0].Rollback.Approval.Timeout != 30*time.Minute || c.Services[0].Rollback.Approval.OnTimeout != "hold" {
		t.Fatalf("approve defaults: %+v %v", c.Services[0].Rollback.Approval, c.Warnings())
	}
}

func TestNotifyAndITSMValidation(t *testing.T) {
	base := `
version: v1
credentials: {snow: {type: basic, user: u, password_env: P}, key: {type: ssh, user: u}}
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true"}}}
services:
  - name: s
    targets: [a]
    probes: [{id: h, type: tcp, tcp: {address: "x:1"}}]
    rules: [{name: down, when: {metric: h.up, op: "==", value: 0}}]
    rollback: {executor: x, mode: auto}
`
	for tail, want := range map[string]string{
		"notify: [{type: sms, url: x}]\n":                                                            "type must be webhook, slack, teams, email or pagerduty",
		"notify: [{type: teams}]\n":                                                                  "url, url_env or url_ref required",
		"notify: [{type: email, smtp: {host: h}}]\n":                                                 "smtp.host, smtp.from and smtp.to required",
		"notify: [{type: pagerduty}]\n":                                                              "routing_key_ref or routing_key_env",
		"notify: [{type: slack, url: x, min_level: loud}]\n":                                         "min_level",
		"notify: [{type: slack, url: x, services: [nope]}]\n":                                        "unknown service",
		"itsm: {servicenow: {url: u}}\n":                                                             "url and credential required",
		"itsm: {servicenow: {url: u, credential: key}}\n":                                            "must be of type basic or token",
		"itsm: {servicenow: {url: u, credential: snow, change_gate: {on_error: maybe}}}\n":           "on_error must be open or closed",
		"itsm: {servicenow: {url: u, credential: snow, incidents: {on: [deploy]}}}\n":                "rollback_failed | circuit_opened",
		"itsm: {servicenow: {url: u, credential: snow, incidents: {urgency: 9}}}\n":                  "urgency and impact must be 1..3",
		"console: {redirect_url: https://v.example/console/auth/callback}\n":                         "needs auth.oidc",
		"auth: {oidc: {issuer: i, audience: a}}\nconsole: {redirect_url: https://v.example/login}\n": "/console/auth/callback",
		"server: {tls: {cert_file: a.crt}}\n":                                                        "cert_file and key_file required",
		"server: {tls: {cert_file: a.crt, key_file: a.key, client_auth: require}}\n":                 "needs client_ca_file",
		"server: {tls: {cert_file: a.crt, key_file: a.key, min_version: \"1.1\"}}\n":                 "min_version must be 1.2 or 1.3",
		"agent: {tls: {cert_file: a.crt}}\n":                                                         "cert_file and key_file go together",
		"console: {session_key_ref: plain-text-key}\n":                                               "console.session_key_ref",
	} {
		if _, err := Parse([]byte(base + tail)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", strings.TrimSpace(tail), err, want)
		}
	}
	c, err := Parse([]byte(base + "itsm: {servicenow: {url: u, credential: snow, change_gate: {enabled: true}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	sn := c.ITSM.ServiceNow
	if sn.ChangeGate.OnError != "closed" || len(sn.ChangeGate.AllowedStates) != 2 || len(sn.Incidents.On) != 2 || sn.Incidents.Urgency != 1 {
		t.Fatalf("defaults: %+v", sn)
	}
}

func TestRemoteGrepHasNoLineCount(t *testing.T) {
	cfg := func(rule string) string {
		return `
version: v1
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true"}}}
services:
  - name: s
    targets: [a]
    probes: [{id: app, type: log, log: {path: /var/log/app.log, patterns: {fatal: FATAL}, remote_grep: "FATAL"}}]
    rules: [{name: r, when: ` + rule + `}]
    rollback: {executor: x, mode: auto}
`
	}
	if _, err := Parse([]byte(cfg(`{metric: app.match.fatal, agg: sum, op: ">", value: 0}`))); err != nil {
		t.Fatalf("match metric with remote_grep: %v", err)
	}
	for _, rule := range []string{
		`{metric: app.lines, agg: rate, op: "<", value: 1}`,
		`{any: [{metric: app.match.fatal, op: ">", value: 0}, {not: {metric: app.lines, op: ">", value: 0}}]}`,
		`{metric: app.match.fatal, ratio_of: app.lines, op: ">", value: 5}`,
	} {
		if _, err := Parse([]byte(cfg(rule))); err == nil || !strings.Contains(err.Error(), "app.lines is not available") {
			t.Errorf("%s: %v", rule, err)
		}
	}
}

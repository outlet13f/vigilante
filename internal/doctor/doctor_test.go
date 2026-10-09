package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"vigilante/internal/config"
	"vigilante/internal/secrets"
	"vigilante/internal/secrets/vaulttest"
	"vigilante/internal/transport"
)

const procOut = "0.5 0 0 1/1 1\n@@\ncpu 1 1 1 1 1 0 0 0\n@@\nMemTotal: 100 kB\nMemAvailable: 50 kB\n@@\n@@\n2\n"

func testConfig(t *testing.T, extraLogProbes int) *config.Config {
	t.Helper()
	var extra strings.Builder
	for i := 0; i < extraLogProbes; i++ {
		fmt.Fprintf(&extra, "      - {id: log%d, type: log, log: {path: /var/log/x%d.log, patterns: {e: ERROR}}}\n", i, i)
	}
	cfg, err := config.Parse([]byte(`
version: v1
credentials:
  ssh-key: {type: ssh, user: deploy, private_key_file: /nonexistent/key, insecure_ignore_host_key: true}
targets:
  - {name: app-1, address: 10.0.0.1, connection: {type: ssh, credential: ssh-key, sudo: true}}
  - {name: app-2, address: 10.0.0.2, connection: {type: ssh, credential: ssh-key}}
  - {name: lb-1, address: 10.0.0.9, connection: {type: ssh, credential: ssh-key}}
traffic:
  edge: {type: nginx, nginx: {hosts: [lb-1], upstream_file: /etc/nginx/up.conf, member_format: "{{.Address}}:8080"}}
executors:
  link: {type: symlink, symlink: {link: /opt/app/current, releases_dir: /opt/app/releases, init: systemd, unit: app}}
services:
  - name: order
    targets: [app-1, app-2]
    probes:
      - {id: access, type: access_log, access_log: {path: /var/log/access.log}}
      - {id: host, type: host}
      - {id: db, type: db, interval: 1s, timeout: 500ms, db: {driver: postgres, dsn: "postgres://x@127.0.0.1:1/db", pool_size: 5}}
` + extra.String() + `
    rules: [{name: down, when: {metric: access.count_5xx, ratio_of: access.requests, op: ">", value: 2}}]
    rollback: {executor: link, traffic: edge}
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func runners() map[string]*transport.Mock {
	app1 := (&transport.Mock{Name: "app-1"}).On("echo vigilante-ok", "", errors.New("sudo: a password is required"))
	app2 := (&transport.Mock{Name: "app-2"}).
		On("echo vigilante-ok", "vigilante-ok\n", nil).
		On("tail -n 200 '/var/log/access.log'", `10.0.0.5 - - [10/Oct/2026:10:00:00 +0900] "GET / HTTP/1.1" 200 5 "-" "x" 0.010`+"\n", nil).
		On("tail -n 200", "line\n", nil).
		On("/proc/loadavg", procOut, nil).
		On("readlink -f", "/opt/app/releases/v2\n", nil).
		On("test -d '/opt/app/releases/v1'", "", errors.New("exit status 1")).
		On("systemctl is-active", "active\n", nil)
	lb := (&transport.Mock{Name: "lb-1"}).
		On("echo vigilante-ok", "vigilante-ok\n", nil).
		On("cat ", "upstream app {\n  server 10.0.0.1:8080;\n}\n", nil)
	return map[string]*transport.Mock{"app-1": app1, "app-2": app2, "lb-1": lb}
}

func find(t *testing.T, cs []Check, scope, subject, nameContains string) Check {
	t.Helper()
	for _, c := range cs {
		if c.Scope == scope && c.Subject == subject && strings.Contains(c.Name, nameContains) {
			return c
		}
	}
	t.Fatalf("no check %s/%s/%q in:\n%s", scope, subject, nameContains, dump(cs))
	return Check{}
}

func dump(cs []Check) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "%s %s %s %s — %s\n", c.Status, c.Scope, c.Subject, c.Name, c.Detail)
	}
	return b.String()
}

func runDoctor(t *testing.T, cfg *config.Config, previous string) []Check {
	t.Helper()
	m := runners()
	cs, err := Run(context.Background(), cfg, Options{Previous: previous, Runners: func(n string) (transport.Runner, error) {
		return m[n], nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestDoctorFindsRealProblems(t *testing.T) {
	cs := runDoctor(t, testConfig(t, 0), "v1")

	if c := find(t, cs, ScopeCredential, "ssh-key", "자격증명"); c.Status != Fail || !strings.Contains(c.Detail, "/nonexistent/key") {
		t.Errorf("missing key file: %+v", c)
	}
	if c := find(t, cs, ScopeTarget, "app-1", "SSH"); c.Status != Fail || !strings.Contains(c.Hint, "NOPASSWD") {
		t.Errorf("sudo without NOPASSWD must fail with a sudoers hint: %+v", c)
	}
	if c := find(t, cs, ScopeTarget, "app-2", "SSH"); c.Status != OK {
		t.Errorf("app-2 reachable: %+v", c)
	}
	if c := find(t, cs, ScopeExecutor, "app-2", "이전 릴리스"); c.Status != Fail {
		t.Errorf("missing previous release must fail: %+v", c)
	}
	if c := find(t, cs, ScopeExecutor, "app-2", "서비스 app"); c.Status != OK {
		t.Errorf("unit active: %+v", c)
	}
	if c := find(t, cs, ScopeProbe, "app-2", "access"); c.Status != OK || !strings.Contains(c.Detail, "1줄 중 1줄") {
		t.Errorf("access log format: %+v", c)
	}
	if c := find(t, cs, ScopeProbe, "app-2", "host"); c.Status != OK {
		t.Errorf("host probe: %+v", c)
	}
	if c := find(t, cs, ScopeProbe, "app-2", "db"); c.Status != Fail {
		t.Errorf("unreachable db must fail: %+v", c)
	}
	if c := find(t, cs, ScopeTraffic, "edge", "풀에 있는지"); c.Status != Fail || !strings.Contains(c.Detail, "app-2(10.0.0.2:8080)") {
		t.Errorf("app-2 missing from the nginx upstream must fail: %+v", c)
	}
	if c := find(t, cs, ScopeCapacity, "order", "DB 프로브"); c.Status != Warn {
		t.Errorf("5 new connections/s x 2 targets must warn: %+v", c)
	}
	// Scopes come out in report order.
	for i := 1; i < len(cs); i++ {
		if scopeOrder[cs[i-1].Scope] > scopeOrder[cs[i].Scope] {
			t.Fatalf("out of order at %d:\n%s", i, dump(cs))
		}
	}
}

func TestDoctorAccessLogFormatMismatch(t *testing.T) {
	cfg := testConfig(t, 0)
	cfg.Services[0].Probes[0].AccessLog.Format = "json"
	cs := runDoctor(t, cfg, "")
	if c := find(t, cs, ScopeProbe, "app-2", "access"); c.Status != Fail || !strings.Contains(c.Hint, "format") {
		t.Errorf("combined lines read as json must fail: %+v", c)
	}
	if c := find(t, cs, ScopeExecutor, "app-2", "이전 릴리스"); c.Status != Skip {
		t.Errorf("without --previous the release check is skipped: %+v", c)
	}
}

func TestDoctorWarnsOnSSHSessionBudget(t *testing.T) {
	cs := runDoctor(t, testConfig(t, 8), "")
	// 9 log streams + 1 host poll + 2 reserved = 12 > 10
	if c := find(t, cs, ScopeCapacity, "app-2", "SSH 세션"); c.Status != Warn || !strings.Contains(c.Detail, "= 12") {
		t.Errorf("session budget: %+v", c)
	}
}

func TestHint(t *testing.T) {
	cases := map[string]string{
		"ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey]": "authorized_keys",
		"knownhosts: key mismatch":                      "known_hosts",
		"dial tcp 10.0.0.1:22: i/o timeout":             "방화벽",
		"f5 PATCH ...: 403 Forbidden":                   "역할",
		"x509: certificate signed by unknown authority": "CA",
	}
	for in, want := range cases {
		if h := Hint(in); !strings.Contains(h, want) {
			t.Errorf("Hint(%q) = %q, want it to mention %q", in, h, want)
		}
	}
}

func TestDoctorChecksVaultReferences(t *testing.T) {
	v := vaulttest.New(t)
	v.Put("secret/prod/ssh", map[string]any{"passphrase": "key-passphrase"})
	v.Deny("ssh-client-signer/sign/ops")
	t.Setenv("VGL_VAULT_TOKEN", v.RootToken)
	if err := secrets.Configure(config.Secrets{Vault: &config.Vault{Address: v.URL, TokenEnv: "VGL_VAULT_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secrets.Configure(config.Secrets{}) })

	cfg := testConfig(t, 0)
	c := cfg.Credentials["ssh-key"]
	c.PrivateKeyFile = ""
	c.PassphraseRef = "vault:secret/prod/ssh#passphrase"
	c.PasswordRef = "vault:secret/prod/ssh#password"
	c.SSHCA = &config.SSHCA{Mount: "ssh-client-signer", Role: "ops"}
	cfg.Credentials["ssh-key"] = c

	got := find(t, runDoctor(t, cfg, ""), ScopeCredential, "ssh-key", "자격증명")
	if got.Status != Fail || !strings.Contains(got.Detail, "secret/prod/ssh#password") || !strings.Contains(got.Detail, "ssh_ca sign") {
		t.Fatalf("missing key and denied CA role must both be reported: %+v", got)
	}
	if strings.Contains(got.Detail, "passphrase") {
		t.Fatalf("readable reference must not be reported: %+v", got)
	}
	if !strings.Contains(got.Hint, "sign/<role>") {
		t.Fatalf("hint should point at the vault policy: %+v", got)
	}
}

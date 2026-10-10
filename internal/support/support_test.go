package support

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

const cfg = `# production config
version: v1
server:
  auth_token_env: VIGILANTE_TOKEN   # a reference: kept
  state: {backend: postgres, dsn: "postgres://vigilante:Sup3rS3cret@db:5432/vgl"}
credentials:
  f5:
    type: basic
    user: admin
    password: hunter2-plain
    password_ref: "vault:secret/prod/f5#password"
  ssh:
    type: ssh
    private_key: "-----BEGIN OPENSSH PRIVATE KEY-----abc"
    passphrase_env: SSH_PASS
auth:
  service_accounts:
    - {name: ci, token_sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08}
  oidc: {issuer: "https://sso.example/realms/corp", audience: vigilante, username_claim: preferred_username}
api:
  token_ttl: 1h
executors:
  deploy:
    type: webhook
    webhook:
      url: https://deploy.example/api/rollback
      headers: {Authorization: "Bearer abcdefghijklmnop", X-Team: payments}
notify:
  - {type: slack, url: "https://hooks.slack.com/services/T000/B000/XXXXXXXXXXXX", min_level: warning}
services:
  - name: web
    probes:
      - {id: h, type: http, http: {url: "http://{{.Address}}:8080/health"}}
`

func TestRedactYAML(t *testing.T) {
	out, n, err := RedactYAML([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, secret := range []string{"Sup3rS3cret", "hunter2-plain", "BEGIN OPENSSH", "9f86d081", "abcdefghijklmnop", "XXXXXXXXXXXX"} {
		if strings.Contains(s, secret) {
			t.Errorf("secret %q survived:\n%s", secret, s)
		}
	}
	for _, kept := range []string{"auth_token_env: VIGILANTE_TOKEN", "vault:secret/prod/f5#password", "passphrase_env: SSH_PASS",
		"token_ttl: 1h", "user: admin", "X-Team: payments", "https://deploy.example/api/rollback", "http://{{.Address}}:8080/health",
		"username_claim: preferred_username", "# production config", "dsn: REDACTED"} {
		if !strings.Contains(s, kept) {
			t.Errorf("%q should be kept:\n%s", kept, s)
		}
	}
	if n != 6 {
		t.Errorf("redacted %d values, want 6", n)
	}
}

func TestRedactText(t *testing.T) {
	in := `level=WARN msg="login" token=vgl_AbCdEf123456789 key=vgk_zzzzzzzzzzzz header="Authorization: Bearer eyJhbGciOi.eyJzdWIiOiJ.c2lnbmF0dXJl"
dsn=host=db user=v password=pw123 sslmode=require url=https://u:p4ss@host/x id=eyJhbGciOiJSUzI1.eyJzdWIiOiJhbGlj.ZmFrZXNpZ25hdHVy`
	out := RedactText(in)
	for _, secret := range []string{"AbCdEf123456789", "zzzzzzzzzzzz", "pw123", "p4ss", "ZmFrZXNpZ25hdHVy", "c2lnbmF0dXJl"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q survived: %s", secret, out)
		}
	}
	if !strings.Contains(out, "vgl_REDACTED") || !strings.Contains(out, "sslmode=require") {
		t.Errorf("unexpected: %s", out)
	}
}

func TestBundleRedactsAndListsFiles(t *testing.T) {
	var buf bytes.Buffer
	b := New(&buf, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC))
	_ = b.Add("logs/server.log", []byte("auth ok token=vat_0123456789abcdef\n"))
	_ = b.AddJSON("state.json", map[string]any{"deployments": 3})
	b.Note("doctor skipped (--no-doctor)")
	if err := b.Close("vigilante 1.0.0"); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(data)
	}
	if strings.Contains(files["logs/server.log"], "0123456789abcdef") {
		t.Error("log not redacted")
	}
	m := files["MANIFEST.txt"]
	if !strings.Contains(m, "vigilante 1.0.0") || !strings.Contains(m, "state.json") || !strings.Contains(m, "doctor skipped") {
		t.Errorf("manifest:\n%s", m)
	}
}

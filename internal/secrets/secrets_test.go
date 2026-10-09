package secrets

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"vigilante/internal/config"
	"vigilante/internal/secrets/vaulttest"
)

var ctx = context.Background()

func TestEnvAndFileRefs(t *testing.T) {
	t.Setenv("VGL_TEST_PW", "env-password")
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := New(config.Secrets{})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := r.Resolve(ctx, "env:VGL_TEST_PW"); err != nil || v != "env-password" {
		t.Fatalf("env ref = %q, %v", v, err)
	}
	if v, err := r.Resolve(ctx, "file:"+p); err != nil || v != "file-token" {
		t.Fatalf("file ref = %q, %v (trailing newline must be trimmed)", v, err)
	}
	if _, err := r.Resolve(ctx, "env:VGL_TEST_UNSET"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unset env must be ErrNotFound, got %v", err)
	}
	if _, err := r.Resolve(ctx, "vault:secret/x#y"); err == nil || !strings.Contains(err.Error(), "no secrets.vault") {
		t.Fatalf("vault ref without vault config: %v", err)
	}
	if _, err := r.Resolve(ctx, "plain-text"); err == nil {
		t.Fatal("unknown scheme must be rejected")
	}
}

func approle(t *testing.T, v *vaulttest.Server) config.Secrets {
	t.Setenv("VGL_ROLE_ID", v.RoleID)
	t.Setenv("VGL_SECRET_ID", v.SecretID)
	return config.Secrets{CacheTTL: time.Minute, Vault: &config.Vault{
		Address: v.URL, Namespace: "ops", Auth: "approle", RoleIDEnv: "VGL_ROLE_ID", SecretIDEnv: "VGL_SECRET_ID"}}
}

func TestVaultAppRoleKVCacheAndRelogin(t *testing.T) {
	v := vaulttest.New(t)
	v.Put("secret/prod/f5", map[string]any{"password": "f5-admin-pass", "port": 443})
	r, err := New(approle(t, v))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r.Now = func() time.Time { return now }

	got, err := r.Resolve(ctx, "vault:secret/prod/f5#password")
	if err != nil || got != "f5-admin-pass" {
		t.Fatalf("resolve = %q, %v", got, err)
	}
	if v.Logins != 1 || v.Reads != 1 || v.LastNamespace != "ops" {
		t.Fatalf("logins=%d reads=%d ns=%q", v.Logins, v.Reads, v.LastNamespace)
	}
	if p, _ := r.Resolve(ctx, "vault:secret/prod/f5#port"); p != "443" {
		t.Fatalf("non-string values are formatted: %q", p)
	}

	// Within cache_ttl: no new read.
	reads := v.Reads
	if _, err := r.Resolve(ctx, "vault:secret/prod/f5#password"); err != nil || v.Reads != reads {
		t.Fatalf("cached value must not hit vault (reads %d -> %d)", reads, v.Reads)
	}

	// Token revoked early and cache expired: one 403, re-login, retry succeeds.
	v.Revoke()
	now = now.Add(2 * time.Minute)
	if got, err := r.Resolve(ctx, "vault:secret/prod/f5#password"); err != nil || got != "f5-admin-pass" {
		t.Fatalf("after revoke = %q, %v", got, err)
	}
	if v.Logins != 2 {
		t.Fatalf("403 must trigger exactly one re-login, logins=%d", v.Logins)
	}
}

func TestVaultErrors(t *testing.T) {
	v := vaulttest.New(t)
	v.Put("secret/prod/db", map[string]any{"dsn": "postgres://..."})
	v.Deny("secret/prod/hsm")
	t.Setenv("VGL_VAULT_TOKEN", v.RootToken)
	r, _ := New(config.Secrets{Vault: &config.Vault{Address: v.URL, TokenEnv: "VGL_VAULT_TOKEN"}})

	if _, err := r.Resolve(ctx, "vault:secret/prod/db#missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing key: %v", err)
	}
	if _, err := r.Resolve(ctx, "vault:secret/prod/nothing#x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing path: %v", err)
	}
	_, err := r.Resolve(ctx, "vault:secret/prod/hsm#pin")
	var ve *VaultError
	if !errors.As(err, &ve) || ve.Status != 403 {
		t.Errorf("policy denial must surface as 403: %v", err)
	}

	bad, _ := New(approle(t, v))
	t.Setenv("VGL_SECRET_ID", "wrong")
	if _, err := bad.Resolve(ctx, "vault:secret/prod/db#dsn"); err == nil || !strings.Contains(err.Error(), "vault login (approle)") {
		t.Errorf("bad approle secret: %v", err)
	}
}

func TestKubernetesAuth(t *testing.T) {
	v := vaulttest.New(t)
	v.Put("kv/app", map[string]any{"token": "k8s-secret-value"})
	jwt := filepath.Join(t.TempDir(), "sa-token")
	_ = os.WriteFile(jwt, []byte("eyJhbGciOi.fake.jwt\n"), 0o600)
	r, _ := New(config.Secrets{Vault: &config.Vault{Address: v.URL, Auth: "kubernetes", K8sRole: "vigilante", K8sJWTPath: jwt}})
	if got, err := r.Resolve(ctx, "vault:kv/app#token"); err != nil || got != "k8s-secret-value" {
		t.Fatalf("kubernetes auth = %q, %v", got, err)
	}
}

func TestSignSSHKey(t *testing.T) {
	v := vaulttest.New(t)
	t.Setenv("VGL_VAULT_TOKEN", v.RootToken)
	if err := Configure(config.Secrets{Vault: &config.Vault{Address: v.URL, TokenEnv: "VGL_VAULT_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Configure(config.Secrets{}) })

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewSignerFromKey(priv)
	cert, err := SignSSHKey(ctx, config.SSHCA{Mount: "ssh-client-signer", Role: "ops", TTL: 10 * time.Minute}, "deploy", key.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cert.SignatureKey.Marshal(), v.CA.PublicKey().Marshal()) {
		t.Fatal("certificate not signed by the vault CA")
	}
	if !bytes.Equal(cert.Key.Marshal(), key.PublicKey().Marshal()) {
		t.Fatal("certificate is for a different key")
	}
	if len(cert.ValidPrincipals) != 1 || cert.ValidPrincipals[0] != "deploy" {
		t.Fatalf("principals default to the credential user: %v", cert.ValidPrincipals)
	}
	if v.LastSign["ttl"] != "10m0s" || v.LastSign["cert_type"] != "user" {
		t.Fatalf("sign request: %v", v.LastSign)
	}

	v.Deny("ssh-client-signer/sign/ops")
	if _, err := SignSSHKey(ctx, config.SSHCA{Mount: "ssh-client-signer", Role: "ops"}, "deploy", key.PublicKey()); err == nil ||
		!strings.Contains(err.Error(), "ssh_ca sign (ssh-client-signer/ops)") {
		t.Fatalf("denied role: %v", err)
	}
}

func TestRedactHandler(t *testing.T) {
	t.Setenv("VGL_REDACT", "hunter2-very-secret")
	t.Setenv("VGL_SHORT", "abc")
	if _, err := Value(ctx, "", "VGL_REDACT"); err != nil {
		t.Fatal(err)
	}
	_, _ = Value(ctx, "", "VGL_SHORT")
	var buf bytes.Buffer
	log := slog.New(RedactHandler{slog.NewTextHandler(&buf, nil)}).With("dsn", "postgres://u:hunter2-very-secret@db/x")
	log.Info("login with hunter2-very-secret failed", "err", errors.New("bad password hunter2-very-secret"), "short", "abc")
	out := buf.String()
	if strings.Contains(out, "hunter2-very-secret") {
		t.Fatalf("secret leaked into log: %s", out)
	}
	if strings.Count(out, "[REDACTED]") != 3 {
		t.Fatalf("message, error and With() attr must all be masked: %s", out)
	}
	if !strings.Contains(out, "short=abc") {
		t.Fatalf("values shorter than 6 chars are left alone: %s", out)
	}
}

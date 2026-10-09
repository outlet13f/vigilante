package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"vigilante/internal/config"
)

// vaultClient speaks the small part of the Vault HTTP API vigilante needs:
// token / AppRole / Kubernetes login, KV v2 reads and SSH certificate signing.
type vaultClient struct {
	cfg  config.Vault
	http *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time // zero = no expiry known (token auth)
}

func newVault(cfg config.Vault) (*vaultClient, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("secrets.vault.ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("secrets.vault.ca_file: no certificates found")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	if cfg.Auth == "" {
		cfg.Auth = "token"
	}
	return &vaultClient{cfg: cfg, http: &http.Client{Transport: tr, Timeout: 15 * time.Second}}, nil
}

// VaultError is a non-2xx answer from Vault.
type VaultError struct {
	Status int
	Errors []string
}

func (e *VaultError) Error() string {
	return fmt.Sprintf("vault %d: %s", e.Status, strings.Join(e.Errors, "; "))
}

func (v *vaultClient) currentToken(ctx context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token != "" && (v.tokenExp.IsZero() || time.Now().Before(v.tokenExp.Add(-30*time.Second))) {
		return v.token, nil
	}
	return v.loginLocked(ctx)
}

func (v *vaultClient) loginLocked(ctx context.Context) (string, error) {
	switch v.cfg.Auth {
	case "token":
		env := v.cfg.TokenEnv
		if env == "" {
			env = "VAULT_TOKEN"
		}
		t := os.Getenv(env)
		if t == "" {
			return "", fmt.Errorf("vault token auth: %s is empty", env)
		}
		v.token, v.tokenExp = t, time.Time{}
		return t, nil
	case "approle":
		mount := v.cfg.AuthMount
		if mount == "" {
			mount = "approle"
		}
		return v.loginWith(ctx, mount, map[string]string{"role_id": os.Getenv(v.cfg.RoleIDEnv), "secret_id": os.Getenv(v.cfg.SecretIDEnv)})
	case "kubernetes":
		mount := v.cfg.AuthMount
		if mount == "" {
			mount = "kubernetes"
		}
		path := v.cfg.K8sJWTPath
		if path == "" {
			path = "/var/run/secrets/kubernetes.io/serviceaccount/token"
		}
		jwt, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("vault kubernetes auth: %w", err)
		}
		return v.loginWith(ctx, mount, map[string]string{"role": v.cfg.K8sRole, "jwt": strings.TrimSpace(string(jwt))})
	}
	return "", fmt.Errorf("unknown vault auth %q", v.cfg.Auth)
}

func (v *vaultClient) loginWith(ctx context.Context, mount string, body map[string]string) (string, error) {
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := v.raw(ctx, http.MethodPost, "/v1/auth/"+mount+"/login", "", body, &out); err != nil {
		return "", fmt.Errorf("vault login (%s): %w", v.cfg.Auth, err)
	}
	if out.Auth.ClientToken == "" {
		return "", errors.New("vault login returned no token")
	}
	v.token = out.Auth.ClientToken
	v.tokenExp = time.Time{}
	if out.Auth.LeaseDuration > 0 {
		v.tokenExp = time.Now().Add(time.Duration(out.Auth.LeaseDuration) * time.Second)
	}
	return v.token, nil
}

// call performs an authenticated request; a 403 with a login-based method
// re-authenticates once (the token may have been revoked or expired early).
func (v *vaultClient) call(ctx context.Context, method, path string, body, out any) error {
	tok, err := v.currentToken(ctx)
	if err != nil {
		return err
	}
	err = v.raw(ctx, method, path, tok, body, out)
	var ve *VaultError
	if errors.As(err, &ve) && ve.Status == http.StatusForbidden && v.cfg.Auth != "token" {
		v.mu.Lock()
		tok, lerr := v.loginLocked(ctx)
		v.mu.Unlock()
		if lerr != nil {
			return lerr
		}
		return v.raw(ctx, method, path, tok, body, out)
	}
	return err
}

func (v *vaultClient) raw(ctx context.Context, method, path, token string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(v.cfg.Address, "/")+path, rd)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if v.cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.cfg.Namespace)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(raw, &e)
		return &VaultError{Status: resp.StatusCode, Errors: e.Errors}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// kv reads one key of a KV v2 secret.
func (v *vaultClient) kv(ctx context.Context, mount, path, key string) (string, error) {
	var out struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := v.call(ctx, http.MethodGet, "/v1/"+mount+"/data/"+path, nil, &out); err != nil {
		var ve *VaultError
		if errors.As(err, &ve) && ve.Status == http.StatusNotFound {
			return "", ErrNotFound
		}
		return "", err
	}
	val, ok := out.Data.Data[key]
	if !ok {
		return "", fmt.Errorf("%w: key %q not in %s/%s", ErrNotFound, key, mount, path)
	}
	if s, ok := val.(string); ok {
		return s, nil
	}
	return fmt.Sprint(val), nil
}

// SignSSHKey asks Vault's SSH CA to sign pub for user, returning the certificate.
func SignSSHKey(ctx context.Context, ca config.SSHCA, user string, pub ssh.PublicKey) (*ssh.Certificate, error) {
	r := Default()
	if r.vault == nil {
		return nil, errors.New("ssh_ca: no secrets.vault configured")
	}
	principals := ca.Principals
	if len(principals) == 0 {
		principals = []string{user}
	}
	ttl := ca.TTL
	if ttl == 0 {
		ttl = 30 * time.Minute
	}
	body := map[string]any{
		"public_key":       string(ssh.MarshalAuthorizedKey(pub)),
		"valid_principals": strings.Join(principals, ","),
		"ttl":              ttl.String(),
		"cert_type":        "user",
	}
	var out struct {
		Data struct {
			SignedKey string `json:"signed_key"`
		} `json:"data"`
	}
	if err := r.vault.call(ctx, http.MethodPost, "/v1/"+ca.Mount+"/sign/"+ca.Role, body, &out); err != nil {
		return nil, fmt.Errorf("ssh_ca sign (%s/%s): %w", ca.Mount, ca.Role, err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(out.Data.SignedKey))
	if err != nil {
		return nil, fmt.Errorf("ssh_ca: unreadable signed key: %w", err)
	}
	cert, ok := key.(*ssh.Certificate)
	if !ok {
		return nil, errors.New("ssh_ca: vault did not return a certificate")
	}
	return cert, nil
}

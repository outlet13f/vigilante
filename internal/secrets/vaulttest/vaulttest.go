// Package vaulttest is an in-process fake of the Vault HTTP API subset
// vigilante uses (AppRole / Kubernetes login, KV v2 read, SSH CA sign), for
// tests in any package.
package vaulttest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type Server struct {
	*httptest.Server
	CA        ssh.Signer // signs user certificates
	RootToken string     // always valid
	RoleID    string
	SecretID  string

	mu            sync.Mutex
	kv            map[string]map[string]any // "<mount>/<path>" -> data
	tokens        map[string]bool
	denied        map[string]bool // "<mount>/<path>" the policy forbids
	Logins        int
	Reads         int
	Signs         int
	LastNamespace string
	LastSign      map[string]any
}

func New(t testing.TB) *Server {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{CA: ca, RootToken: "root-token", RoleID: "role-1", SecretID: "secret-1",
		kv: map[string]map[string]any{}, tokens: map[string]bool{}, denied: map[string]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Put stores a KV v2 secret at "<mount>/<path>".
func (s *Server) Put(path string, data map[string]any) {
	s.mu.Lock()
	s.kv[path] = data
	s.mu.Unlock()
}

// Deny makes the policy refuse a path (KV path or "<mount>/sign/<role>").
func (s *Server) Deny(path string) {
	s.mu.Lock()
	s.denied[path] = true
	s.mu.Unlock()
}

// Revoke invalidates every login token (as if they expired early).
func (s *Server) Revoke() {
	s.mu.Lock()
	s.tokens = map[string]bool{}
	s.mu.Unlock()
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastNamespace = r.Header.Get("X-Vault-Namespace")
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	str := func(k string) string { v, _ := body[k].(string); return v }

	if strings.HasPrefix(path, "auth/") && strings.HasSuffix(path, "/login") {
		ok := false
		switch {
		case strings.Contains(path, "approle"):
			ok = str("role_id") == s.RoleID && str("secret_id") == s.SecretID
		case strings.Contains(path, "kubernetes"):
			ok = str("role") != "" && str("jwt") != ""
		}
		if !ok {
			fail(w, http.StatusBadRequest, "invalid credentials")
			return
		}
		s.Logins++
		tok := fmt.Sprintf("s.login-%d", s.Logins)
		s.tokens[tok] = true
		reply(w, map[string]any{"auth": map[string]any{"client_token": tok, "lease_duration": 3600}})
		return
	}

	tok := r.Header.Get("X-Vault-Token")
	if tok != s.RootToken && !s.tokens[tok] {
		fail(w, http.StatusForbidden, "permission denied")
		return
	}
	if mount, sub, ok := strings.Cut(path, "/data/"); ok && r.Method == http.MethodGet {
		key := mount + "/" + sub
		if s.denied[key] {
			fail(w, http.StatusForbidden, "permission denied")
			return
		}
		s.Reads++
		data, ok := s.kv[key]
		if !ok {
			fail(w, http.StatusNotFound, "")
			return
		}
		reply(w, map[string]any{"data": map[string]any{"data": data, "metadata": map[string]any{"version": 1}}})
		return
	}
	if _, _, ok := strings.Cut(path, "/sign/"); ok && r.Method == http.MethodPost {
		if s.denied[path] {
			fail(w, http.StatusForbidden, "permission denied")
			return
		}
		s.Signs++
		s.LastSign = body
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(str("public_key")))
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		ttl, err := time.ParseDuration(str("ttl"))
		if err != nil {
			ttl = 30 * time.Minute
		}
		now := time.Now()
		cert := &ssh.Certificate{
			Key: pub, Serial: uint64(s.Signs), CertType: ssh.UserCert, KeyId: "vault-test",
			ValidPrincipals: strings.Split(str("valid_principals"), ","),
			ValidAfter:      uint64(now.Add(-time.Minute).Unix()),
			ValidBefore:     uint64(now.Add(ttl).Unix()),
			Permissions:     ssh.Permissions{Extensions: map[string]string{"permit-pty": ""}},
		}
		if err := cert.SignCert(rand.Reader, s.CA); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, map[string]any{"data": map[string]any{"serial_number": fmt.Sprint(cert.Serial), "signed_key": string(ssh.MarshalAuthorizedKey(cert))}})
		return
	}
	fail(w, http.StatusNotFound, "no handler for "+path)
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	errs := []string{}
	if msg != "" {
		errs = append(errs, msg)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": errs})
}

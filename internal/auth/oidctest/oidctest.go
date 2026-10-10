// Package oidctest is a minimal OpenID Connect provider for tests:
// discovery, JWKS, an authorization endpoint that signs in User without a
// prompt, and a token endpoint that enforces PKCE (S256).
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type IDP struct {
	URL string
	srv *httptest.Server
	key *rsa.PrivateKey

	mu sync.Mutex
	// User signs in at the authorization endpoint.
	User   string
	Groups []string
	// TTL of issued ID tokens (default 1h).
	TTL   time.Duration
	codes map[string]grant
	// Exchanges counts successful code exchanges.
	Exchanges int
}

type grant struct {
	challenge, clientID, user string
	groups                    []string
}

func New(t testing.TB) *IDP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &IDP{key: key, User: "alice", TTL: time.Hour, codes: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": m.URL, "jwks_uri": m.URL + "/keys",
			"authorization_endpoint": m.URL + "/auth", "token_endpoint": m.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"code_challenge_methods_supported":      []string{"S256"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("GET /auth", m.authorize)
	mux.HandleFunc("POST /token", m.token)
	m.srv = httptest.NewServer(mux)
	m.URL = m.srv.URL
	t.Cleanup(m.srv.Close)
	return m
}

func (m *IDP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "need response_type=code with an S256 code_challenge", http.StatusBadRequest)
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	code := hex.EncodeToString(b)
	m.mu.Lock()
	m.codes[code] = grant{challenge: q.Get("code_challenge"), clientID: q.Get("client_id"), user: m.User, groups: m.Groups}
	m.mu.Unlock()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	v := back.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (m *IDP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	m.mu.Lock()
	g, ok := m.codes[r.PostForm.Get("code")]
	delete(m.codes, r.PostForm.Get("code"))
	m.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		return
	}
	m.mu.Lock()
	m.Exchanges++
	m.mu.Unlock()
	id := m.Token(map[string]any{"aud": g.clientID, "sub": "id-" + g.user, "preferred_username": g.user, "groups": g.groups})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-" + g.user, "token_type": "Bearer", "expires_in": int(m.TTL.Seconds()), "id_token": id})
}

// Token signs claims; iss, iat and exp are filled in when missing.
func (m *IDP) Token(claims map[string]any) string {
	now := time.Now()
	c := map[string]any{"iss": m.URL, "iat": now.Unix(), "exp": now.Add(m.TTL).Unix()}
	for k, v := range claims {
		c[k] = v
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: m.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		panic(err)
	}
	raw, err := jwt.Signed(sig).Claims(c).Serialize()
	if err != nil {
		panic(err)
	}
	return raw
}

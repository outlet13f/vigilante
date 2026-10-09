package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"vigilante/internal/config"
)

func TestRoleScopeMatrix(t *testing.T) {
	order := Service{Name: "order-api", Team: "payments"}
	web := Service{Name: "web", Team: "frontend"}
	p := &Principal{Bindings: []Binding{
		{Role: Deployer, Scope: Scope{Kind: "team", Value: "payments"}},
		{Role: Viewer, Scope: Scope{Kind: "all"}},
		{Role: Operator, Scope: Scope{Kind: "service", Value: "web"}},
	}}
	cases := []struct {
		a    Action
		s    Service
		want bool
	}{
		{ActRead, web, true}, {ActRead, order, true},
		{ActDeploy, order, true}, {ActDeploy, Service{Name: "batch", Team: "data"}, false},
		{ActRollback, order, false}, {ActRollback, web, true}, {ActApprove, web, true},
		{ActCircuit, Service{}, false}, {ActAgent, order, false},
		{ActDeploy, Service{}, false}, // global actions need an all-scope grant
	}
	for _, c := range cases {
		if got := p.Can(c.a, c.s); got != c.want {
			t.Errorf("Can(%s, %+v) = %v, want %v", c.a, c.s, got, c.want)
		}
	}
	agent := &Principal{Bindings: []Binding{{Role: Agent, Scope: Scope{Kind: "all"}}}}
	if !agent.Can(ActAgent, order) || agent.Can(ActRead, order) {
		t.Fatal("agent role is limited to agent actions")
	}
	admin := &Principal{Bindings: []Binding{{Role: Admin, Scope: Scope{Kind: "all"}}}}
	if !admin.Can(ActCircuit, Service{}) || !admin.Can(ActAgent, order) {
		t.Fatal("admin@* may do everything")
	}
}

func TestServiceAccountsAndLegacy(t *testing.T) {
	tok, sha, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	oldTok, oldSha, _ := NewToken()
	a, err := New(context.Background(), config.Auth{ServiceAccounts: []config.ServiceAccount{
		{Name: "ci-order", TokenSHA256: sha, Expires: "2026-12-31", Roles: []config.Grant{{Role: "deployer", Scope: "service=order-api"}}},
		{Name: "retired", TokenSHA256: oldSha, Expires: "2026-01-01", Roles: []config.Grant{{Role: "admin"}}},
	}}, "break-glass")
	if err != nil {
		t.Fatal(err)
	}
	a.Now = func() time.Time { return time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC) }
	p, err := a.AuthenticateToken(context.Background(), tok)
	if err != nil || p.ID != "sa:ci-order" || !p.Can(ActDeploy, Service{Name: "order-api"}) || p.Can(ActDeploy, Service{Name: "web"}) {
		t.Fatalf("service account: %+v %v", p, err)
	}
	if _, err := a.AuthenticateToken(context.Background(), oldTok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token accepted: %v", err)
	}
	if _, err := a.AuthenticateToken(context.Background(), "vgl_made-up"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("unknown token accepted")
	}
	if p, err := a.AuthenticateToken(context.Background(), "break-glass"); err != nil || p.ID != "token:legacy" || !p.Can(ActCircuit, Service{}) {
		t.Fatalf("legacy token: %+v %v", p, err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	if _, err := a.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("missing token must be rejected when auth is configured")
	}
	off, _ := New(context.Background(), config.Auth{}, "")
	if p, err := off.Authenticate(r); err != nil || p != Anonymous || !off.Disabled() {
		t.Fatal("no auth configured = anonymous (dev mode)")
	}
}

// mockIDP is a minimal OIDC provider: discovery document + JWKS + a signer.
type mockIDP struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newIDP(t *testing.T) *mockIDP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockIDP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": m.srv.URL, "jwks_uri": m.srv.URL + "/keys",
			"authorization_endpoint": m.srv.URL + "/auth", "token_endpoint": m.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockIDP) token(t *testing.T, claims map[string]any) string {
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: m.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(sig).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestOIDCGroupsAndUsers(t *testing.T) {
	idp := newIDP(t)
	a, err := New(context.Background(), config.Auth{
		OIDC: &config.OIDC{Issuer: idp.srv.URL, Audience: "vigilante"},
		RoleBindings: []config.RoleBinding{
			{Group: "sre", Role: "operator"},
			{User: "alice", Role: "admin"},
			{Group: "payments-dev", Role: "deployer", Scope: "team=payments"},
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := func(user string, groups []string, aud string, exp time.Time) map[string]any {
		return map[string]any{"iss": idp.srv.URL, "aud": aud, "sub": "id-" + user, "preferred_username": user,
			"groups": groups, "iat": now.Unix(), "exp": exp.Unix()}
	}
	ctx := context.Background()

	p, err := a.AuthenticateToken(ctx, idp.token(t, claims("bob", []string{"sre", "payments-dev"}, "vigilante", now.Add(time.Hour))))
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "user:bob" || !p.Can(ActRollback, Service{Name: "web"}) || p.Can(ActCircuit, Service{}) {
		t.Fatalf("bob: %+v", p)
	}
	p, _ = a.AuthenticateToken(ctx, idp.token(t, claims("alice", nil, "vigilante", now.Add(time.Hour))))
	if !p.Can(ActCircuit, Service{}) {
		t.Fatalf("alice is admin by user binding: %+v", p)
	}
	p, err = a.AuthenticateToken(ctx, idp.token(t, claims("carol", []string{"marketing"}, "vigilante", now.Add(time.Hour))))
	if err != nil || len(p.Bindings) != 0 {
		t.Fatalf("unmapped user authenticates but has no grants: %+v %v", p, err)
	}
	if _, err := a.AuthenticateToken(ctx, idp.token(t, claims("bob", []string{"sre"}, "other-app", now.Add(time.Hour)))); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("token for another audience accepted: %v", err)
	}
	if _, err := a.AuthenticateToken(ctx, idp.token(t, claims("bob", []string{"sre"}, "vigilante", now.Add(-time.Minute)))); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token accepted: %v", err)
	}
	other := newIDP(t) // a token signed by a different key with our issuer must fail
	forged := other.token(t, claims("alice", nil, "vigilante", now.Add(time.Hour)))
	if _, err := a.AuthenticateToken(ctx, forged); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("forged token accepted: %v", err)
	}
}

// Package auth identifies API callers and decides what they may do.
//
// Callers present a bearer token: a service-account token (vgl_…, matched
// by its SHA-256), an OIDC JWT from the company identity provider (groups
// mapped to roles), or the legacy server.auth_token_env token (treated as a
// break-glass admin). Authorization is role × scope: a grant's role must be
// high enough for the action and its scope must cover the service touched.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"vigilante/internal/config"
)

// Role levels; Agent sits outside the viewer..admin ladder.
type Role int

const (
	RoleNone Role = iota
	Agent
	Viewer
	Deployer
	Operator
	Admin
)

var roleNames = map[string]Role{"agent": Agent, "viewer": Viewer, "deployer": Deployer, "operator": Operator, "admin": Admin}

func (r Role) String() string {
	for n, v := range roleNames {
		if v == r {
			return n
		}
	}
	return "none"
}

func ParseRole(s string) (Role, error) {
	r, ok := roleNames[s]
	if !ok {
		return RoleNone, fmt.Errorf("unknown role %q (viewer|deployer|operator|admin|agent)", s)
	}
	return r, nil
}

// Action is what a request wants to do.
type Action string

const (
	ActRead     Action = "read"     // viewer
	ActDeploy   Action = "deploy"   // deployer: create, phases, abort, baselines, mark-good
	ActRollback Action = "rollback" // operator: manual rollback
	ActApprove  Action = "approve"  // operator: approve a gated escalation
	ActCircuit  Action = "circuit"  // admin: circuit reset/trip
	ActAgent    Action = "agent"    // agent (or admin): push samples, heartbeat
)

var needs = map[Action]Role{ActRead: Viewer, ActDeploy: Deployer, ActRollback: Operator, ActApprove: Operator, ActCircuit: Admin, ActAgent: Agent}

// Required is the minimum role for an action.
func Required(a Action) Role { return needs[a] }

func (r Role) allows(a Action) bool {
	need := needs[a]
	if need == Agent {
		return r == Agent || r == Admin
	}
	return r != Agent && r >= need
}

// Scope limits a grant to all services, one team, or one service.
type Scope struct {
	Kind  string // all | team | service
	Value string
}

func (s Scope) String() string {
	if s.Kind == "all" {
		return "*"
	}
	return s.Kind + "=" + s.Value
}

func ParseScope(s string) (Scope, error) {
	if s == "" || s == "*" {
		return Scope{Kind: "all"}, nil
	}
	k, v, ok := strings.Cut(s, "=")
	if !ok || v == "" || (k != "team" && k != "service") {
		return Scope{}, fmt.Errorf("scope %q: use *, team=<team> or service=<name>", s)
	}
	return Scope{Kind: k, Value: v}, nil
}

// Service identifies what a request touches; the zero value means "global"
// (only an all-scope grant covers it).
type Service struct {
	Name string
	Team string
}

func (s Scope) covers(svc Service) bool {
	switch s.Kind {
	case "all":
		return true
	case "team":
		return svc.Team != "" && svc.Team == s.Value
	case "service":
		return svc.Name != "" && svc.Name == s.Value
	}
	return false
}

type Binding struct {
	Role  Role
	Scope Scope
}

func (b Binding) String() string { return b.Role.String() + "@" + b.Scope.String() }

// Principal is an authenticated caller.
type Principal struct {
	ID       string // user:alice | sa:ci-order | token:legacy | webhook:github | anonymous
	Kind     string // user | service | legacy | webhook | anonymous
	Groups   []string
	Bindings []Binding
}

// Can reports whether the principal may do a on svc.
func (p *Principal) Can(a Action, svc Service) bool {
	for _, b := range p.Bindings {
		if b.Role.allows(a) && b.Scope.covers(svc) {
			return true
		}
	}
	return false
}

// CanSomewhere reports whether any grant allows a, in any scope (used for
// endpoints that list or show cross-service information).
func (p *Principal) CanSomewhere(a Action) bool {
	for _, b := range p.Bindings {
		if b.Role.allows(a) {
			return true
		}
	}
	return false
}

var (
	ErrUnauthenticated = errors.New("authentication required")
	ErrInvalidToken    = errors.New("invalid or expired token")
)

type serviceAccount struct {
	name     string
	expires  time.Time // zero = never
	bindings []Binding
}

type oidcBinding struct {
	group, user string
	binding     Binding
}

// Authenticator turns a request's bearer token into a Principal.
type Authenticator struct {
	disabled  bool
	legacy    string
	accounts  map[string]serviceAccount // by sha256 hex
	verifier  *oidc.IDTokenVerifier
	userClaim string
	grpClaim  string
	oidcBinds []oidcBinding
	Now       func() time.Time
}

// Disabled reports that no authentication is configured (development only).
func (a *Authenticator) Disabled() bool { return a.disabled }

// New builds the authenticator. With OIDC configured it fetches the
// provider's discovery document, so it needs the issuer to be reachable.
func New(ctx context.Context, cfg config.Auth, legacyToken string) (*Authenticator, error) {
	a := &Authenticator{legacy: legacyToken, accounts: map[string]serviceAccount{}, Now: time.Now}
	for _, sa := range cfg.ServiceAccounts {
		acc := serviceAccount{name: sa.Name}
		if sa.Expires != "" {
			t, err := time.Parse("2006-01-02", sa.Expires)
			if err != nil {
				return nil, fmt.Errorf("service account %s: expires: %w", sa.Name, err)
			}
			acc.expires = t.Add(24 * time.Hour) // valid through that day
		}
		for _, g := range sa.Roles {
			b, err := grant(g.Role, g.Scope)
			if err != nil {
				return nil, fmt.Errorf("service account %s: %w", sa.Name, err)
			}
			acc.bindings = append(acc.bindings, b)
		}
		a.accounts[strings.ToLower(sa.TokenSHA256)] = acc
	}
	for _, rb := range cfg.RoleBindings {
		b, err := grant(rb.Role, rb.Scope)
		if err != nil {
			return nil, err
		}
		a.oidcBinds = append(a.oidcBinds, oidcBinding{group: rb.Group, user: rb.User, binding: b})
	}
	if o := cfg.OIDC; o != nil {
		provider, err := oidc.NewProvider(ctx, o.Issuer)
		if err != nil {
			return nil, fmt.Errorf("oidc discovery for %s: %w", o.Issuer, err)
		}
		a.verifier = provider.Verifier(&oidc.Config{ClientID: o.Audience, Now: func() time.Time { return a.Now() }})
		a.userClaim, a.grpClaim = o.UsernameClaim, o.GroupsClaim
		if a.userClaim == "" {
			a.userClaim = "preferred_username"
		}
		if a.grpClaim == "" {
			a.grpClaim = "groups"
		}
	}
	a.disabled = legacyToken == "" && len(a.accounts) == 0 && a.verifier == nil
	return a, nil
}

func grant(role, scope string) (Binding, error) {
	r, err := ParseRole(role)
	if err != nil {
		return Binding{}, err
	}
	s, err := ParseScope(scope)
	if err != nil {
		return Binding{}, err
	}
	return Binding{Role: r, Scope: s}, nil
}

// Anonymous is the principal used when auth is disabled: full access, but
// every action is still recorded as anonymous.
var Anonymous = &Principal{ID: "anonymous", Kind: "anonymous", Bindings: []Binding{{Role: Admin, Scope: Scope{Kind: "all"}}}}

// Authenticate identifies the caller of r.
func (a *Authenticator) Authenticate(r *http.Request) (*Principal, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		if a.disabled {
			return Anonymous, nil
		}
		return nil, ErrUnauthenticated
	}
	return a.AuthenticateToken(r.Context(), raw)
}

// AuthenticateToken identifies the holder of a bearer token.
func (a *Authenticator) AuthenticateToken(ctx context.Context, raw string) (*Principal, error) {
	if strings.HasPrefix(raw, TokenPrefix) {
		sum := sha256.Sum256([]byte(raw))
		acc, ok := a.accounts[hex.EncodeToString(sum[:])]
		if !ok || (!acc.expires.IsZero() && a.Now().After(acc.expires)) {
			return nil, ErrInvalidToken
		}
		return &Principal{ID: "sa:" + acc.name, Kind: "service", Bindings: acc.bindings}, nil
	}
	if a.legacy != "" && subtle.ConstantTimeCompare([]byte(raw), []byte(a.legacy)) == 1 {
		return &Principal{ID: "token:legacy", Kind: "legacy", Bindings: []Binding{{Role: Admin, Scope: Scope{Kind: "all"}}}}, nil
	}
	if a.verifier != nil {
		tok, err := a.verifier.Verify(ctx, raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
		}
		var claims map[string]any
		if err := tok.Claims(&claims); err != nil {
			return nil, ErrInvalidToken
		}
		user, _ := claims[a.userClaim].(string)
		if user == "" {
			user = tok.Subject
		}
		p := &Principal{ID: "user:" + user, Kind: "user", Groups: stringList(claims[a.grpClaim])}
		for _, ob := range a.oidcBinds {
			if (ob.user != "" && ob.user == user) || (ob.group != "" && slices.Contains(p.Groups, ob.group)) {
				p.Bindings = append(p.Bindings, ob.binding)
			}
		}
		return p, nil
	}
	return nil, ErrInvalidToken
}

func stringList(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{x}
	}
	return nil
}

// TokenPrefix marks Vigilante service-account tokens.
const TokenPrefix = "vgl_"

// NewToken returns a fresh service-account token and the SHA-256 hex to put
// in the config. The token itself is shown once and never stored.
func NewToken() (token, sha string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

type ctxKey struct{}

// WithPrincipal / FromContext carry the caller through a request.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

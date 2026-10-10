package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"vigilante/internal/auth"
	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/orchestrator"
)

// clientPrincipal turns an API key or OAuth access token into a principal.
func (s *Server) clientPrincipal(_ context.Context, raw string) (*auth.Principal, error) {
	c, scopes, err := s.E.ResolveClientCredential(raw)
	if err != nil {
		return nil, err
	}
	p := &auth.Principal{ID: "client:" + c.Name, Kind: "client", Scopes: scopes, ClientID: c.ID}
	for _, g := range c.Grants {
		if b, err := auth.ParseGrant(g); err == nil {
			p.Bindings = append(p.Bindings, b)
		}
	}
	return p, nil
}

// ---------------------------------------------------------------- OAuth token endpoint

type oauthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

func oauthFail(w http.ResponseWriter, status int, code, desc string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="vigilante"`)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, oauthError{Error: code, Description: desc})
}

// v2Token implements the OAuth 2.0 client credentials grant (RFC 6749 §4.4).
// Errors follow RFC 6749 §5.2, not problem+json, so OAuth libraries work.
func (s *Server) v2Token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthFail(w, 400, "invalid_request", "body must be application/x-www-form-urlencoded")
		return
	}
	id, secret, basic := r.BasicAuth()
	if !basic {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id == "" || secret == "" {
		oauthFail(w, 401, "invalid_client", "client authentication required (HTTP Basic or client_id/client_secret)")
		return
	}
	if gt := r.PostForm.Get("grant_type"); gt != "client_credentials" {
		oauthFail(w, 400, "unsupported_grant_type", "only client_credentials is supported")
		return
	}
	c, err := s.E.CheckClientSecret(id, secret)
	if err != nil {
		s.E.Audit(journal.Entry{Actor: "client-id:" + id, Source: "api", Action: "denied", Reason: "oauth token: invalid client credentials"})
		oauthFail(w, 401, "invalid_client", "unknown client, wrong secret, or the client is revoked or expired")
		return
	}
	scopes := c.Scopes
	if req := strings.Fields(r.PostForm.Get("scope")); len(req) > 0 {
		for _, sc := range req {
			if !slices.Contains(c.Scopes, sc) {
				oauthFail(w, 400, "invalid_scope", fmt.Sprintf("scope %q is not granted to this client", sc))
				return
			}
		}
		scopes = req
	}
	v := s.limits.take(classDefault+"|client:"+c.Name, s.limitFor(&auth.Principal{ClientID: c.ID}, classDefault))
	if !v.ok {
		w.Header().Set("Retry-After", fmt.Sprint(max(int(v.retryAfter.Seconds()), 1)))
		oauthFail(w, 429, "slow_down", "rate limit: "+v.why)
		return
	}
	tok, t, err := s.E.IssueToken(c, scopes, s.E.Cfg.API.TokenTTL)
	if err != nil {
		oauthFail(w, 500, "server_error", err.Error())
		return
	}
	s.E.Audit(journal.Entry{Actor: "client:" + c.Name, Source: "api", Action: "oauth.token", Reason: strings.Join(scopes, " ")})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, 200, map[string]any{"access_token": tok, "token_type": "Bearer",
		"expires_in": int(time.Until(t.ExpiresAt).Round(time.Second).Seconds()), "scope": strings.Join(scopes, " ")})
}

// ---------------------------------------------------------------- API client management

type clientV2 struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Type        string           `json:"type"`
	Description string           `json:"description,omitempty"`
	Scopes      []string         `json:"scopes"`
	Grants      []string         `json:"grants"`
	SecretHint  string           `json:"secret_hint"`
	RateLimit   *model.RateLimit `json:"rate_limit,omitempty"`
	ExpiresAt   *time.Time       `json:"expires_at,omitempty"`
	CreatedBy   string           `json:"created_by,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	RotatedAt   *time.Time       `json:"rotated_at,omitempty"`
	RevokedAt   *time.Time       `json:"revoked_at,omitempty"`
	LastUsedAt  *time.Time       `json:"last_used_at,omitempty"`
	Status      string           `json:"status"` // active | expired | revoked
}

func toClientV2(c *model.APIClient) clientV2 {
	st := "active"
	switch {
	case c.RevokedAt != nil:
		st = "revoked"
	case !c.Active(time.Now()):
		st = "expired"
	}
	return clientV2{ID: c.ID, Name: c.Name, Type: c.Type, Description: c.Description, Scopes: c.Scopes, Grants: c.Grants,
		SecretHint: c.SecretHint, RateLimit: c.RateLimit, ExpiresAt: c.ExpiresAt, CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt,
		RotatedAt: c.RotatedAt, RevokedAt: c.RevokedAt, LastUsedAt: c.LastUsedAt, Status: st}
}

var clientNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type clientFields struct {
	Name        *string          `json:"name"`
	Type        *string          `json:"type"`
	Description *string          `json:"description"`
	Scopes      *[]string        `json:"scopes"`
	Grants      *[]string        `json:"grants"`
	ExpiresAt   *time.Time       `json:"expires_at"`
	RateLimit   *model.RateLimit `json:"rate_limit"`
}

// validate checks the fields that are set; create=true also requires the
// mandatory ones.
func (s *Server) validateClient(f clientFields, create bool) []fieldError {
	var errs []fieldError
	if create {
		if f.Name == nil || !clientNameRe.MatchString(*f.Name) {
			errs = append(errs, fieldError{"name", "1-64 of a-z, 0-9, - and _, starting with a letter or digit"})
		}
		if f.Type == nil || (*f.Type != model.ClientOAuth && *f.Type != model.ClientAPIKey) {
			errs = append(errs, fieldError{"type", "oauth or api_key"})
		}
		if f.Scopes == nil {
			errs = append(errs, fieldError{"scopes", "required"})
		}
		if f.Grants == nil {
			errs = append(errs, fieldError{"grants", "required"})
		}
	} else if f.Name != nil || f.Type != nil {
		errs = append(errs, fieldError{"name", "name and type cannot change; create a new client"})
	}
	if f.Scopes != nil {
		if len(*f.Scopes) == 0 {
			errs = append(errs, fieldError{"scopes", "at least one scope"})
		}
		for _, sc := range *f.Scopes {
			if !slices.Contains(auth.AllScopes, sc) {
				errs = append(errs, fieldError{"scopes", fmt.Sprintf("unknown scope %q (%s)", sc, strings.Join(auth.AllScopes, ", "))})
			}
		}
	}
	if f.Grants != nil {
		if len(*f.Grants) == 0 {
			errs = append(errs, fieldError{"grants", "at least one grant"})
		}
		for _, g := range *f.Grants {
			b, err := auth.ParseGrant(g)
			if err != nil {
				errs = append(errs, fieldError{"grants", err.Error()})
				continue
			}
			if b.Scope.Kind == "service" {
				if _, ok := s.E.Cfg.Service(b.Scope.Value); !ok {
					errs = append(errs, fieldError{"grants", fmt.Sprintf("grant %q: unknown service", g)})
				}
			}
		}
	}
	if f.ExpiresAt != nil && !f.ExpiresAt.After(time.Now()) {
		errs = append(errs, fieldError{"expires_at", "must be in the future"})
	}
	if rl := f.RateLimit; rl != nil && (rl.Rate < 0 || rl.Burst < 0 || rl.Daily < 0 || (rl.Rate > 0 && rl.Burst < 1)) {
		errs = append(errs, fieldError{"rate_limit", "rate, burst and daily >= 0; burst >= 1 when rate > 0"})
	}
	return errs
}

func (s *Server) manage(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	return s.allow(w, r, auth.ActManage, auth.Service{})
}

func (s *Server) v2CreateClient(w http.ResponseWriter, r *http.Request) {
	p, ok := s.manage(w, r)
	if !ok {
		return
	}
	var f clientFields
	if !s.decodeStrict(w, r, &f, false) {
		return
	}
	if errs := s.validateClient(f, true); len(errs) > 0 {
		s.problem(w, r, 422, "validation_failed", "invalid API client", errs...)
		return
	}
	for _, c := range s.E.Clients() {
		if c.Name == *f.Name && c.RevokedAt == nil {
			s.problem(w, r, http.StatusConflict, "conflict", fmt.Sprintf("an active client named %q exists (%s)", c.Name, c.ID))
			return
		}
	}
	c := &model.APIClient{Name: *f.Name, Type: *f.Type, Scopes: *f.Scopes, Grants: *f.Grants, ExpiresAt: f.ExpiresAt,
		RateLimit: f.RateLimit, CreatedBy: p.ID}
	if f.Description != nil {
		c.Description = *f.Description
	}
	secret, err := s.E.CreateClient(c)
	if err != nil {
		s.problem(w, r, 500, "internal", err.Error())
		return
	}
	s.audit(r, "api-client.create", "", "", fmt.Sprintf("%s (%s) scopes=%s grants=%s", c.Name, c.ID, strings.Join(c.Scopes, " "), strings.Join(c.Grants, " ")))
	w.Header().Set("Location", "/v2/api-clients/"+c.ID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"client": toClientV2(c), "secret": secret})
}

func (s *Server) v2ListClients(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.manage(w, r); !ok {
		return
	}
	pr, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	items := []clientV2{}
	for _, c := range s.E.Clients() {
		items = append(items, toClientV2(c))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name+"|"+items[i].ID < items[j].Name+"|"+items[j].ID })
	out, next := paginate(items, func(c clientV2) string { return c.Name + "|" + c.ID }, false, pr)
	if out == nil {
		out = []clientV2{}
	}
	writeJSON(w, 200, page[clientV2]{Items: out, NextCursor: next})
}

func (s *Server) v2GetClient(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.manage(w, r); !ok {
		return
	}
	c, ok := s.E.Client(r.PathValue("id"))
	if !ok {
		s.problem(w, r, 404, "not_found", fmt.Sprintf("API client %q not found", r.PathValue("id")))
		return
	}
	writeJSON(w, 200, toClientV2(c))
}

func (s *Server) v2UpdateClient(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.manage(w, r); !ok {
		return
	}
	var f clientFields
	if !s.decodeStrict(w, r, &f, false) {
		return
	}
	if errs := s.validateClient(f, false); len(errs) > 0 {
		s.problem(w, r, 422, "validation_failed", "invalid API client change", errs...)
		return
	}
	c, err := s.E.UpdateClient(r.PathValue("id"), func(c *model.APIClient) error {
		if c.RevokedAt != nil {
			return errors.New("client is revoked")
		}
		if f.Description != nil {
			c.Description = *f.Description
		}
		if f.Scopes != nil {
			c.Scopes = *f.Scopes
		}
		if f.Grants != nil {
			c.Grants = *f.Grants
		}
		if f.ExpiresAt != nil {
			c.ExpiresAt = f.ExpiresAt
		}
		if f.RateLimit != nil {
			c.RateLimit = f.RateLimit
		}
		return nil
	})
	if !s.clientErr(w, r, err) {
		return
	}
	s.audit(r, "api-client.update", "", "", fmt.Sprintf("%s (%s) scopes=%s grants=%s", c.Name, c.ID, strings.Join(c.Scopes, " "), strings.Join(c.Grants, " ")))
	writeJSON(w, 200, toClientV2(c))
}

func (s *Server) clientErr(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, orchestrator.ErrClientNotFound):
		s.problem(w, r, 404, "not_found", fmt.Sprintf("API client %q not found", r.PathValue("id")))
	default:
		s.problem(w, r, http.StatusConflict, "conflict", err.Error())
	}
	return false
}

func (s *Server) v2RotateClient(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.manage(w, r); !ok {
		return
	}
	secret, c, err := s.E.RotateClientSecret(r.PathValue("id"))
	if !s.clientErr(w, r, err) {
		return
	}
	s.audit(r, "api-client.rotate", "", "", c.Name+" ("+c.ID+")")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"client": toClientV2(c), "secret": secret})
}

func (s *Server) v2RevokeClient(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.manage(w, r); !ok {
		return
	}
	c, err := s.E.RevokeClient(r.PathValue("id"))
	if !s.clientErr(w, r, err) {
		return
	}
	s.audit(r, "api-client.revoke", "", "", c.Name+" ("+c.ID+")")
	writeJSON(w, 200, toClientV2(c))
}

// auditAllowed: audit spans services, so it needs viewer on * and, for API
// clients, the audit:read scope (not deployments:read).
func (s *Server) auditAllowed(w http.ResponseWriter, r *http.Request) bool {
	p := auth.FromContext(r.Context())
	if p != nil && p.HasScope(auth.ScopeAuditRead) {
		rp := *p
		rp.Scopes = nil // the role check below; the scope is checked above
		if rp.Can(auth.ActRead, auth.Service{}) {
			return true
		}
	}
	err := fmt.Errorf("forbidden: %s needs role viewer on all services (*) and, for API clients, scope %s", principalID(p), auth.ScopeAuditRead)
	s.denied(r, "", err)
	s.fail(w, r, http.StatusForbidden, "forbidden", err)
	return false
}

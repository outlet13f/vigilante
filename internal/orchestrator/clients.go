package orchestrator

import (
	"crypto/subtle"
	"errors"
	"slices"
	"sort"
	"time"

	"vigilante/internal/auth"
	"vigilante/internal/journal"
	"vigilante/internal/model"
)

var (
	ErrClientNotFound = errors.New("API client not found")
	ErrBadCredentials = errors.New("invalid client credentials")
)

// lastUsedEvery bounds how often a client's last-used time is persisted.
const lastUsedEvery = time.Hour

func (e *Engine) saveClient(c *model.APIClient) {
	cp := *c
	e.record(journal.Entry{Kind: journal.KindAPIClient, Client: &cp})
}

// CreateClient registers a client and returns its secret, shown only once.
func (e *Engine) CreateClient(c *model.APIClient) (string, error) {
	prefix := auth.ClientSecretPrefix
	if c.Type == model.ClientAPIKey {
		prefix = auth.APIKeyPrefix
	}
	secret, sha, err := auth.NewSecret(prefix)
	if err != nil {
		return "", err
	}
	c.ID = newID("cli_")
	c.SecretSHA256, c.SecretHint = sha, secret[:8]
	c.CreatedAt = time.Now().UTC()
	e.clientMu.Lock()
	defer e.clientMu.Unlock()
	e.mu.Lock()
	e.clients[c.ID] = c
	e.mu.Unlock()
	e.saveClient(c)
	return secret, nil
}

// Clients lists clients by name.
func (e *Engine) Clients() []*model.APIClient {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*model.APIClient, 0, len(e.clients))
	for _, c := range e.clients {
		cp := *c
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Client returns a copy of one client.
func (e *Engine) Client(id string) (*model.APIClient, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.clients[id]
	if !ok {
		return nil, false
	}
	cp := *c
	return &cp, true
}

// UpdateClient applies fn to a client and stores the result.
func (e *Engine) UpdateClient(id string, fn func(*model.APIClient) error) (*model.APIClient, error) {
	// Snapshots are written in the order they are made, so replay ends on
	// the latest one.
	e.clientMu.Lock()
	defer e.clientMu.Unlock()
	e.mu.Lock()
	c, ok := e.clients[id]
	if !ok {
		e.mu.Unlock()
		return nil, ErrClientNotFound
	}
	cp := *c
	if err := fn(&cp); err != nil {
		e.mu.Unlock()
		return nil, err
	}
	e.clients[id] = &cp
	e.mu.Unlock()
	e.saveClient(&cp)
	out := cp
	return &out, nil
}

// RotateClientSecret replaces the secret; the old one stops working at
// once, and so do access tokens issued with it.
func (e *Engine) RotateClientSecret(id string) (string, *model.APIClient, error) {
	var secret string
	c, err := e.UpdateClient(id, func(c *model.APIClient) error {
		if c.RevokedAt != nil {
			return errors.New("client is revoked")
		}
		prefix := auth.ClientSecretPrefix
		if c.Type == model.ClientAPIKey {
			prefix = auth.APIKeyPrefix
		}
		s, sha, err := auth.NewSecret(prefix)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		secret, c.SecretSHA256, c.SecretHint, c.RotatedAt = s, sha, s[:8], &now
		return nil
	})
	if err == nil {
		e.dropTokens(id)
	}
	return secret, c, err
}

// RevokeClient disables a client and its tokens for good.
func (e *Engine) RevokeClient(id string) (*model.APIClient, error) {
	c, err := e.UpdateClient(id, func(c *model.APIClient) error {
		if c.RevokedAt == nil {
			now := time.Now().UTC()
			c.RevokedAt = &now
		}
		return nil
	})
	if err == nil {
		e.dropTokens(id)
	}
	return c, err
}

func (e *Engine) dropTokens(clientID string) {
	e.mu.Lock()
	for h, t := range e.tokens {
		if t.ClientID == clientID {
			delete(e.tokens, h)
		}
	}
	e.mu.Unlock()
}

// CheckClientSecret authenticates an OAuth client at the token endpoint.
func (e *Engine) CheckClientSecret(id, secret string) (*model.APIClient, error) {
	c, ok := e.Client(id)
	if !ok || c.Type != model.ClientOAuth || !c.Active(time.Now()) ||
		subtle.ConstantTimeCompare([]byte(auth.HashSecret(secret)), []byte(c.SecretSHA256)) != 1 {
		return nil, ErrBadCredentials
	}
	return c, nil
}

// IssueToken creates an access token for a client, stored as its hash.
// Issued tokens are dropped when the client's secret rotates or it is revoked.
func (e *Engine) IssueToken(c *model.APIClient, scopes []string, ttl time.Duration) (string, *model.AccessToken, error) {
	raw, sha, err := auth.NewSecret(auth.AccessTokenPrefix)
	if err != nil {
		return "", nil, err
	}
	exp := time.Now().Add(ttl).UTC()
	if c.ExpiresAt != nil && c.ExpiresAt.Before(exp) {
		exp = *c.ExpiresAt
	}
	t := &model.AccessToken{SHA256: sha, ClientID: c.ID, Scopes: scopes, ExpiresAt: exp}
	e.mu.Lock()
	e.tokens[sha] = t
	for h, old := range e.tokens {
		if time.Now().After(old.ExpiresAt) {
			delete(e.tokens, h)
		}
	}
	e.mu.Unlock()
	cp := *t
	e.record(journal.Entry{Kind: journal.KindAccessToken, Token: &cp})
	return raw, t, nil
}

// ResolveClientCredential maps an API key or access token to its client and
// the scopes it carries.
func (e *Engine) ResolveClientCredential(raw string) (*model.APIClient, []string, error) {
	sha := auth.HashSecret(raw)
	now := time.Now()
	e.mu.Lock()
	var c *model.APIClient
	var scopes []string
	if t, ok := e.tokens[sha]; ok && now.Before(t.ExpiresAt) {
		// Narrowing a client's scopes also narrows tokens already issued.
		if c = e.clients[t.ClientID]; c != nil {
			for _, sc := range t.Scopes {
				if slices.Contains(c.Scopes, sc) {
					scopes = append(scopes, sc)
				}
			}
		}
	} else if !ok {
		for _, cand := range e.clients {
			if cand.Type == model.ClientAPIKey && subtle.ConstantTimeCompare([]byte(cand.SecretSHA256), []byte(sha)) == 1 {
				c, scopes = cand, cand.Scopes
				break
			}
		}
	}
	if c == nil || !c.Active(now) {
		e.mu.Unlock()
		return nil, nil, auth.ErrInvalidToken
	}
	var used *time.Time
	if c.LastUsedAt == nil || now.Sub(*c.LastUsedAt) > lastUsedEvery {
		t := now.UTC()
		c.LastUsedAt, used = &t, &t
	}
	cp := *c
	scopes = append([]string{}, scopes...) // never nil: a client is always scope-limited, even to nothing
	e.mu.Unlock()
	if used != nil { // at most hourly per client; written before answering so nothing outlives the request
		e.record(journal.Entry{Kind: journal.KindClientUsed, Message: cp.ID, Time: *used})
	}
	return &cp, scopes, nil
}

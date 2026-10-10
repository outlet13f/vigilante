package model

import "time"

// APIClient is an integration registered through the API: an OAuth client
// (client credentials grant) or a static API key. Only the SHA-256 of its
// secret is kept.
type APIClient struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Type         string     `json:"type"` // oauth | api_key
	Description  string     `json:"description,omitempty"`
	Scopes       []string   `json:"scopes"`
	Grants       []string   `json:"grants"` // role@scope, e.g. deployer@team=payments
	SecretSHA256 string     `json:"secret_sha256"`
	SecretHint   string     `json:"secret_hint"` // first characters, to tell keys apart
	RateLimit    *RateLimit `json:"rate_limit,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	CreatedBy    string     `json:"created_by,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	RotatedAt    *time.Time `json:"rotated_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

// Active reports whether the client may authenticate at t.
func (c *APIClient) Active(t time.Time) bool {
	return c.RevokedAt == nil && (c.ExpiresAt == nil || t.Before(*c.ExpiresAt))
}

const (
	ClientOAuth  = "oauth"
	ClientAPIKey = "api_key"
)

// RateLimit is a token bucket: Rate requests per second on average, bursts
// up to Burst, and at most Daily requests per UTC day (0 = no daily cap).
type RateLimit struct {
	Rate  float64 `json:"rate" yaml:"rate"`
	Burst int     `json:"burst" yaml:"burst"`
	Daily int     `json:"daily,omitempty" yaml:"daily"`
}

// AccessToken is an issued OAuth access token (stored by hash).
type AccessToken struct {
	SHA256    string    `json:"sha256"`
	ClientID  string    `json:"client_id"`
	Scopes    []string  `json:"scopes"`
	ExpiresAt time.Time `json:"expires_at"`
}

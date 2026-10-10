// Package console serves the operations console: a static single-page UI
// (embedded, no external assets) that talks only to the public v2 API.
//
// Sign-in uses the company identity provider (OIDC authorization code with
// PKCE). The session is an encrypted, authenticated cookie holding the ID
// token, so any node of an HA cluster can serve it when they share
// console.session_key_ref; changing requests must echo a CSRF token
// (double-submit cookie). Without OIDC the console asks for an API token,
// kept only in the browser tab.
package console

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"vigilante/internal/config"
	"vigilante/internal/secrets"
)

//go:embed static
var static embed.FS

const (
	sessionCookie = "vgl_session"
	csrfCookie    = "vgl_csrf"
	flowCookie    = "vgl_oauth"
	// CSRFHeader must carry the csrf cookie's value on changing requests.
	CSRFHeader = "X-CSRF-Token"
	maxSession = 12 * time.Hour
)

// Verify checks a token the way the API does (the console never trusts a
// token the API would not).
type Verify func(ctx context.Context, rawToken string) error

type Console struct {
	log    *slog.Logger
	aead   cipher.AEAD
	oauth  *oauth2.Config // nil = token sign-in
	verify Verify
	secure bool
	// hsts: the console is reached over HTTPS (server.tls, or an https
	// console.redirect_url behind a TLS proxy).
	hsts bool
	Now  func() time.Time
}

// hstsValue is sent on console responses over HTTPS. No includeSubDomains:
// sibling hosts of the console may still serve plain HTTP.
const hstsValue = "max-age=31536000"

// New builds the console. OIDC sign-in is used when auth.oidc is set and
// console.redirect_url is given.
func New(ctx context.Context, cfg *config.Config, verify Verify, log *slog.Logger) (*Console, error) {
	c := &Console{log: log, verify: verify, Now: time.Now}
	cc := cfg.Console
	c.hsts = cfg.Server.TLS != nil || strings.HasPrefix(cc.RedirectURL, "https://")
	key := make([]byte, 32)
	if cc.SessionKeyRef != "" {
		v, err := secrets.Resolve(ctx, cc.SessionKeyRef)
		if err != nil {
			return nil, fmt.Errorf("console.session_key_ref: %w", err)
		}
		if len(v) < 32 {
			return nil, errors.New("console.session_key_ref must be at least 32 characters")
		}
		sum := sha256.Sum256([]byte(v))
		key = sum[:]
	} else {
		_, _ = rand.Read(key)
		if cfg.Auth.OIDC != nil {
			log.Warn("console.session_key_ref is not set: console sessions end when this process restarts and are not shared between HA nodes")
		}
	}
	block, _ := aes.NewCipher(key)
	c.aead, _ = cipher.NewGCM(block)
	if o := cfg.Auth.OIDC; o != nil && cc.RedirectURL != "" {
		provider, err := oidc.NewProvider(ctx, o.Issuer)
		if err != nil {
			return nil, fmt.Errorf("console oidc discovery: %w", err)
		}
		secret := ""
		if cc.ClientSecretRef != "" {
			if secret, err = secrets.Resolve(ctx, cc.ClientSecretRef); err != nil {
				return nil, fmt.Errorf("console.client_secret_ref: %w", err)
			}
		}
		clientID := cc.ClientID
		if clientID == "" {
			clientID = o.Audience
		}
		scopes := cc.Scopes
		if len(scopes) == 0 {
			scopes = []string{oidc.ScopeOpenID, "profile", "email"}
		}
		c.oauth = &oauth2.Config{ClientID: clientID, ClientSecret: secret, Endpoint: provider.Endpoint(), RedirectURL: cc.RedirectURL, Scopes: scopes}
		c.secure = strings.HasPrefix(cc.RedirectURL, "https://")
	}
	return c, nil
}

// Register mounts the console under /console/.
func (c *Console) Register(mux *http.ServeMux) {
	sub, _ := fs.Sub(static, "static")
	files := http.FileServer(http.FS(sub))
	handle := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			c.securityHeaders(w, r)
			h(w, r)
		})
	}
	handle("GET /console/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/console/" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.StripPrefix("/console/", files).ServeHTTP(w, r)
	})
	handle("GET /console", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console/", http.StatusFound)
	})
	handle("GET /console/auth/mode", func(w http.ResponseWriter, r *http.Request) {
		mode := "token"
		if c.oauth != nil {
			mode = "oidc"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"mode": mode, "signed_in": c.hasSession(r)})
	})
	handle("GET /console/auth/login", c.login)
	handle("GET /console/auth/callback", c.callback)
	handle("POST /console/auth/logout", c.logout)
}

// securityHeaders goes on every console response; HSTS only when the
// console is served over HTTPS (browsers ignore it over plain HTTP anyway).
func (c *Console) securityHeaders(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if c.hsts || r.TLS != nil {
		w.Header().Set("Strict-Transport-Security", hstsValue)
	}
}

func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
}

// ---------------------------------------------------------------- sealed cookies

func (c *Console) seal(v any) string {
	plain, _ := json.Marshal(v)
	nonce := make([]byte, c.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(c.aead.Seal(nonce, nonce, plain, nil))
}

func (c *Console) open(s string, v any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) < c.aead.NonceSize() {
		return false
	}
	plain, err := c.aead.Open(nil, raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():], nil)
	return err == nil && json.Unmarshal(plain, v) == nil
}

type session struct {
	Token   string `json:"t"`
	Expires int64  `json:"x"`
	CSRF    string `json:"c"`
}

func (c *Console) setCookie(w http.ResponseWriter, name, value string, maxAge int, httpOnly bool, same http.SameSite) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: httpOnly, Secure: c.secure, SameSite: same})
}

func (c *Console) session(r *http.Request) (*session, bool) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, false
	}
	var s session
	if !c.open(ck.Value, &s) || c.Now().Unix() >= s.Expires {
		return nil, false
	}
	return &s, true
}

func (c *Console) hasSession(r *http.Request) bool {
	_, ok := c.session(r)
	return ok
}

// SessionToken returns the bearer token of a console session cookie. For
// changing requests it also checks the CSRF token; ok is false without a
// session, csrfOK is false when the check fails.
func (c *Console) SessionToken(r *http.Request) (token string, ok, csrfOK bool) {
	s, ok := c.session(r)
	if !ok {
		return "", false, false
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return s.Token, true, true
	}
	h := r.Header.Get(CSRFHeader)
	return s.Token, true, h != "" && subtle.ConstantTimeCompare([]byte(h), []byte(s.CSRF)) == 1
}

// ---------------------------------------------------------------- OIDC sign-in

type flow struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Expires  int64  `json:"x"`
}

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (c *Console) login(w http.ResponseWriter, r *http.Request) {
	if c.oauth == nil {
		http.Error(w, "OIDC sign-in is not configured (auth.oidc and console.redirect_url); use an API token", http.StatusNotFound)
		return
	}
	f := flow{State: randomString(), Verifier: oauth2.GenerateVerifier(), Expires: c.Now().Add(10 * time.Minute).Unix()}
	c.setCookie(w, flowCookie, c.seal(f), 600, true, http.SameSiteLaxMode)
	http.Redirect(w, r, c.oauth.AuthCodeURL(f.State, oauth2.S256ChallengeOption(f.Verifier)), http.StatusFound)
}

func (c *Console) callback(w http.ResponseWriter, r *http.Request) {
	if c.oauth == nil {
		http.NotFound(w, r)
		return
	}
	fail := func(code int, msg string) {
		c.log.Warn("console sign-in failed", "reason", msg)
		securityHeaders(w)
		http.Error(w, "Sign-in failed: "+msg, code)
	}
	ck, err := r.Cookie(flowCookie)
	var f flow
	if err != nil || !c.open(ck.Value, &f) || c.Now().Unix() > f.Expires {
		fail(http.StatusBadRequest, "the sign-in attempt expired; start again")
		return
	}
	c.setCookie(w, flowCookie, "", -1, true, http.SameSiteLaxMode)
	if e := r.URL.Query().Get("error"); e != "" {
		fail(http.StatusUnauthorized, "identity provider: "+e+" "+r.URL.Query().Get("error_description"))
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(f.State)) != 1 {
		fail(http.StatusBadRequest, "state mismatch")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tok, err := c.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		fail(http.StatusUnauthorized, "code exchange: "+err.Error())
		return
	}
	idToken, _ := tok.Extra("id_token").(string)
	if idToken == "" {
		fail(http.StatusUnauthorized, "no ID token in the response")
		return
	}
	if err := c.verify(ctx, idToken); err != nil {
		fail(http.StatusForbidden, err.Error())
		return
	}
	exp := c.Now().Add(maxSession)
	if e := tokenExpiry(idToken); !e.IsZero() && e.Before(exp) {
		exp = e
	}
	csrf := randomString()
	maxAge := int(time.Until(exp).Seconds())
	c.setCookie(w, sessionCookie, c.seal(session{Token: idToken, Expires: exp.Unix(), CSRF: csrf}), maxAge, true, http.SameSiteLaxMode)
	c.setCookie(w, csrfCookie, csrf, maxAge, false, http.SameSiteStrictMode)
	http.Redirect(w, r, "/console/", http.StatusFound)
}

func (c *Console) logout(w http.ResponseWriter, r *http.Request) {
	if _, ok, csrfOK := c.SessionToken(r); ok && !csrfOK {
		http.Error(w, "missing or wrong "+CSRFHeader, http.StatusForbidden)
		return
	}
	c.setCookie(w, sessionCookie, "", -1, true, http.SameSiteLaxMode)
	c.setCookie(w, csrfCookie, "", -1, false, http.SameSiteStrictMode)
	w.WriteHeader(http.StatusNoContent)
}

// tokenExpiry reads exp from a JWT already verified elsewhere.
func tokenExpiry(jwt string) time.Time {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"vigilante/internal/auth/oidctest"
	"vigilante/internal/config"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type rig struct {
	c      *Console
	url    string
	client *http.Client
}

// newRig serves a console (plus /probe, which reports SessionToken) and a
// browser-like client that follows redirects and keeps cookies.
func newRig(t *testing.T, idp *oidctest.IDP, verify Verify) *rig {
	t.Setenv("VGL_TEST_SESSION_KEY", strings.Repeat("k", 32))
	hs := httptest.NewUnstartedServer(nil)
	base := "http://" + hs.Listener.Addr().String()
	cfg := &config.Config{Console: config.Console{SessionKeyRef: "env:VGL_TEST_SESSION_KEY"}}
	if idp != nil {
		cfg.Auth.OIDC = &config.OIDC{Issuer: idp.URL, Audience: "vigilante"}
		cfg.Console.RedirectURL = base + "/console/auth/callback"
	}
	if verify == nil {
		verify = func(context.Context, string) error { return nil }
	}
	c, err := New(context.Background(), cfg, verify, quiet)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	c.Register(mux)
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		tok, ok, csrfOK := c.SessionToken(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "ok": ok, "csrf_ok": csrfOK})
	})
	hs.Config.Handler = mux
	hs.Start()
	t.Cleanup(hs.Close)
	jar, _ := cookiejar.New(nil)
	return &rig{c: c, url: base, client: &http.Client{Jar: jar}}
}

func (g *rig) cookie(name string) string {
	u, _ := url.Parse(g.url)
	for _, ck := range g.client.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

func (g *rig) probe(t *testing.T, method string, hdr map[string]string) map[string]any {
	req, _ := http.NewRequest(method, g.url+"/probe", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func TestOIDCSignInSessionAndCSRF(t *testing.T) {
	idp := oidctest.New(t)
	var verified string
	g := newRig(t, idp, func(_ context.Context, raw string) error { verified = raw; return nil })

	resp, err := g.client.Get(g.url + "/console/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Request.URL.Path != "/console/" || !strings.Contains(string(body), "Vigilante") {
		t.Fatalf("sign-in should end on the console: %d %s", resp.StatusCode, resp.Request.URL)
	}
	if idp.Exchanges != 1 || verified == "" {
		t.Fatalf("code exchanged %d times, verified %q", idp.Exchanges, verified)
	}
	if g.cookie(flowCookie) != "" {
		t.Fatal("the PKCE flow cookie must be cleared after the callback")
	}
	csrf := g.cookie(csrfCookie)
	if csrf == "" || g.cookie(sessionCookie) == "" {
		t.Fatal("session and csrf cookies expected")
	}
	if strings.Contains(g.cookie(sessionCookie), verified[:20]) {
		t.Fatal("the session cookie must be encrypted, not carry the token in clear")
	}

	if p := g.probe(t, "GET", nil); p["ok"] != true || p["csrf_ok"] != true || p["token"] != verified {
		t.Fatalf("GET with session: %v", p)
	}
	if p := g.probe(t, "POST", nil); p["ok"] != true || p["csrf_ok"] != false {
		t.Fatalf("POST without CSRF header must fail the check: %v", p)
	}
	if p := g.probe(t, "POST", map[string]string{CSRFHeader: "nope"}); p["csrf_ok"] != false {
		t.Fatalf("wrong CSRF header accepted: %v", p)
	}
	if p := g.probe(t, "POST", map[string]string{CSRFHeader: csrf}); p["csrf_ok"] != true {
		t.Fatalf("matching CSRF header refused: %v", p)
	}

	var mode map[string]any
	r2, _ := g.client.Get(g.url + "/console/auth/mode")
	_ = json.NewDecoder(r2.Body).Decode(&mode)
	r2.Body.Close()
	if mode["mode"] != "oidc" || mode["signed_in"] != true {
		t.Fatalf("mode %v", mode)
	}

	// Sessions end with the ID token (here: when the clock passes it).
	g.c.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if p := g.probe(t, "GET", nil); p["ok"] != false {
		t.Fatalf("expired session still accepted: %v", p)
	}
	g.c.Now = time.Now

	// Logout needs the CSRF header, then clears both cookies.
	req, _ := http.NewRequest("POST", g.url+"/console/auth/logout", nil)
	if r, _ := g.client.Do(req); r.StatusCode != http.StatusForbidden {
		t.Fatalf("logout without CSRF: %d", r.StatusCode)
	}
	req, _ = http.NewRequest("POST", g.url+"/console/auth/logout", nil)
	req.Header.Set(CSRFHeader, csrf)
	if r, _ := g.client.Do(req); r.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if g.cookie(sessionCookie) != "" || g.probe(t, "GET", nil)["ok"] != false {
		t.Fatal("session survived logout")
	}
}

func TestCallbackRejections(t *testing.T) {
	idp := oidctest.New(t)
	refuse := errors.New("no role binding")
	verify := func(context.Context, string) error { return nil }
	g := newRig(t, idp, func(ctx context.Context, raw string) error { return verify(ctx, raw) })
	noRedirect := &http.Client{Jar: g.client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// Without the flow cookie (an attacker-crafted link) the callback refuses.
	r, _ := http.Get(g.url + "/console/auth/callback?code=x&state=y")
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback without flow cookie: %d", r.StatusCode)
	}

	// A state that does not match the flow cookie is refused.
	r, _ = noRedirect.Get(g.url + "/console/auth/login")
	loc, _ := url.Parse(r.Header.Get("Location"))
	if loc.Query().Get("code_challenge_method") != "S256" || loc.Query().Get("code_challenge") == "" {
		t.Fatalf("authorization request without PKCE: %s", loc)
	}
	r, _ = noRedirect.Get(g.url + "/console/auth/callback?code=x&state=forged")
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("state mismatch: %d", r.StatusCode)
	}

	// A user the API would not accept gets no session.
	verify = func(context.Context, string) error { return refuse }
	r, _ = g.client.Get(g.url + "/console/auth/login")
	if r.StatusCode != http.StatusForbidden || g.cookie(sessionCookie) != "" {
		t.Fatalf("refused user: %d session=%q", r.StatusCode, g.cookie(sessionCookie))
	}

	// A tampered session cookie is ignored.
	u, _ := url.Parse(g.url)
	g.client.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: "AAAA" + strings.Repeat("x", 60), Path: "/"}})
	if p := g.probe(t, "GET", nil); p["ok"] != false {
		t.Fatalf("tampered cookie accepted: %v", p)
	}
}

func TestStaticFilesAndTokenMode(t *testing.T) {
	g := newRig(t, nil, nil)
	r, err := http.Get(g.url + "/console/")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	csp := r.Header.Get("Content-Security-Policy")
	if r.StatusCode != 200 || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") ||
		r.Header.Get("X-Frame-Options") != "DENY" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("index: %d %v", r.StatusCode, r.Header)
	}
	r, _ = http.Get(g.url + "/console/app.js")
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("app.js: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if r, _ := noRedirect.Get(g.url + "/console"); r.StatusCode != http.StatusFound || r.Header.Get("Location") != "/console/" {
		t.Fatalf("/console redirect: %d %s", r.StatusCode, r.Header.Get("Location"))
	}

	var mode map[string]any
	r, _ = http.Get(g.url + "/console/auth/mode")
	_ = json.NewDecoder(r.Body).Decode(&mode)
	r.Body.Close()
	if mode["mode"] != "token" {
		t.Fatalf("without OIDC the console asks for a token: %v", mode)
	}
	if r, _ := http.Get(g.url + "/console/auth/login"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("login without OIDC: %d", r.StatusCode)
	}
}

// HSTS goes on every console response when the console is reached over
// HTTPS: server.tls, an https redirect_url (TLS proxy), or a TLS request.
func TestHSTSOverHTTPS(t *testing.T) {
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	serve := func(t *testing.T, cfg *config.Config, tlsServer bool) (*http.Client, string) {
		t.Helper()
		c, err := New(context.Background(), cfg, func(context.Context, string) error { return nil }, quiet)
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		c.Register(mux)
		var hs *httptest.Server
		if tlsServer {
			hs = httptest.NewTLSServer(mux)
		} else {
			hs = httptest.NewServer(mux)
		}
		t.Cleanup(hs.Close)
		client := *hs.Client()
		client.CheckRedirect = noRedirect.CheckRedirect
		return &client, hs.URL
	}
	paths := []struct{ method, path string }{
		{"GET", "/console/"}, {"GET", "/console/app.js"}, {"GET", "/console"}, {"GET", "/console/auth/mode"},
		{"GET", "/console/auth/login"}, {"GET", "/console/auth/callback"}, {"POST", "/console/auth/logout"},
	}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		tls  bool
		want string
	}{
		{"plain http", &config.Config{}, false, ""},
		{"http redirect_url", &config.Config{Console: config.Console{RedirectURL: "http://vigilante.internal/console/auth/callback"}}, false, ""},
		{"server.tls", &config.Config{Server: config.Server{TLS: &config.ServerTLS{CertFile: "c.pem", KeyFile: "k.pem"}}}, false, "max-age=31536000"},
		{"https redirect_url", &config.Config{Console: config.Console{RedirectURL: "https://vigilante.example.internal/console/auth/callback"}}, false, "max-age=31536000"},
		{"tls request", &config.Config{}, true, "max-age=31536000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, base := serve(t, tc.cfg, tc.tls)
			for _, p := range paths {
				req, _ := http.NewRequest(p.method, base+p.path, nil)
				r, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				r.Body.Close()
				if got := r.Header.Get("Strict-Transport-Security"); got != tc.want {
					t.Errorf("%s %s: Strict-Transport-Security %q, want %q", p.method, p.path, got, tc.want)
				}
				if r.Header.Get("X-Content-Type-Options") != "nosniff" || r.Header.Get("Content-Security-Policy") == "" {
					t.Errorf("%s %s: security headers missing: %v", p.method, p.path, r.Header)
				}
			}
		})
	}
}

func TestSessionKeyMustBeLongEnough(t *testing.T) {
	t.Setenv("VGL_SHORT", "short")
	_, err := New(context.Background(), &config.Config{Console: config.Console{SessionKeyRef: "env:VGL_SHORT"}}, nil, quiet)
	if err == nil || !strings.Contains(err.Error(), "32") {
		t.Fatalf("short session key accepted: %v", err)
	}
}

// The UI never builds HTML from strings, so data from the API cannot inject markup.
func TestAppAvoidsHTMLSinks(t *testing.T) {
	b, err := static.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(string(b), sink) {
			t.Errorf("app.js uses %s", sink)
		}
	}
}

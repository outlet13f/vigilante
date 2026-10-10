package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"vigilante/internal/audit"
	"vigilante/internal/auth/oidctest"
	"vigilante/internal/config"
	"vigilante/internal/console"
	"vigilante/internal/orchestrator"
)

// newConsoleServer serves the API with the console signing in through idp.
// sre is operator everywhere; nobody else has a role.
func newConsoleServer(t *testing.T, idp *oidctest.IDP) (*Server, string) {
	t.Setenv("VGL_TEST_SESSION_KEY", strings.Repeat("s", 40))
	hs := httptest.NewUnstartedServer(nil)
	base := "http://" + hs.Listener.Addr().String()
	cfg, err := config.Parse([]byte(`
version: v1
server: {journal_path: ` + filepath.ToSlash(filepath.Join(t.TempDir(), "j.jsonl")) + `}
auth:
  oidc: {issuer: ` + idp.URL + `, audience: vigilante}
  role_bindings: [{group: sre, role: operator}]
console:
  redirect_url: ` + base + `/console/auth/callback
  session_key_ref: env:VGL_TEST_SESSION_KEY
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true", on: local}}}
services:
  - name: svc
    team: payments
    targets: [a]
    probes: [{id: h, type: tcp, tcp: {address: "127.0.0.1:1"}}]
    rules: [{name: down, when: {metric: h.consecutive_failures, op: ">=", value: 3}}]
    rollback: {executor: x, mode: auto}
`))
	if err != nil {
		t.Fatal(err)
	}
	e, err := orchestrator.New(cfg, orchestrator.Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	s, err := New(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	hs.Config.Handler = s.Handler()
	hs.Start()
	t.Cleanup(hs.Close)
	return s, base
}

func browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func jarCookie(c *http.Client, base, name string) string {
	u, _ := url.Parse(base)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

func TestConsoleSessionUsesTheAPI(t *testing.T) {
	idp := oidctest.New(t)
	idp.User, idp.Groups = "bob", []string{"sre"}
	s, base := newConsoleServer(t, idp)
	b := browser()

	if r, err := b.Get(base + "/console/auth/login"); err != nil || r.StatusCode != 200 {
		t.Fatalf("sign-in: %v %v", r, err)
	}
	r, err := b.Get(base + "/v2/me")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(body), `"user:bob"`) || !strings.Contains(string(body), "operator@*") {
		t.Fatalf("/v2/me with the session cookie: %d %s", r.StatusCode, body)
	}

	post := func(path, csrf string) int {
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(`{"reason":"from the console"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/problem+json, application/json")
		if csrf != "" {
			req.Header.Set(console.CSRFHeader, csrf)
		}
		resp, err := b.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// A cross-site form or script cannot read the csrf cookie, so it cannot
	// send the header: the cookie alone does not authorize changes.
	if code := post("/v2/circuit/trip", ""); code != http.StatusForbidden {
		t.Fatalf("change without CSRF header: %d", code)
	}
	if s.E.Breaker.State().State != "CLOSED" {
		t.Fatal("circuit changed without CSRF token")
	}
	// operator may not trip the circuit (admin only): the session carries
	// the user's own grants, nothing more.
	csrf := jarCookie(b, base, "vgl_csrf")
	if code := post("/v2/circuit/trip", csrf); code != http.StatusForbidden {
		t.Fatalf("operator tripping the circuit: %d", code)
	}

	// A changing call with the CSRF header works and is audited as the user, source ui.
	if r, _ := b.Get(base + "/v2/deployments"); r.StatusCode != 200 {
		t.Fatalf("list: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("POST", base+"/v2/deployments", strings.NewReader(`{"id":"c1","service":"svc","version":"v2","previous_version":"v1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(console.CSRFHeader, csrf)
	resp, err := b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		t.Fatalf("create from the console: %d", resp.StatusCode)
	}
	if code := post("/v2/deployments/c1/abort", csrf); code != 200 {
		t.Fatalf("abort with reason: %d", code)
	}
	if code := post("/v2/deployments/c1/rollbacks", csrf); code != 202 {
		t.Fatalf("rollback from the console: %d", code)
	}
	found := map[string]bool{}
	entries, _ := audit.Query(context.Background(), s.E.Journal, audit.Filter{})
	for _, e := range entries {
		if e.Actor == "user:bob" && e.Source == "ui" {
			found[e.Action] = true
			if e.Action == "rollback.manual" && e.Reason != "from the console" {
				t.Errorf("rollback reason not recorded: %+v", e)
			}
		}
	}
	for _, want := range []string{"console.sign_in", "deployment.create", "rollback.manual"} {
		if !found[want] {
			t.Errorf("audit entry %s by user:bob from ui missing (have %v)", want, found)
		}
	}

	// A bearer token still wins over the cookie (API clients are unaffected).
	req, _ = http.NewRequest("GET", base+"/v2/me", nil)
	req.Header.Set("Authorization", "Bearer bogus")
	if resp, _ := b.Do(req); resp.StatusCode != 401 {
		t.Fatalf("bad bearer token with a valid cookie: %d", resp.StatusCode)
	}
}

func TestConsoleRefusesUsersWithoutRoles(t *testing.T) {
	idp := oidctest.New(t)
	idp.User, idp.Groups = "mallory", []string{"marketing"}
	_, base := newConsoleServer(t, idp)
	b := browser()
	r, err := b.Get(base + "/console/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "role_bindings") || jarCookie(b, base, "vgl_session") != "" {
		t.Fatalf("user without roles: %d %s", r.StatusCode, body)
	}
	if r, _ := b.Get(base + "/v2/me"); r.StatusCode != 401 {
		t.Fatalf("no session expected: %d", r.StatusCode)
	}
}

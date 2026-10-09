package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"vigilante/internal/auth"
	"vigilante/internal/config"
	"vigilante/internal/model"
	"vigilante/internal/orchestrator"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	return newTestServerWith(t, "")
}

// newTestServerWith builds a server; authYAML (an "auth:" block) enables
// service accounts. The legacy token "tok" is always an admin.
func newTestServerWith(t *testing.T, authYAML string) (*Server, *httptest.Server) {
	cfg, err := config.Parse([]byte(authYAML + `
version: v1
server: {journal_path: ` + filepath.ToSlash(filepath.Join(t.TempDir(), "j.jsonl")) + `}
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true", on: local}}}
services:
  - name: svc
    team: payments
    targets: [a]
    probes: [{id: h, type: tcp, tcp: {address: "127.0.0.1:1"}}]
    rules: [{name: down, when: {metric: h.consecutive_failures, op: ">=", value: 3}}]
    rollback: {executor: x}
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
	if s.Auth, err = auth.New(context.Background(), cfg.Auth, "tok"); err != nil {
		t.Fatal(err)
	}
	s.WebhookSecret = "hook"
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	return s, hs
}

func call(t *testing.T, method, url, token, body string, hdr map[string]string) (*http.Response, map[string]any) {
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestAuthAndLifecycle(t *testing.T) {
	s, hs := newTestServer(t)
	if r, _ := call(t, "GET", hs.URL+"/v1/deployments", "", "", nil); r.StatusCode != 401 {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	r, d := call(t, "POST", hs.URL+"/v1/deployments", "tok", `{"id":"d1","service":"svc","version":"v2","previous_version":"v1"}`, nil)
	if r.StatusCode != 201 || d["state"] != "PENDING" {
		t.Fatalf("create: %d %v", r.StatusCode, d)
	}
	if r, _ := call(t, "POST", hs.URL+"/v1/deployments/d1/phases/bogus", "tok", "", nil); r.StatusCode != 409 {
		t.Fatalf("bad phase: %d", r.StatusCode)
	}
	r, st := call(t, "GET", hs.URL+"/v1/deployments/d1", "tok", "", nil)
	if r.StatusCode != 200 || st["exit_code"].(float64) != 1 {
		t.Fatalf("get: %d %v", r.StatusCode, st)
	}
	call(t, "POST", hs.URL+"/v1/circuit/trip?reason=freeze", "tok", "", nil)
	if s.E.Breaker.State().State != "OPEN" {
		t.Fatal("trip failed")
	}
	_, h := call(t, "GET", hs.URL+"/healthz", "", "", nil)
	if h["circuit"] != "OPEN" {
		t.Fatalf("healthz %v", h)
	}
}

func TestSampleIngestTagsAgentSource(t *testing.T) {
	s, hs := newTestServer(t)
	body := `[{"target":"a","metric":"h.up","value":1,"time":"2026-10-09T10:00:00Z"}]`
	if r, _ := call(t, "POST", hs.URL+"/v1/samples", "tok", body, nil); r.StatusCode != 204 {
		t.Fatalf("ingest %d", r.StatusCode)
	}
	if m := s.E.Store.Metrics("a"); len(m) != 1 {
		t.Fatalf("metrics %v", m)
	}
}

func TestGitHubWebhookSignature(t *testing.T) {
	_, hs := newTestServer(t)
	body := `{"deployment_status":{"state":"success"},"deployment":{"id":42,"ref":"v2","payload":{"service":"svc","previous_version":"v1"}}}`
	if r, _ := call(t, "POST", hs.URL+"/v1/webhooks/github", "", body, map[string]string{"X-Hub-Signature-256": "sha256=bad"}); r.StatusCode != 401 {
		t.Fatalf("bad signature accepted: %d", r.StatusCode)
	}
	mac := hmac.New(sha256.New, []byte("hook"))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	r, d := call(t, "POST", hs.URL+"/v1/webhooks/github", "", body, map[string]string{"X-Hub-Signature-256": sig})
	if r.StatusCode != 201 || d["id"] != "gh-42" || d["version"] != "v2" {
		t.Fatalf("webhook: %d %v", r.StatusCode, d)
	}
}

func TestParseWebhookIgnoresNonSuccess(t *testing.T) {
	req, err := ParseWebhook("gitlab", []byte(`{"status":"running","variables":{"VIGILANTE_SERVICE":"svc"}}`), "")
	if err != nil || req != nil {
		t.Fatalf("%v %v", req, err)
	}
	req, err = ParseWebhook("jenkins", []byte(`{"service":"svc","version":"v3","phase":"canary"}`), "")
	if err != nil || req.Version != "v3" || model.Phase(req.Phase) != model.PhaseCanary {
		t.Fatalf("%+v %v", req, err)
	}
}

func TestCreateFillsPreviousFromLastGood(t *testing.T) {
	s, hs := newTestServer(t)
	if _, err := s.E.MarkGood("svc", "v7", ""); err != nil {
		t.Fatal(err)
	}
	r, d := call(t, "POST", hs.URL+"/v1/deployments", "tok", `{"id":"d9","service":"svc","version":"v8"}`, nil)
	if r.StatusCode != 201 || d["previous_version"] != "v7" {
		t.Fatalf("create: %d %v", r.StatusCode, d["previous_version"])
	}
}

type fakeLeadership struct {
	leader bool
	addr   string
}

func (f fakeLeadership) IsLeader() bool           { return f.leader }
func (f fakeLeadership) Leader() (string, string) { return "node-x", f.addr }

func TestFollowerForwardsToLeader(t *testing.T) {
	leader, leaderHS := newTestServer(t)
	leader.HA = fakeLeadership{leader: true}
	call(t, "POST", leaderHS.URL+"/v1/deployments", "tok", `{"id":"on-leader","service":"svc","version":"v2","previous_version":"v1"}`, nil)

	follower, followerHS := newTestServer(t)
	follower.HA = fakeLeadership{addr: leaderHS.URL}
	r, out := call(t, "GET", followerHS.URL+"/v1/deployments/on-leader", "tok", "", nil)
	if r.StatusCode != 200 || out["deployment"] == nil {
		t.Fatalf("follower did not forward: %d %v", r.StatusCode, out)
	}
	_, h := call(t, "GET", followerHS.URL+"/healthz", "", "", nil)
	if h["role"] != "follower" || h["leader"] != leaderHS.URL {
		t.Fatalf("healthz is answered locally: %v", h)
	}
	// A request another follower already forwarded is not bounced again.
	r, _ = call(t, "GET", followerHS.URL+"/v1/deployments", "tok", "", map[string]string{forwardedHeader: "node-y"})
	if r.StatusCode != 503 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("forward loop guard: %d", r.StatusCode)
	}
	// No leader known yet -> 503 with Retry-After.
	follower.HA = fakeLeadership{}
	if r, _ := call(t, "GET", followerHS.URL+"/v1/deployments", "tok", "", nil); r.StatusCode != 503 {
		t.Fatalf("no leader: %d", r.StatusCode)
	}
}

func sa(t *testing.T, name, role, scope string) (token, yaml string) {
	tok, sha, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return tok, "  - {name: " + name + ", token_sha256: " + sha + ", roles: [{role: " + role + ", scope: \"" + scope + "\"}]}\n"
}

func TestRoleAndScopeEnforcement(t *testing.T) {
	deployer, y1 := sa(t, "ci-svc", "deployer", "service=svc")
	otherTeam, y2 := sa(t, "ci-other", "deployer", "team=frontend")
	viewer, y3 := sa(t, "dash", "viewer", "*")
	agent, y4 := sa(t, "agents", "agent", "*")
	s, hs := newTestServerWith(t, "auth:\n  service_accounts:\n"+y1+y2+y3+y4)
	create := `{"id":"d1","service":"svc","version":"v2","previous_version":"v1"}`

	if r, _ := call(t, "POST", hs.URL+"/v1/deployments", "", create, nil); r.StatusCode != 401 || r.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous create: %d", r.StatusCode)
	}
	if r, out := call(t, "POST", hs.URL+"/v1/deployments", otherTeam, create, nil); r.StatusCode != 403 || !strings.Contains(out["error"].(string), "team=payments") {
		t.Fatalf("other team create: %d %v", r.StatusCode, out)
	}
	if r, _ := call(t, "POST", hs.URL+"/v1/deployments", viewer, create, nil); r.StatusCode != 403 {
		t.Fatalf("viewer create: %d", r.StatusCode)
	}
	r, d := call(t, "POST", hs.URL+"/v1/deployments", deployer, create, nil)
	if r.StatusCode != 201 || d["created_by"] != "sa:ci-svc" {
		t.Fatalf("deployer create: %d %v", r.StatusCode, d)
	}
	if r, _ := call(t, "GET", hs.URL+"/v1/deployments/d1", viewer, "", nil); r.StatusCode != 200 {
		t.Fatalf("viewer read: %d", r.StatusCode)
	}
	// The other team's token sees an empty list, not d1.
	req, _ := http.NewRequest("GET", hs.URL+"/v1/deployments", nil)
	req.Header.Set("Authorization", "Bearer "+otherTeam)
	resp, _ := http.DefaultClient.Do(req)
	var list []map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 0 {
		t.Fatalf("list leaked another team's deployment: %v", list)
	}
	if r, _ := call(t, "POST", hs.URL+"/v1/deployments/d1/rollback", deployer, "{}", nil); r.StatusCode != 403 {
		t.Fatalf("deployer may not roll back manually: %d", r.StatusCode)
	}
	if r, _ := call(t, "POST", hs.URL+"/v1/circuit/reset", deployer, "", nil); r.StatusCode != 403 {
		t.Fatalf("circuit reset needs admin: %d", r.StatusCode)
	}
	if r, _ := call(t, "POST", hs.URL+"/v1/circuit/reset", "tok", "", nil); r.StatusCode != 200 {
		t.Fatalf("legacy admin token: %d", r.StatusCode)
	}
	if r, _ := call(t, "POST", hs.URL+"/v1/samples", viewer, `[{"target":"a","metric":"h.up","value":1}]`, nil); r.StatusCode != 403 {
		t.Fatalf("viewer may not push samples: %d", r.StatusCode)
	}
	if r, _ := call(t, "POST", hs.URL+"/v1/samples", agent, `[{"target":"a","metric":"h.up","value":1}]`, nil); r.StatusCode != 204 {
		t.Fatalf("agent push: %d", r.StatusCode)
	}
	if r, _ := call(t, "GET", hs.URL+"/v1/deployments", agent, "", nil); r.StatusCode != 200 {
		t.Fatalf("agent list: %d", r.StatusCode) // allowed, but filtered to nothing
	}
	_, who := call(t, "GET", hs.URL+"/v1/whoami", deployer, "", nil)
	if who["id"] != "sa:ci-svc" || who["grants"].([]any)[0] != "deployer@service=svc" {
		t.Fatalf("whoami: %v", who)
	}
	_ = s
}

func TestFourEyesApproval(t *testing.T) {
	alice, y1 := sa(t, "alice-bot", "operator", "*")
	bob, y2 := sa(t, "bob-bot", "operator", "*")
	s, hs := newTestServerWith(t, "auth:\n  four_eyes: true\n  service_accounts:\n"+y1+y2)
	if r, _ := call(t, "POST", hs.URL+"/v1/deployments", alice, `{"id":"d1","service":"svc","version":"v2","previous_version":"v1"}`, nil); r.StatusCode != 201 {
		t.Fatalf("create: %d", r.StatusCode)
	}
	s.E.Live("d1").State = model.StateAwaitApproval // as left by a gated escalation
	r, out := call(t, "POST", hs.URL+"/v1/deployments/d1/approve", alice, "", nil)
	if r.StatusCode != 403 || !strings.Contains(out["error"].(string), "four-eyes") {
		t.Fatalf("creator approving own deployment: %d %v", r.StatusCode, out)
	}
	if r, _ := call(t, "POST", hs.URL+"/v1/deployments/d1/approve", bob, "", nil); r.StatusCode != 202 {
		t.Fatalf("second operator approval: %d", r.StatusCode)
	}
}

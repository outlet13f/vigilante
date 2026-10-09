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

	"vigilante/internal/config"
	"vigilante/internal/model"
	"vigilante/internal/orchestrator"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	cfg, err := config.Parse([]byte(`
version: v1
server: {journal_path: ` + filepath.ToSlash(filepath.Join(t.TempDir(), "j.jsonl")) + `}
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true", on: local}}}
services:
  - name: svc
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
	s := New(context.Background(), e)
	s.Token, s.WebhookSecret = "tok", "hook"
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

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"

	"vigilante/internal/auth"
	"vigilante/internal/config"
	"vigilante/internal/itsm"
	"vigilante/internal/itsm/snowtest"
	"vigilante/internal/orchestrator"
)

var (
	specOnce sync.Once
	specVal  validator.Validator
	specDoc  libopenapi.Document
	specErr  error
)

// spec loads api/openapi.yaml once.
func spec(t *testing.T) (validator.Validator, libopenapi.Document) {
	t.Helper()
	specOnce.Do(func() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
		if err != nil {
			specErr = err
			return
		}
		if specDoc, err = libopenapi.NewDocument(raw); err != nil {
			specErr = err
			return
		}
		v, errs := validator.NewValidator(specDoc)
		if len(errs) > 0 {
			specErr = errs[0]
			return
		}
		if ok, verrs := v.ValidateDocument(); !ok {
			for _, e := range verrs {
				t.Logf("spec: %s", e.Message)
			}
			specErr = io.ErrUnexpectedEOF
			return
		}
		specVal = v
	})
	if specErr != nil {
		t.Fatalf("api/openapi.yaml: %v", specErr)
	}
	return specVal, specDoc
}

// v2Client calls the server and validates every request and response
// against the spec.
type v2Client struct {
	t     *testing.T
	base  string
	token string
}

type v2Resp struct {
	*http.Response
	Body []byte
	JSON map[string]any
}

func (c *v2Client) do(method, path, body string, hdr map[string]string) *v2Resp {
	c.t.Helper()
	v, _ := spec(c.t)
	req, _ := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, val := range hdr {
		req.Header.Set(k, val)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Validate against the spec with fresh bodies.
	vreq, _ := http.NewRequest(method, "https://vigilante.example.internal:8088"+path, strings.NewReader(body))
	vreq.Header = req.Header.Clone()
	vresp := *resp
	vresp.Body = io.NopCloser(bytes.NewReader(raw))
	vresp.Request = vreq
	if strings.HasPrefix(path, "/v2/") && resp.StatusCode != http.StatusNotModified {
		// A request the spec forbids must be refused; either way the
		// response itself must match the spec.
		if ok, _ := v.ValidateHttpRequest(vreq); !ok && resp.StatusCode < 400 {
			c.t.Errorf("contract: %s %s violates the spec but was accepted (%d)", method, path, resp.StatusCode)
		}
		vreq2, _ := http.NewRequest(method, vreq.URL.String(), strings.NewReader(body))
		vreq2.Header = vreq.Header
		vresp.Request = vreq2
		if ok, errs := v.ValidateHttpResponse(vreq2, &vresp); !ok {
			for _, e := range errs {
				if e.ValidationType == "path" && resp.StatusCode == http.StatusNotFound {
					continue // an endpoint the spec does not have: nothing to match
				}
				c.t.Errorf("contract: %s %s -> %d: %s (%s)", method, path, resp.StatusCode, e.Message, e.Reason)
			}
			c.t.Logf("body: %s", raw)
		}
	}
	out := &v2Resp{Response: resp, Body: raw}
	_ = json.Unmarshal(raw, &out.JSON)
	return out
}

func newV2Server(t *testing.T, authYAML string) (*Server, string) {
	t.Helper()
	cfg, err := config.Parse([]byte(authYAML + `
version: v1
server: {journal_path: ` + filepath.ToSlash(filepath.Join(t.TempDir(), "j.jsonl")) + `}
targets:
  - {name: a, kind: vm, address: 10.0.0.1, labels: {zone: z1}}
  - {name: b, kind: vm, address: 10.0.0.2}
executors: {x: {type: exec, exec: {rollback: "true", on: local}}}
services:
  - name: svc
    team: payments
    targets: [a]
    probes: [{id: h, type: tcp, interval: 50ms, timeout: 50ms, tcp: {address: "127.0.0.1:1"}}]
    rules: [{name: down, when: {metric: h.consecutive_failures, op: ">=", value: 3}}]
    phases: {canary: {observation_window: 3s, eval_interval: 100ms}}
    rollback: {executor: x}
  - name: zeta
    team: search
    targets: [b]
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
	ctx, cancel := context.WithCancel(context.Background())
	s, err := New(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if s.Auth, err = auth.New(ctx, cfg.Auth, "tok"); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		hs.Close()
		s.Close() // waits for observations and operations to record their end
		cancel()
		e.Close()
	})
	return s, hs.URL
}

func waitOp(t *testing.T, c *v2Client, id string) *v2Resp {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		r := c.do("GET", "/v2/operations/"+id, "", nil)
		if r.JSON["status"] != "running" {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s still running", id)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func wantProblem(t *testing.T, r *v2Resp, status int, code string) {
	t.Helper()
	if r.StatusCode != status || r.JSON["code"] != code || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("want %d %s problem, got %d %s: %s", status, code, r.StatusCode, r.Header.Get("Content-Type"), r.Body)
	}
	if r.JSON["request_id"] == "" || r.JSON["request_id"] != r.Header.Get("X-Request-ID") {
		t.Fatalf("problem must carry the request id: %s", r.Body)
	}
}

func TestV2ContractLifecycle(t *testing.T) {
	_, base := newV2Server(t, "")
	c := &v2Client{t: t, base: base, token: "tok"}

	if r := c.do("GET", "/v2/me", "", nil); r.StatusCode != 200 || r.JSON["id"] != "token:legacy" {
		t.Fatalf("me: %d %s", r.StatusCode, r.Body)
	}

	// Services: paging by name with an opaque cursor.
	r := c.do("GET", "/v2/services?limit=1", "", nil)
	items := r.JSON["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "svc" || r.JSON["next_cursor"] == nil {
		t.Fatalf("first page: %s", r.Body)
	}
	r = c.do("GET", "/v2/services?limit=1&cursor="+r.JSON["next_cursor"].(string), "", nil)
	if items := r.JSON["items"].([]any); len(items) != 1 || items[0].(map[string]any)["name"] != "zeta" || r.JSON["next_cursor"] != nil {
		t.Fatalf("second page: %s", r.Body)
	}
	wantProblem(t, c.do("GET", "/v2/services?limit=0", "", nil), 400, "bad_request")
	wantProblem(t, c.do("GET", "/v2/services?cursor=!!!", "", nil), 400, "bad_request")
	if r := c.do("GET", "/v2/services/svc", "", nil); r.StatusCode != 200 || r.JSON["phases"].([]any)[0] != "canary" {
		t.Fatalf("service: %s", r.Body)
	}
	wantProblem(t, c.do("GET", "/v2/services/nope", "", nil), 404, "not_found")
	if r := c.do("GET", "/v2/targets", "", nil); r.StatusCode != 200 || len(r.JSON["items"].([]any)) != 2 {
		t.Fatalf("targets: %s", r.Body)
	}
	if r := c.do("GET", "/v2/targets/a", "", nil); r.StatusCode != 200 || r.JSON["services"].([]any)[0] != "svc" {
		t.Fatalf("target: %s", r.Body)
	}
	if r := c.do("GET", "/v2/presets", "", nil); r.StatusCode != 200 || len(r.JSON["items"].([]any)) < 4 {
		t.Fatalf("presets: %s", r.Body)
	}

	// Without a last good version a deployment has no rollback target.
	wantProblem(t, c.do("POST", "/v2/deployments", `{"service":"svc","version":"v2"}`, nil), 422, "validation_failed")
	wantProblem(t, c.do("GET", "/v2/services/svc/last-good", "", nil), 404, "not_found")
	if r := c.do("PUT", "/v2/services/svc/last-good", `{"version":"v1","reason":"baseline release"}`, nil); r.StatusCode != 200 || r.JSON["version"] != "v1" {
		t.Fatalf("last-good: %s", r.Body)
	}
	wantProblem(t, c.do("POST", "/v2/deployments", `{"service":"svc","version":"v2","bogus":1}`, nil), 400, "bad_request")
	wantProblem(t, c.do("POST", "/v2/deployments", `{"service":"nope","version":"v2"}`, nil), 422, "validation_failed")

	r = c.do("POST", "/v2/deployments", `{"id":"d1","service":"svc","version":"v2"}`, nil)
	if r.StatusCode != 201 || r.JSON["previous_version"] != "v1" || r.Header.Get("Location") != "/v2/deployments/d1" || r.Header.Get("ETag") == "" {
		t.Fatalf("create: %d %s", r.StatusCode, r.Body)
	}
	if again := c.do("POST", "/v2/deployments", `{"id":"d1","service":"svc","version":"v2"}`, nil); again.StatusCode != 200 {
		t.Fatalf("re-registering the same deployment returns it: %d", again.StatusCode)
	}
	etag := c.do("GET", "/v2/deployments/d1", "", nil).Header.Get("ETag")
	if r := c.do("GET", "/v2/deployments/d1", "", map[string]string{"If-None-Match": etag}); r.StatusCode != 304 {
		t.Fatalf("If-None-Match: %d", r.StatusCode)
	}
	wantProblem(t, c.do("POST", "/v2/deployments/d1/observations", `{"phase":"canary"}`, map[string]string{"If-Match": `W/"stale"`}), 412, "precondition_failed")
	wantProblem(t, c.do("POST", "/v2/deployments/d1/observations", `{"phase":"full"}`, nil), 422, "validation_failed")
	wantProblem(t, c.do("POST", "/v2/deployments/d1/observations", `{"phase":"later"}`, nil), 422, "validation_failed")

	// Observe: the canary's probe fails, so the phase is rolled back.
	r = c.do("POST", "/v2/deployments/d1/observations", `{"phase":"canary"}`, map[string]string{"If-Match": etag, "Idempotency-Key": "obs-1"})
	if r.StatusCode != 202 || r.JSON["status"] != "running" || r.Header.Get("Location") != "/v2/operations/"+r.JSON["id"].(string) {
		t.Fatalf("observe: %d %s", r.StatusCode, r.Body)
	}
	opID := r.JSON["id"].(string)
	// A retry with the same key does not start a second observation.
	replay := c.do("POST", "/v2/deployments/d1/observations", `{"phase":"canary"}`, map[string]string{"If-Match": etag, "Idempotency-Key": "obs-1"})
	if replay.StatusCode != 202 || replay.JSON["id"] != opID || replay.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %s", replay.StatusCode, replay.Body)
	}
	wantProblem(t, c.do("POST", "/v2/deployments/d1/observations", `{"phase":"rolling"}`, map[string]string{"Idempotency-Key": "obs-1"}), 422, "idempotency_key_reused")
	wantProblem(t, c.do("POST", "/v2/deployments/d1/observations", `{"phase":"canary"}`, nil), 409, "conflict")

	done := waitOp(t, c, opID)
	res := done.JSON["result"].(map[string]any)
	if done.JSON["status"] != "completed" || res["state"] != "ROLLED_BACK" || res["exit_code"].(float64) != 2 || done.JSON["finished_at"] == nil {
		t.Fatalf("operation result: %s", done.Body)
	}
	if r := c.do("GET", "/v2/operations?deployment_id=d1", "", nil); len(r.JSON["items"].([]any)) != 1 {
		t.Fatalf("operations list: %s", r.Body)
	}
	if r := c.do("GET", "/v2/deployments/d1", "", nil); r.JSON["state"] != "ROLLED_BACK" || r.JSON["exit_code"].(float64) != 2 {
		t.Fatalf("deployment after: %s", r.Body)
	}

	// Manual rollback of a second deployment, then nothing to approve.
	c.do("POST", "/v2/deployments", `{"id":"d2","service":"svc","version":"v3","previous_version":"v1"}`, nil)
	r = c.do("POST", "/v2/deployments/d2/rollbacks", `{"reason":"bad metrics in APM"}`, map[string]string{"Idempotency-Key": "rb-1"})
	if r.StatusCode != 202 || r.JSON["kind"] != "rollback" {
		t.Fatalf("rollback: %d %s", r.StatusCode, r.Body)
	}
	if done := waitOp(t, c, r.JSON["id"].(string)); done.JSON["result"].(map[string]any)["state"] != "ROLLED_BACK" {
		t.Fatalf("rollback result: %s", done.Body)
	}
	wantProblem(t, c.do("POST", "/v2/deployments/d2/rollbacks", `{"executor":"nope"}`, nil), 422, "validation_failed")
	wantProblem(t, c.do("POST", "/v2/deployments/d2/approvals", `{}`, nil), 409, "conflict")
	if r := c.do("POST", "/v2/deployments/d2/abort", "", nil); r.StatusCode != 200 || r.JSON["id"] != "d2" {
		t.Fatalf("abort: %s", r.Body)
	}
	if r := c.do("GET", "/v2/deployments?service=svc&limit=1", "", nil); len(r.JSON["items"].([]any)) != 1 || r.JSON["next_cursor"] == nil {
		t.Fatalf("deployment paging: %s", r.Body)
	}

	// Baseline capture is asynchronous.
	r = c.do("POST", "/v2/services/svc/baselines", `{"window":"300ms"}`, nil)
	if r.StatusCode != 202 || r.JSON["kind"] != "baseline" {
		t.Fatalf("baseline: %d %s", r.StatusCode, r.Body)
	}
	waitOp(t, c, r.JSON["id"].(string))
	wantProblem(t, c.do("POST", "/v2/services/svc/baselines", `{"window":"soon"}`, nil), 422, "validation_failed")

	// Circuit.
	wantProblem(t, c.do("POST", "/v2/circuit/trip", `{"reason":""}`, nil), 422, "validation_failed")
	if r := c.do("POST", "/v2/circuit/trip", `{"reason":"freeze for incident"}`, nil); r.JSON["state"] != "OPEN" || r.JSON["opened_at"] == nil {
		t.Fatalf("trip: %s", r.Body)
	}
	if r := c.do("POST", "/v2/circuit/reset", `{"reason":"incident closed"}`, nil); r.JSON["state"] != "CLOSED" {
		t.Fatalf("reset: %s", r.Body)
	}
	if r := c.do("GET", "/v2/circuit", "", nil); r.StatusCode != 200 {
		t.Fatalf("circuit: %s", r.Body)
	}

	// Audit trail, oldest first, paged by store position.
	r = c.do("GET", "/v2/audit-events?limit=2", "", nil)
	first := r.JSON["items"].([]any)
	if len(first) != 2 || r.JSON["next_cursor"] == nil {
		t.Fatalf("audit page: %s", r.Body)
	}
	r2 := c.do("GET", "/v2/audit-events?limit=500&cursor="+r.JSON["next_cursor"].(string), "", nil)
	rest := r2.JSON["items"].([]any)
	if rest[0].(map[string]any)["seq"].(float64) <= first[1].(map[string]any)["seq"].(float64) {
		t.Fatal("audit pages must not overlap")
	}
	var actions []string
	for _, it := range append(first, rest...) {
		a, _ := it.(map[string]any)["action"].(string) // circuit and rollback-start entries have none
		actions = append(actions, a)
	}
	for _, want := range []string{"mark-good", "deployment.create", "phase.start", "rollback.auto", "rollback.manual", "circuit.trip", "circuit.reset"} {
		if !strings.Contains(strings.Join(actions, ","), want) {
			t.Errorf("audit trail lacks %s: %v", want, actions)
		}
	}

	// Errors are problem documents everywhere under /v2.
	wantProblem(t, (&v2Client{t: t, base: base}).do("GET", "/v2/deployments", "", nil), 401, "unauthenticated")
	wantProblem(t, c.do("GET", "/v2/nothing-here", "", nil), 404, "not_found")
	wantProblem(t, c.do("GET", "/v2/deployments/nope", "", nil), 404, "not_found")
}

func TestV2Scopes(t *testing.T) {
	devTok, dev := sa(t, "search-dev", "operator", "team=search")
	_, base := newV2Server(t, "auth:\n  four_eyes: true\n  service_accounts:\n"+dev)
	admin := &v2Client{t: t, base: base, token: "tok"}
	c := &v2Client{t: t, base: base, token: devTok}

	if r := c.do("GET", "/v2/services", "", nil); len(r.JSON["items"].([]any)) != 1 {
		t.Fatalf("a team-scoped caller sees only its services: %s", r.Body)
	}
	if r := c.do("GET", "/v2/targets", "", nil); len(r.JSON["items"].([]any)) != 1 {
		t.Fatalf("targets filtered by service: %s", r.Body)
	}
	wantProblem(t, c.do("GET", "/v2/targets/a", "", nil), 403, "forbidden")
	wantProblem(t, c.do("GET", "/v2/services/svc", "", nil), 403, "forbidden")
	admin.do("PUT", "/v2/services/svc/last-good", `{"version":"v1"}`, nil)
	admin.do("POST", "/v2/deployments", `{"id":"p1","service":"svc","version":"v2"}`, nil)
	wantProblem(t, c.do("GET", "/v2/deployments/p1", "", nil), 403, "forbidden")
	wantProblem(t, c.do("POST", "/v2/deployments/p1/rollbacks", `{}`, nil), 403, "forbidden")
	wantProblem(t, c.do("POST", "/v2/circuit/trip", `{"reason":"x"}`, nil), 403, "forbidden")
	wantProblem(t, c.do("GET", "/v2/audit-events", "", nil), 403, "forbidden")
	if r := c.do("GET", "/v2/deployments", "", nil); len(r.JSON["items"].([]any)) != 0 {
		t.Fatalf("lists hide other teams' deployments: %s", r.Body)
	}
	// Idempotency keys are per caller: the same key from two callers is two requests.
	r1 := admin.do("PUT", "/v2/services/zeta/last-good", `{"version":"z1"}`, map[string]string{"Idempotency-Key": "k"})
	r2 := c.do("PUT", "/v2/services/zeta/last-good", `{"version":"z1"}`, map[string]string{"Idempotency-Key": "k"})
	if r1.StatusCode != 200 || r2.StatusCode != 200 || r2.Header.Get("Idempotent-Replayed") == "true" {
		t.Fatalf("keys must be scoped to the caller: %d %d %q", r1.StatusCode, r2.StatusCode, r2.Header.Get("Idempotent-Replayed"))
	}
}

// TestV2RoutesMatchSpec: every v2 operation in the spec is served, and
// nothing is served under /v2 that the spec does not describe.
func TestV2RoutesMatchSpec(t *testing.T) {
	_, doc := spec(t)
	model, err := doc.BuildV3Model()
	if err != nil {
		t.Fatal(err)
	}
	inSpec := map[string]bool{}
	for path, item := range model.Model.Paths.PathItems.FromOldest() {
		if !strings.HasPrefix(path, "/v2/") {
			continue
		}
		for method := range item.GetOperations().FromOldest() {
			inSpec[strings.ToUpper(method)+" "+path] = true
		}
	}
	served := map[string]bool{}
	s := &Server{}
	for _, rt := range s.v2Routes() {
		served[rt.pattern] = true
	}
	for k := range inSpec {
		if !served[k] {
			t.Errorf("spec operation not served: %s", k)
		}
	}
	for k := range served {
		if !inSpec[k] {
			t.Errorf("served but not in the spec: %s", k)
		}
	}
	if len(inSpec) < 20 {
		t.Fatalf("only %d v2 operations found in the spec", len(inSpec))
	}
}

func TestV2ApproveModeDecisions(t *testing.T) {
	s, base := newV2Server(t, "")
	sv, _ := s.E.Cfg.Service("svc")
	sv.Rollback.Mode = "approve"
	c := &v2Client{t: t, base: base, token: "tok"}
	c.do("PUT", "/v2/services/svc/last-good", `{"version":"v1"}`, nil)

	fail := func(id string) {
		t.Helper()
		c.do("POST", "/v2/deployments", `{"id":"`+id+`","service":"svc","version":"v2"}`, nil)
		r := c.do("POST", "/v2/deployments/"+id+"/observations", `{"phase":"canary"}`, nil)
		done := waitOp(t, c, r.JSON["id"].(string))
		if res := done.JSON["result"].(map[string]any); res["state"] != "AWAITING_APPROVAL" || res["exit_code"].(float64) != 3 {
			t.Fatalf("approve mode must wait: %s", done.Body)
		}
	}

	fail("am1")
	d := c.do("GET", "/v2/deployments/am1", "", nil)
	pr, ok := d.JSON["pending_rollback"].(map[string]any)
	if !ok || pr["targets"].([]any)[0] != "a" || pr["expires_at"] == nil {
		t.Fatalf("pending rollback: %s", d.Body)
	}
	wantProblem(t, c.do("POST", "/v2/deployments/am1/approvals", `{"decision":"maybe"}`, nil), 422, "validation_failed")
	r := c.do("POST", "/v2/deployments/am1/approvals", `{"decision":"reject","comment":"false positive"}`, nil)
	if r.StatusCode != 202 || r.JSON["kind"] != "approval" {
		t.Fatalf("reject: %d %s", r.StatusCode, r.Body)
	}
	if done := waitOp(t, c, r.JSON["id"].(string)); done.JSON["result"].(map[string]any)["state"] != "HELD" {
		t.Fatalf("after reject: %s", done.Body)
	}
	if d := c.do("GET", "/v2/deployments/am1", "", nil); d.JSON["pending_rollback"] != nil || !strings.Contains(d.JSON["reason"].(string), "false positive") {
		t.Fatalf("held deployment: %s", d.Body)
	}
	wantProblem(t, c.do("POST", "/v2/deployments/am1/approvals", `{}`, nil), 409, "conflict")

	fail("am2")
	r = c.do("POST", "/v2/deployments/am2/approvals", `{"comment":"go"}`, nil)
	if done := waitOp(t, c, r.JSON["id"].(string)); done.JSON["result"].(map[string]any)["state"] != "ROLLED_BACK" {
		t.Fatalf("after approve: %s", done.Body)
	}
	if d := c.do("GET", "/v2/deployments/am2", "", nil); d.JSON["approved_by"] != "token:legacy" {
		t.Fatalf("approver not recorded: %s", d.Body)
	}
	r = c.do("GET", "/v2/audit-events?action=rollback.reject", "", nil)
	if items := r.JSON["items"].([]any); len(items) != 1 || items[0].(map[string]any)["reason"] != "false positive" {
		t.Fatalf("audit: %s", r.Body)
	}
}

func TestV2ChangeFreeze(t *testing.T) {
	depTok, dep := sa(t, "ci", "deployer", "*")
	_, base := newV2Server(t, "auth:\n  service_accounts:\n"+dep)
	admin := &v2Client{t: t, base: base, token: "tok"}
	ci := &v2Client{t: t, base: base, token: depTok}
	admin.do("PUT", "/v2/services/svc/last-good", `{"version":"v1"}`, nil)
	admin.do("POST", "/v2/deployments", `{"id":"before","service":"svc","version":"v2"}`, nil)

	wantProblem(t, ci.do("POST", "/v2/freezes", `{"name":"x","ends_at":"2099-01-01T00:00:00Z"}`, nil), 403, "forbidden")
	wantProblem(t, admin.do("POST", "/v2/freezes", `{"name":"x","ends_at":"2001-01-01T00:00:00Z"}`, nil), 422, "validation_failed")
	r := admin.do("POST", "/v2/freezes", `{"name":"incident-7","reason":"payment outage","ends_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`","teams":["payments"]}`, nil)
	if r.StatusCode != 201 || r.JSON["allow_rollback"] != true {
		t.Fatalf("declare: %d %s", r.StatusCode, r.Body)
	}
	id := r.JSON["id"].(string)
	if l := ci.do("GET", "/v2/freezes", "", nil); len(l.JSON["items"].([]any)) != 1 || l.JSON["items"].([]any)[0].(map[string]any)["active"] != true {
		t.Fatalf("list: %s", l.Body)
	}

	// New deployments of the team's service are refused; others are not.
	r = ci.do("POST", "/v2/deployments", `{"service":"svc","version":"v3"}`, nil)
	wantProblem(t, r, 409, "change_frozen")
	if !strings.Contains(r.JSON["detail"].(string), "payment outage") {
		t.Fatalf("detail: %s", r.Body)
	}
	admin.do("PUT", "/v2/services/zeta/last-good", `{"version":"z1"}`, nil)
	if r := ci.do("POST", "/v2/deployments", `{"service":"zeta","version":"z2"}`, nil); r.StatusCode != 201 {
		t.Fatalf("another team's service is not frozen: %d %s", r.StatusCode, r.Body)
	}
	// A deployment registered before the freeze cannot start a phase either.
	wantProblem(t, ci.do("POST", "/v2/deployments/before/observations", `{"phase":"canary"}`, nil), 409, "change_frozen")
	// Only admins may override.
	wantProblem(t, ci.do("POST", "/v2/deployments", `{"service":"svc","version":"v3","freeze_override":"hotfix"}`, nil), 403, "forbidden")
	r = admin.do("POST", "/v2/deployments", `{"id":"hotfix-1","service":"svc","version":"v3","freeze_override":"fix for incident-7"}`, nil)
	if r.StatusCode != 201 || !strings.Contains(r.JSON["freeze_override"].(string), "fix for incident-7") {
		t.Fatalf("override: %d %s", r.StatusCode, r.Body)
	}
	if r := admin.do("POST", "/v2/deployments/hotfix-1/observations", `{"phase":"canary"}`, nil); r.StatusCode != 202 {
		t.Fatalf("overridden deployment may observe: %d %s", r.StatusCode, r.Body)
	} else {
		waitOp(t, admin, r.JSON["id"].(string))
	}
	if a := admin.do("GET", "/v2/audit-events?action=freeze.override", "", nil); len(a.JSON["items"].([]any)) != 1 {
		t.Fatalf("override not audited: %s", a.Body)
	}

	if r := admin.do("DELETE", "/v2/freezes/"+id, "", nil); r.StatusCode != 200 || r.JSON["ended_at"] == nil {
		t.Fatalf("end: %d %s", r.StatusCode, r.Body)
	}
	if r := ci.do("POST", "/v2/deployments", `{"service":"svc","version":"v4"}`, nil); r.StatusCode != 201 {
		t.Fatalf("after the freeze: %d %s", r.StatusCode, r.Body)
	}
	wantProblem(t, admin.do("DELETE", "/v2/freezes/nope", "", nil), 404, "not_found")
}

func TestV2ServiceNowGateIncidentsAndNotes(t *testing.T) {
	snow := snowtest.New(t)
	now := time.Now().UTC()
	snow.AddChange("CHG100", "sys100", "-1", "approved", now.Add(-time.Hour).Format("2006-01-02 15:04:05"), now.Add(time.Hour).Format("2006-01-02 15:04:05"))
	snow.AddChange("CHG200", "sys200", "-3", "requested", now.Add(-time.Hour).Format("2006-01-02 15:04:05"), now.Add(time.Hour).Format("2006-01-02 15:04:05"))
	t.Setenv("VGL_SNOW_PW", "snow-pass")
	s, base := newV2Server(t, `credentials: {snow: {type: basic, user: vigilante, password_ref: "env:VGL_SNOW_PW"}}
itsm:
  servicenow:
    url: `+snow.Srv.URL+`
    credential: snow
    change_gate: {enabled: true, services: [svc]}
    incidents: {enabled: true, assignment_group: SRE}
`)
	c := &v2Client{t: t, base: base, token: "tok"}
	c.do("PUT", "/v2/services/svc/last-good", `{"version":"v1"}`, nil)

	r := c.do("POST", "/v2/deployments", `{"service":"svc","version":"v2"}`, nil)
	wantProblem(t, r, 409, "change_ticket_invalid")
	r = c.do("POST", "/v2/deployments", `{"service":"svc","version":"v2","change_ticket":"CHG200"}`, nil)
	wantProblem(t, r, 409, "change_ticket_invalid")
	if !strings.Contains(r.JSON["detail"].(string), "not approved") {
		t.Fatalf("detail: %s", r.Body)
	}
	// Services outside the gate need no ticket.
	c.do("PUT", "/v2/services/zeta/last-good", `{"version":"z1"}`, nil)
	if r := c.do("POST", "/v2/deployments", `{"service":"zeta","version":"z2"}`, nil); r.StatusCode != 201 {
		t.Fatalf("ungated service: %d %s", r.StatusCode, r.Body)
	}

	r = c.do("POST", "/v2/deployments", `{"id":"chg-1","service":"svc","version":"v2"}`, map[string]string{"X-Change-Ticket": "CHG100"})
	if r.StatusCode != 201 || r.JSON["change_ticket"].(map[string]any)["sys_id"] != "sys100" {
		t.Fatalf("valid ticket: %d %s", r.StatusCode, r.Body)
	}
	// The canary fails and is rolled back; the ticket gets the story.
	op := c.do("POST", "/v2/deployments/chg-1/observations", `{"phase":"canary"}`, nil)
	waitOp(t, c, op.JSON["id"].(string))
	deadline := time.Now().Add(10 * time.Second)
	var notes []string
	for time.Now().Before(deadline) {
		_, wn := snow.Snapshot()
		if notes = wn["sys100"]; len(notes) >= 4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"observation started", "phase FAILED", "rollback started", "rollback completed"} {
		if !strings.Contains(joined, want) {
			t.Errorf("work notes lack %q:\n%s", want, joined)
		}
	}

	// ServiceNow down: fail closed by default (503 + Retry-After).
	snow.SetDown(true)
	r = c.do("POST", "/v2/deployments", `{"service":"svc","version":"v3","change_ticket":"CHG100"}`, nil)
	wantProblem(t, r, 503, "itsm_unavailable")
	if r.Header.Get("Retry-After") == "" {
		t.Fatal("Retry-After missing")
	}
	// fail open: proceeds, marked unverified
	s.E.ITSM = itsm.NewServiceNow(func() config.ServiceNow {
		sn := *s.E.Cfg.ITSM.ServiceNow
		sn.ChangeGate.OnError = "open"
		return sn
	}(), s.E.Cfg.Credentials)
	r = c.do("POST", "/v2/deployments", `{"service":"svc","version":"v3","change_ticket":"CHG100"}`, nil)
	if r.StatusCode != 201 || r.JSON["change_ticket"].(map[string]any)["unverified"] != true {
		t.Fatalf("fail open: %d %s", r.StatusCode, r.Body)
	}
	snow.SetDown(false)

	// An open circuit raises one incident.
	c.do("POST", "/v2/circuit/trip", `{"reason":"two failed rollbacks"}`, nil)
	deadline = time.Now().Add(10 * time.Second)
	var inc []map[string]any
	for time.Now().Before(deadline) {
		if inc, _ = snow.Snapshot(); len(inc) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(inc) != 1 || !strings.Contains(inc[0]["short_description"].(string), "circuit breaker OPEN") || inc[0]["assignment_group"] != "SRE" {
		t.Fatalf("incident: %v", inc)
	}
}

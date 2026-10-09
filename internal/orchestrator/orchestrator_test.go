package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"vigilante/internal/config"
	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/safety"
	"vigilante/internal/store"
	"vigilante/internal/transport"
)

// fakeApp is an HTTP health endpoint whose health flips when "rolled back".
type fakeApp struct {
	healthy atomic.Bool
	srv     *httptest.Server
}

func newFakeApp(healthy bool) *fakeApp {
	a := &fakeApp{}
	a.healthy.Store(healthy)
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.healthy.Load() {
			w.WriteHeader(500)
		}
	}))
	return a
}

// scriptRunner is a Runner whose behaviour is a function of the command.
type scriptRunner struct {
	name string
	mu   sync.Mutex
	cmds []string
	fn   func(cmd string, stdin string) (string, error)
}

func (s *scriptRunner) String() string { return s.name }
func (s *scriptRunner) Run(_ context.Context, cmd string, stdin io.Reader) (string, error) {
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	s.mu.Lock()
	s.cmds = append(s.cmds, cmd)
	s.mu.Unlock()
	return s.fn(cmd, in)
}
func (s *scriptRunner) Stream(ctx context.Context, _ string, _ func(string)) error {
	<-ctx.Done()
	return nil
}
func (s *scriptRunner) Dial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("no dial")
}
func (s *scriptRunner) joined() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.cmds, "\n----\n")
}

// fakeLB keeps an nginx upstream file in memory.
type fakeLB struct {
	scriptRunner
	conf string
}

func newFakeLB(conf string) *fakeLB {
	lb := &fakeLB{conf: conf}
	lb.name = "lb"
	lb.fn = func(cmd, stdin string) (string, error) {
		if strings.HasPrefix(cmd, "cat ") {
			return lb.conf, nil
		}
		if strings.Contains(cmd, "nginx -t") {
			lb.conf = stdin
		}
		return "", nil
	}
	return lb
}

var downRe = regexp.MustCompile(`server (\S+) down;`)

func (lb *fakeLB) down() []string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	var out []string
	for _, m := range downRe.FindAllStringSubmatch(lb.conf, -1) {
		out = append(out, m[1])
	}
	return out
}

const baseConfig = `
version: v1
server: {journal_path: %[1]q}
targets:
  - {name: app-1, address: 10.0.0.1, labels: {url: %[2]q}, connection: {type: local}}
  - {name: app-2, address: 10.0.0.2, labels: {url: %[3]q}, connection: {type: local}}
  - {name: lb-1, address: 10.0.0.100, connection: {type: local}}
traffic:
  lb:
    type: nginx
    nginx: {hosts: [lb-1], upstream_file: /etc/nginx/up.conf, member_format: "{{.Address}}:8080"}
executors:
  link:
    type: symlink
    symlink: {link: /opt/app/current, releases_dir: /opt/app/releases, init: none}
  esc:
    type: exec
    exec: {rollback: "escalate {{.Name}}"}
services:
  - name: order
    targets: [app-1, app-2]
    probes:
      - {id: health, type: http, interval: 30ms, timeout: 500ms, http: {url: "{{.Labels.url}}"}}
    rules:
      - name: down
        when: {metric: health.consecutive_failures, op: ">=", value: 2}
    phases:
      canary: {targets: [app-1], observation_window: 1500ms, warmup: 0s, eval_interval: 40ms}
      full: {observation_window: 600ms, eval_interval: 40ms}
    rollback:
      executor: link
      traffic: lb
      retry: {attempts: 2, backoff: 10ms}
      step_timeout: 5s
      plan:
        - action: traffic.drain
        - action: app.rollback
        - action: app.verify
        - {action: probe.verify, probe: health, successes: 2}
        - action: traffic.enable
%[4]s
safety:
  circuit_breaker: {failure_threshold: 2, window: 1h}
  blast_radius: {min_healthy: 1}
  flapping: {max_rollbacks_per_hour: %[5]d}
`

type harness struct {
	t       *testing.T
	e       *Engine
	app1    *fakeApp
	app2    *fakeApp
	lb      *fakeLB
	host    *scriptRunner
	journal string
	cfg     *config.Config
}

type opts struct {
	escalation string
	maxPerHour int
	upstream   string
	linkFails  bool
	escFails   bool
	journal    string
	app1, app2 *fakeApp
	store      store.Store // shared state store (HA tests); default: file journal
	owner      string
}

func newHarness(t *testing.T, o opts) *harness {
	t.Helper()
	h := &harness{t: t}
	if o.journal == "" {
		o.journal = filepath.Join(t.TempDir(), "journal.jsonl")
	}
	h.journal = o.journal
	if o.maxPerHour == 0 {
		o.maxPerHour = 10
	}
	if o.upstream == "" {
		o.upstream = "upstream order {\n  server 10.0.0.1:8080;\n  server 10.0.0.2:8080;\n}\n"
	}
	h.app1, h.app2 = o.app1, o.app2
	if h.app1 == nil {
		h.app1 = newFakeApp(true)
		t.Cleanup(h.app1.srv.Close)
	}
	if h.app2 == nil {
		h.app2 = newFakeApp(true)
		t.Cleanup(h.app2.srv.Close)
	}
	raw := fmt.Sprintf(baseConfig, o.journal, h.app1.srv.URL, h.app2.srv.URL, o.escalation, o.maxPerHour)
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	h.lb = newFakeLB(o.upstream)
	h.host = &scriptRunner{name: "app", fn: func(cmd, _ string) (string, error) {
		switch {
		case strings.Contains(cmd, "ln -sfn"):
			if o.linkFails {
				return "", errors.New("exit status 1: No such file or directory")
			}
			h.app1.healthy.Store(true)
			h.app2.healthy.Store(true)
		case strings.HasPrefix(cmd, "escalate"):
			if o.escFails {
				return "", errors.New("escalation failed")
			}
			h.app1.healthy.Store(true)
		case strings.HasPrefix(cmd, "readlink -f"):
			return "/opt/app/releases/v1\n", nil
		}
		return "", nil
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("VIGILANTE_TEST_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	e, err := New(cfg, Options{Log: log, Store: o.store, Owner: o.owner, Runners: func(target string) (transport.Runner, error) {
		if target == "lb-1" {
			return h.lb, nil
		}
		return h.host, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	h.e = e
	return h
}

func (h *harness) deploy(id string) *model.Deployment {
	d, err := h.e.Create(id, "order", "v2", "v1")
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func TestCanaryPassPromotes(t *testing.T) {
	h := newHarness(t, opts{})
	d := h.deploy("d1")
	if err := h.e.Watch(context.Background(), d, model.PhaseCanary); err != nil {
		t.Fatal(err)
	}
	if d.State != model.StatePromoted || model.ExitCode(d) != 0 {
		t.Fatalf("state %s: %s", d.State, d.Reason)
	}
	if strings.Contains(h.lb.joined(), "nginx") {
		t.Fatal("LB touched on a passing canary")
	}
}

func TestCanaryFailDrainsRollsBackAndEnables(t *testing.T) {
	h := newHarness(t, opts{})
	h.app1.healthy.Store(false) // the new version on the canary is broken
	d := h.deploy("d2")
	if err := h.e.Watch(context.Background(), d, model.PhaseCanary); err != nil {
		t.Fatal(err)
	}
	if d.State != model.StateRolledBack || model.ExitCode(d) != 2 {
		t.Fatalf("state %s: %s", d.State, d.Reason)
	}
	if len(d.Breaches) == 0 || d.Breaches[0].Target != "app-1" {
		t.Fatalf("breaches %+v", d.Breaches)
	}
	lbCmds := h.lb.joined()
	if strings.Count(lbCmds, "nginx -s reload") != 2 {
		t.Fatalf("expected drain + enable reloads:\n%s", lbCmds)
	}
	if len(h.lb.down()) != 0 {
		t.Fatalf("target left drained: %v", h.lb.down())
	}
	if !strings.Contains(h.host.joined(), "ln -sfn '/opt/app/releases/v1'") {
		t.Fatal(h.host.joined())
	}
	// Journal holds the full story.
	st, err := journal.Replay(h.journal)
	if err != nil {
		t.Fatal(err)
	}
	if st.Deployments["d2"].State != model.StateRolledBack || st.StepsDone["d2"]["app-1"] != 5 || len(st.Rollbacks["order"]) != 1 {
		t.Fatalf("journal state %+v steps %+v", st.Deployments["d2"].State, st.StepsDone)
	}
}

func TestEscalationRecovers(t *testing.T) {
	h := newHarness(t, opts{linkFails: true, escalation: "      escalation: [{executor: esc}]"})
	h.app1.healthy.Store(false)
	d := h.deploy("d3")
	_ = h.e.Watch(context.Background(), d, model.PhaseCanary)
	if d.State != model.StateRolledBack {
		t.Fatalf("state %s: %s", d.State, d.Reason)
	}
	if !strings.Contains(h.host.joined(), "escalate app-1") {
		t.Fatal("escalation not executed")
	}
	if len(h.lb.down()) != 0 {
		t.Fatal("target should be re-enabled after a successful escalation")
	}
	if h.e.Breaker.State().State != safety.Closed {
		t.Fatal("successful rollback must not count as a failure")
	}
}

func TestRollbackFailureIsolatesAndOpensCircuit(t *testing.T) {
	h := newHarness(t, opts{linkFails: true, escFails: true, escalation: "      escalation: [{executor: esc}]"})
	for i, id := range []string{"f1", "f2"} {
		h.app1.healthy.Store(false)
		d := h.deploy(id)
		_ = h.e.Watch(context.Background(), d, model.PhaseCanary)
		if d.State != model.StateRollbackFailed || model.ExitCode(d) != 3 {
			t.Fatalf("#%d state %s: %s", i, d.State, d.Reason)
		}
		if got := h.lb.down(); len(got) != 1 || got[0] != "10.0.0.1:8080" {
			t.Fatalf("failed target must stay drained, down=%v", got)
		}
	}
	if st := h.e.Breaker.State(); st.State != safety.Open {
		t.Fatalf("circuit %s after 2 failed rollbacks", st.State)
	}
	// The gate is closed for the next deployment...
	err := h.e.Watch(context.Background(), h.deploy("f3"), model.PhaseCanary)
	if !errors.Is(err, safety.ErrCircuitOpen) {
		t.Fatalf("expected circuit-open gate, got %v", err)
	}
	// ...and the OPEN state survives a restart (persisted in the journal).
	h2 := newHarness(t, opts{journal: h.journal, app1: h.app1, app2: h.app2})
	if h2.e.Breaker.State().State != safety.Open {
		t.Fatal("circuit state not restored from journal")
	}
}

func TestApprovalGate(t *testing.T) {
	h := newHarness(t, opts{linkFails: true, escalation: "      escalation: [{executor: esc, require_approval: true}]"})
	h.app1.healthy.Store(false)
	d := h.deploy("a1")
	_ = h.e.Watch(context.Background(), d, model.PhaseCanary)
	if d.State != model.StateAwaitApproval || model.ExitCode(d) != 3 {
		t.Fatalf("state %s: %s", d.State, d.Reason)
	}
	if strings.Contains(h.host.joined(), "escalate") {
		t.Fatal("gated escalation ran without approval")
	}
	if h.e.Breaker.State().Failures != nil {
		t.Fatal("awaiting approval is not a rollback failure")
	}
	_ = h.e.Rollback(context.Background(), d, RollbackOptions{Manual: true, Approved: true, Reason: "approved"})
	if d.State != model.StateRolledBack {
		t.Fatalf("after approval: %s %s", d.State, d.Reason)
	}
}

func TestResumeSkipsCompletedSteps(t *testing.T) {
	dir := t.TempDir()
	jpath := filepath.Join(dir, "journal.jsonl")
	h := newHarness(t, opts{journal: jpath})
	d := h.deploy("r1")
	d.Targets = []string{"app-1"}
	h.e.setState(d, model.StateRollingBack, "rollback in progress")
	// Simulate a crash after the drain and the symlink switch completed.
	h.e.Journal.Append(context.Background(), journal.Entry{Kind: journal.KindStepDone, DeployID: "r1", Target: "app-1", Step: 0})
	h.e.Journal.Append(context.Background(), journal.Entry{Kind: journal.KindStepDone, DeployID: "r1", Target: "app-1", Step: 1})
	h.e.Close()

	// The LB still has app-1 drained from the interrupted attempt.
	h2 := newHarness(t, opts{journal: jpath, app1: h.app1, app2: h.app2,
		upstream: "upstream order {\n  server 10.0.0.1:8080 down;\n  server 10.0.0.2:8080;\n}\n"})
	if n := h2.e.Resume(context.Background()); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	got, _ := h2.e.Deployment("r1")
	if got.State != model.StateRolledBack {
		t.Fatalf("state %s: %s", got.State, got.Reason)
	}
	if strings.Contains(h2.host.joined(), "ln -sfn") {
		t.Fatal("completed app.rollback step re-executed")
	}
	if len(h2.lb.down()) != 0 {
		t.Fatalf("traffic.enable not executed on resume: still down %v", h2.lb.down())
	}
}

func TestBlastRadiusRefusesDrain(t *testing.T) {
	// app-2 is already out of rotation: draining app-1 would empty the pool.
	h := newHarness(t, opts{upstream: "upstream order {\n  server 10.0.0.1:8080;\n  server 10.0.0.2:8080 down;\n}\n"})
	h.app1.healthy.Store(false)
	d := h.deploy("b1")
	_ = h.e.Watch(context.Background(), d, model.PhaseCanary)
	if d.State != model.StateRolledBack {
		t.Fatalf("state %s: %s", d.State, d.Reason)
	}
	if strings.Contains(h.lb.joined(), "nginx -s reload") {
		t.Fatal("LB was modified although draining would breach min_healthy")
	}
	found := false
	for _, ev := range d.Events {
		if ev.Kind == "blast-radius" {
			found = true
		}
	}
	if !found {
		t.Fatal("blast-radius decision not recorded")
	}
}

func TestFlappingGuardBlocksAndIsolates(t *testing.T) {
	h := newHarness(t, opts{maxPerHour: 1})
	h.app1.healthy.Store(false)
	d1 := h.deploy("p1")
	_ = h.e.Watch(context.Background(), d1, model.PhaseCanary)
	if d1.State != model.StateRolledBack {
		t.Fatalf("first: %s", d1.State)
	}
	h.app1.healthy.Store(false)
	d2 := h.deploy("p2")
	_ = h.e.Watch(context.Background(), d2, model.PhaseCanary)
	if d2.State != model.StateRollbackFailed || !strings.Contains(d2.Reason, "flapping") {
		t.Fatalf("second: %s %s", d2.State, d2.Reason)
	}
	if got := h.lb.down(); len(got) != 1 {
		t.Fatalf("failing canary should be isolated, down=%v", got)
	}
}

func TestEnvironmentalProblemHolds(t *testing.T) {
	h := newHarness(t, opts{})
	h.cfg.Services[0].ControlTargets = []string{"auto"}
	h.app1.healthy.Store(false)
	h.app2.healthy.Store(false) // the untouched control target fails too (e.g. shared DB down)
	d := h.deploy("e1")
	_ = h.e.Watch(context.Background(), d, model.PhaseCanary)
	if d.State != model.StateHeld || model.ExitCode(d) != 4 {
		t.Fatalf("state %s: %s", d.State, d.Reason)
	}
	if strings.Contains(h.host.joined(), "ln -sfn") {
		t.Fatal("rolled back for an environmental problem")
	}
}

func TestLastGoodVersionAndMarkGood(t *testing.T) {
	h := newHarness(t, opts{})
	if _, _, ok := h.e.LastGoodVersion("order"); ok {
		t.Fatal("empty journal has no known-good version")
	}
	if _, err := h.e.MarkGood("order", "v1", "baseline before vigilante"); err != nil {
		t.Fatal(err)
	}
	if v, _, ok := h.e.LastGoodVersion("order"); !ok || v != "v1" {
		t.Fatalf("after mark-good: %q %v", v, ok)
	}
	// A rolled-back deployment never becomes the known-good version.
	h.app1.healthy.Store(false)
	bad := h.deploy("bad-1")
	_ = h.e.Watch(context.Background(), bad, model.PhaseCanary)
	if bad.State != model.StateRolledBack {
		t.Fatalf("setup: %s", bad.State)
	}
	// A full phase that passes does.
	good, _ := h.e.Create("good-1", "order", "v3", "v1")
	_ = h.e.Watch(context.Background(), good, model.PhaseFull)
	if good.State != model.StateSucceeded {
		t.Fatalf("setup: %s %s", good.State, good.Reason)
	}
	if v, from, _ := h.e.LastGoodVersion("order"); v != "v3" || from != "good-1" {
		t.Fatalf("got %q from %q", v, from)
	}
	// It survives a restart (read back from the journal).
	h2 := newHarness(t, opts{journal: h.journal, app1: h.app1, app2: h.app2})
	if v, _, _ := h2.e.LastGoodVersion("order"); v != "v3" {
		t.Fatalf("after restart: %q", v)
	}
}

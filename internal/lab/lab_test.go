package lab

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vigilante/internal/config"
	"vigilante/internal/executor"
	"vigilante/internal/orchestrator"
)

// app is the system under test: healthy on v1, failing on v2.
type app struct {
	mu      sync.Mutex
	version string
}

func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.version != "v1" {
			w.WriteHeader(500)
		}
	})
	mux.HandleFunc("/deploy", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.version = r.URL.Query().Get("version")
		a.mu.Unlock()
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.version != r.URL.Query().Get("expect") {
			w.WriteHeader(409)
		}
	})
	return mux
}

// lb is an in-memory load balancer pool.
type lb struct {
	mu        sync.Mutex
	enabled   map[string]bool
	stuckOff  bool // Enable fails: the member stays out of traffic
	drainSeen bool
}

func (l *lb) MemberID(m executor.Member) (string, error) { return m.Target, nil }
func (l *lb) Drain(_ context.Context, ms []executor.Member) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range ms {
		l.enabled[m.Target] = false
	}
	l.drainSeen = true
	return nil
}
func (l *lb) Enable(_ context.Context, ms []executor.Member) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stuckOff {
		return nil // reports success but leaves the member out: the lab must notice
	}
	for _, m := range ms {
		l.enabled[m.Target] = true
	}
	return nil
}
func (l *lb) Pool(context.Context) ([]executor.PoolMember, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []executor.PoolMember
	for _, id := range []string{"a", "b"} {
		out = append(out, executor.PoolMember{ID: id, Enabled: l.enabled[id]})
	}
	return out, nil
}

var pool = &lb{}

func init() {
	executor.RegisterTraffic("labtest", func(string, config.Traffic, executor.TrafficEnv) (executor.TrafficController, error) {
		return pool, nil
	})
}

func engine(t *testing.T, appURL string) *orchestrator.Engine {
	cfg, err := config.Parse([]byte(`version: v1
server: {journal_path: ` + filepath.ToSlash(filepath.Join(t.TempDir(), "j.jsonl")) + `}
targets: [{name: a, address: 127.0.0.1, connection: {type: local}}]
executors:
  deploy: {type: webhook, webhook: {url: "` + appURL + `/deploy?version={{.PreviousVersion}}", verify_url: "` + appURL + `/version?expect={{.PreviousVersion}}"}}
services:
  - name: web
    targets: [a]
    probes: [{id: h, type: http, interval: 50ms, timeout: 200ms, http: {url: "` + appURL + `/health"}}]
    rules: [{name: down, when: {metric: h.consecutive_failures, op: ">=", value: 3}}]
    phases: {canary: {observation_window: 5s, eval_interval: 100ms}}
    rollback: {executor: deploy, mode: approve}
`))
	if err != nil {
		t.Fatal(err)
	}
	// A traffic controller the validator does not know (registered above).
	cfg.Traffic = map[string]config.Traffic{"lb": {Type: "labtest"}}
	sv, _ := cfg.Service("web")
	sv.Rollback.Traffic = "lb"
	sv.Rollback.Plan = []config.Step{{Action: "traffic.drain"}, {Action: "app.rollback"}, {Action: "app.verify"}, {Action: "traffic.enable"}}
	e, err := orchestrator.New(cfg, orchestrator.Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestScenarioPassesAndSummarizes(t *testing.T) {
	a := &app{version: "v1"}
	srv := httptest.NewServer(a.handler())
	defer srv.Close()
	pool.enabled, pool.stuckOff, pool.drainSeen = map[string]bool{"a": true, "b": true}, false, false
	e := engine(t, srv.URL)
	inject := func(ctx context.Context, cmd string) (string, error) {
		_, err := http.Get(srv.URL + "/deploy?version=" + cmd)
		return "deployed " + cmd, err
	}
	opt := Options{Service: "web", Version: "v2", Previous: "v1", Inject: "v2", Label: "test LB 1.0", Shell: inject}
	r := Run(context.Background(), e, opt, 1)
	if !r.Passed || r.FinalState != "ROLLED_BACK" || !pool.drainSeen {
		t.Fatalf("scenario: %+v", r)
	}
	names := []string{}
	for _, s := range r.Steps {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "checkpoint,pool before,inject bad version,observe and roll back,pool after" {
		t.Fatalf("steps %s", got)
	}
	if r.TimeToFail <= 0 || r.RollbackSeconds < 0 || strings.Join(r.Plugins, ",") != "executor:webhook,probe:http,traffic:labtest" {
		t.Fatalf("timings/plugins: %+v", r)
	}
	a.mu.Lock()
	restored := a.version
	a.mu.Unlock()
	if restored != "v1" {
		t.Fatalf("app not restored: %s", restored)
	}

	// The load balancer claims success but leaves the member out: the lab fails the run.
	pool.stuckOff = true
	r2 := Run(context.Background(), e, opt, 2)
	if r2.Passed || r2.Steps[len(r2.Steps)-1].Name != "pool after" || !strings.Contains(r2.Steps[len(r2.Steps)-1].Detail, "not back in traffic: a") {
		t.Fatalf("stuck member not detected: %+v", r2.Steps)
	}

	s := Summarize([]Result{r, r2})
	if len(s) != 1 || s[0].Runs != 2 || s[0].Passed != 1 || len(s[0].Problems) != 1 {
		t.Fatalf("summary %+v", s)
	}
	if md := Markdown(s); !strings.Contains(md, "| test LB 1.0 | web |") || !strings.Contains(md, "| 2 | 1 |") {
		t.Fatalf("markdown:\n%s", md)
	}
}

func TestInjectFailureStopsAndResets(t *testing.T) {
	a := &app{version: "v1"}
	srv := httptest.NewServer(a.handler())
	defer srv.Close()
	pool.enabled, pool.stuckOff = map[string]bool{"a": true, "b": true}, false
	e := engine(t, srv.URL)
	var resets int
	shell := func(_ context.Context, cmd string) (string, error) {
		if cmd == "reset" {
			resets++
			return "", nil
		}
		return "permission denied", errors.New("exit status 1")
	}
	r := Run(context.Background(), e, Options{Service: "web", Previous: "v1", Inject: "deploy-v2", Reset: "reset", Shell: shell}, 1)
	last := r.Steps[len(r.Steps)-1]
	if r.Passed || last.Name != "reset" || resets != 1 || !strings.Contains(r.Steps[len(r.Steps)-2].Detail, "permission denied") {
		t.Fatalf("result %+v", r.Steps)
	}
}

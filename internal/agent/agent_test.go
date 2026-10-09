package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/model"
)

func TestAgentPushesAndFailsafeRollsBack(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "rolled-back")
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer app.Close()

	var pushed atomic.Int64
	var orchestratorUp atomic.Bool
	orchestratorUp.Store(true)
	var mu sync.Mutex
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !orchestratorUp.Load() {
			w.WriteHeader(503)
			return
		}
		switch {
		case r.URL.Path == "/v1/samples":
			var s []model.Sample
			json.NewDecoder(r.Body).Decode(&s)
			pushed.Add(int64(len(s)))
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			mu.Lock()
			defer mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"active": model.Deployment{ID: "d1", Service: "svc", Version: "v2", PreviousVersion: "v1", Targets: []string{"a"}}})
		}
	}))
	defer orch.Close()

	cfg, err := config.Parse([]byte(fmt.Sprintf(`
version: v1
server: {journal_path: %q}
agent: {push_interval: 50ms, heartbeat_interval: 50ms, failsafe_after: 300ms, failsafe: rollback}
targets: [{name: a, address: 127.0.0.1}]
executors:
  x: {type: exec, exec: {rollback: "echo rolled > %s", on: local}}
services:
  - name: svc
    targets: [a]
    probes: [{id: h, type: http, interval: 30ms, http: {url: %q}}]
    rules: [{name: down, when: {metric: h.consecutive_failures, op: ">=", value: 3}}]
    rollback: {executor: x}
`, filepath.ToSlash(filepath.Join(dir, "j.jsonl")), filepath.ToSlash(marker), app.URL)))
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Cfg: cfg, Target: "a", Server: orch.URL, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go a.Run(ctx)

	time.Sleep(400 * time.Millisecond)
	if pushed.Load() == 0 {
		t.Fatal("agent did not push samples")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("failsafe must not act while the orchestrator is reachable")
	}
	orchestratorUp.Store(false) // orchestrator dies mid-canary
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("agent failsafe did not roll back the local target")
}

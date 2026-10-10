//go:build load

// Package load is the M5-4 load harness: one Vigilante engine observes
// many simulated targets for many concurrent deployments, some of which go
// bad, and the run measures what the non-functional goals ask about:
// decision latency (failure injected -> rollback request), wrong verdicts,
// memory, CPU and probe throughput.
//
//	go test -tags load ./test/load -run TestLoad -timeout 30m -v \
//	  -args -targets 2000 -deployments 100 -failing 10 -out load-result.json
//
// Targets are HTTP endpoints of one in-process simulator (no SSH): it
// exercises collection, evaluation, the state store and the rollback path,
// not sshd. The SSH session budget is covered by its own tests.
package load

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/model"
	"vigilante/internal/orchestrator"
)

var (
	nTargets  = flag.Int("targets", 2000, "simulated targets")
	nDeploys  = flag.Int("deployments", 100, "concurrent deployments (one service each)")
	nFailing  = flag.Int("failing", 10, "deployments whose new version goes bad")
	nProbes   = flag.Int("probes", 3, "HTTP probes per target")
	interval  = flag.Duration("interval", 2*time.Second, "probe interval")
	window    = flag.Duration("window", 30*time.Second, "observation window")
	evalEvery = flag.Duration("eval", time.Second, "evaluation interval")
	injectAt  = flag.Duration("inject", 8*time.Second, "when the bad versions break, after the start")
	outFile   = flag.String("out", "", "write the result as JSON here")
)

// sim answers every probe; a service's targets fail once it is broken, and
// its rollback endpoint records when the rollback was requested.
type sim struct {
	mu         sync.Mutex
	broken     map[string]bool
	rollbackAt map[string]time.Time
	requests   atomic.Int64
}

func (s *sim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // kind/service/...
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	svc := parts[1]
	s.mu.Lock()
	defer s.mu.Unlock()
	switch parts[0] {
	case "probe":
		if s.broken[svc] {
			w.WriteHeader(http.StatusInternalServerError)
		}
	case "rollback":
		if _, ok := s.rollbackAt[svc]; !ok {
			s.rollbackAt[svc] = time.Now()
		}
		s.broken[svc] = false
	case "verify":
	default:
		http.NotFound(w, r)
	}
}

func buildConfig(dir string, bases []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "version: v1\nserver: {journal_path: %q}\ntargets:\n", filepath.ToSlash(filepath.Join(dir, "journal.jsonl")))
	for i := 0; i < *nTargets; i++ {
		fmt.Fprintf(&b, "  - {name: t%05d, address: 127.0.0.1, connection: {type: none}}\n", i)
	}
	b.WriteString("executors:\n")
	per := *nTargets / *nDeploys
	for d := 0; d < *nDeploys; d++ {
		base := bases[d]
		fmt.Fprintf(&b, "  x%03d: {type: webhook, webhook: {url: %q, verify_url: %q}}\n", d,
			fmt.Sprintf("%s/rollback/s%03d", base, d), fmt.Sprintf("%s/verify/s%03d", base, d))
	}
	b.WriteString("services:\n")
	for d := 0; d < *nDeploys; d++ {
		fmt.Fprintf(&b, "  - name: s%03d\n    targets: [", d)
		for i := 0; i < per; i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "t%05d", d*per+i)
		}
		b.WriteString("]\n    probes:\n")
		base := bases[d]
		for p := 0; p < *nProbes; p++ {
			fmt.Fprintf(&b, "      - {id: p%d, type: http, interval: %s, timeout: 1s, http: {url: \"%s/probe/s%03d/{{.Name}}/%d\"}}\n",
				p, *interval, base, d, p)
		}
		fmt.Fprintf(&b, "    rules: [{name: down, when: {metric: p0.consecutive_failures, op: \">=\", value: 3}}]\n")
		fmt.Fprintf(&b, "    phases: {canary: {percent: 100, observation_window: %s, warmup: 0s, eval_interval: %s, min_samples: 1}}\n", *window, *evalEvery)
		fmt.Fprintf(&b, "    rollback: {executor: x%03d, mode: auto, step_timeout: 10s}\n", d)
	}
	b.WriteString("safety:\n  circuit_breaker: {failure_threshold: 1000, window: 1h}\n  flapping: {max_rollbacks_per_hour: 1000}\n")
	return b.String()
}

type Result struct {
	Targets, Deployments, Failing, ProbesPerTarget int
	ProbeInterval, Window, EvalInterval            string
	Duration                                       float64 `json:"duration_seconds"`
	WrongVerdicts                                  []string
	DecisionLatency                                map[string]float64 `json:"decision_latency_seconds"` // p50, p90, p99, max
	LatencyBound                                   float64            `json:"latency_bound_seconds"`
	ProbeRequestsPerSec                            float64
	PeakHeapMiB, PeakSysMiB                        float64
	PeakGoroutines                                 int
	CPUSeconds                                     float64
	CPUCores                                       float64 `json:"avg_cpu_cores"`
	Go, OS                                         string
}

func percentile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	sort.Float64s(xs)
	return xs[int(q*float64(len(xs)-1)+0.5)]
}

// cpuSeconds is the CPU time this process used: the runtime's total
// (GOMAXPROCS x wall time) minus idle time. An estimate, as the runtime notes.
func cpuSeconds() float64 {
	s := []metrics.Sample{{Name: "/cpu/classes/total:cpu-seconds"}, {Name: "/cpu/classes/idle:cpu-seconds"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindFloat64 || s[1].Value.Kind() != metrics.KindFloat64 {
		return 0
	}
	return s[0].Value.Float64() - s[1].Value.Float64()
}

func TestLoad(t *testing.T) {
	if *nDeploys <= 0 || *nTargets%*nDeploys != 0 || *nFailing > *nDeploys {
		t.Fatalf("targets (%d) must divide evenly into deployments (%d); failing <= deployments", *nTargets, *nDeploys)
	}
	// One listener per service, like services on their own hosts: a single
	// listener would measure one socket's accept queue, not Vigilante.
	s := &sim{broken: map[string]bool{}, rollbackAt: map[string]time.Time{}}
	bases := make([]string, *nDeploys)
	for d := range bases {
		srv := httptest.NewServer(s)
		defer srv.Close()
		bases[d] = srv.URL
	}

	cfg, err := config.Parse([]byte(buildConfig(t.TempDir(), bases)))
	if err != nil {
		t.Fatal(err)
	}
	e, err := orchestrator.New(cfg, orchestrator.Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// Resource sampling.
	var peakHeap, peakSys uint64
	var peakG int
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				runtime.ReadMemStats(&ms)
				peakHeap, peakSys = max(peakHeap, ms.HeapAlloc), max(peakSys, ms.Sys)
				peakG = max(peakG, runtime.NumGoroutine())
			}
		}
	}()

	cpu0, start := cpuSeconds(), time.Now()
	failing := map[string]bool{}
	for d := 0; d < *nFailing; d++ {
		failing[fmt.Sprintf("s%03d", d*(*nDeploys / *nFailing))] = true // spread over the set
	}
	deps := make([]*model.Deployment, *nDeploys)
	for d := range deps {
		if deps[d], err = e.Create(fmt.Sprintf("load-%03d", d), fmt.Sprintf("s%03d", d), "v2", "v1"); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, d := range deps {
		wg.Add(1)
		go func(d *model.Deployment) {
			defer wg.Done()
			if err := e.Watch(context.Background(), d, model.PhaseCanary); err != nil {
				t.Errorf("%s: %v", d.ID, err)
			}
		}(d)
	}
	time.Sleep(*injectAt)
	injected := time.Now()
	s.mu.Lock()
	for svc := range failing {
		s.broken[svc] = true
	}
	s.mu.Unlock()
	wg.Wait()
	elapsed := time.Since(start)
	cpu := cpuSeconds() - cpu0
	close(stop)
	sampler.Wait()

	res := Result{Targets: *nTargets, Deployments: *nDeploys, Failing: *nFailing, ProbesPerTarget: *nProbes,
		ProbeInterval: interval.String(), Window: window.String(), EvalInterval: evalEvery.String(),
		Duration: elapsed.Seconds(), DecisionLatency: map[string]float64{},
		ProbeRequestsPerSec: float64(s.requests.Load()) / elapsed.Seconds(),
		PeakHeapMiB:         float64(peakHeap) / (1 << 20), PeakSysMiB: float64(peakSys) / (1 << 20), PeakGoroutines: peakG,
		CPUSeconds: cpu, CPUCores: cpu / elapsed.Seconds(), Go: runtime.Version(), OS: runtime.GOOS + "/" + runtime.GOARCH}
	// Bound from the non-functional goal: eval_interval x for + 5s, where the
	// rule needs 3 consecutive failed probes (3 probe intervals) to hold.
	res.LatencyBound = (3*(*interval) + *evalEvery + 5*time.Second).Seconds()
	var lat []float64
	s.mu.Lock()
	for _, d := range deps {
		cp, _ := e.Deployment(d.ID)
		want := model.StatePromoted
		if failing[d.Service] {
			want = model.StateRolledBack
			if at, ok := s.rollbackAt[d.Service]; ok {
				lat = append(lat, at.Sub(injected).Seconds())
			}
		}
		if cp.State != want {
			res.WrongVerdicts = append(res.WrongVerdicts, fmt.Sprintf("%s: %s, want %s (%s)", d.ID, cp.State, want, cp.Reason))
		}
	}
	s.mu.Unlock()
	for _, q := range []struct {
		name string
		q    float64
	}{{"p50", .5}, {"p90", .9}, {"p99", .99}, {"max", 1}} {
		res.DecisionLatency[q.name] = percentile(append([]float64(nil), lat...), q.q)
	}

	out, _ := json.MarshalIndent(res, "", "  ")
	t.Logf("result:\n%s", out)
	if *outFile != "" {
		if err := os.WriteFile(*outFile, append(out, '\n'), 0o644); err != nil {
			t.Error(err)
		}
	}
	if len(res.WrongVerdicts) > 0 {
		t.Errorf("wrong verdicts: %v", res.WrongVerdicts)
	}
	if len(lat) != *nFailing {
		t.Errorf("rollbacks requested for %d of %d bad deployments", len(lat), *nFailing)
	}
	if p := res.DecisionLatency["p99"]; !(p <= res.LatencyBound) {
		t.Errorf("decision latency p99 %.1fs exceeds the goal %.1fs", p, res.LatencyBound)
	}
}

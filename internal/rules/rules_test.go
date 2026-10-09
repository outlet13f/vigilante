package rules

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"vigilante/internal/config"
	"vigilante/internal/metrics"
	"vigilante/internal/model"
)

func mustRules(t *testing.T, src string) []config.Rule {
	t.Helper()
	cfg := `
version: v1
targets: [{name: web-1}, {name: web-2}]
executors: {x: {type: exec, exec: {rollback: "true"}}}
services:
  - name: svc
    targets: [web-1, web-2]
    probes:
      - {id: health, type: tcp, tcp: {address: "x:1"}}
      - {id: access, type: tcp, tcp: {address: "x:1"}}
      - {id: applog, type: tcp, tcp: {address: "x:1"}}
    rollback: {executor: x}
    rules:
` + src
	c, err := config.Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return c.Services[0].Rules
}

func add(s *metrics.Store, target, metric string, v float64, at time.Time) {
	s.Add(model.Sample{Target: target, Metric: metric, Value: v, Time: at})
}

func TestCompositeAnyRule(t *testing.T) {
	rs := mustRules(t, `
      - name: fatal
        when:
          any:
            - {metric: access.count_5xx, ratio_of: access.requests, window: 30s, op: ">", value: 2}
            - {metric: applog.match.exception, agg: rate, window: 10s, op: ">", value: 10}
            - {metric: health.consecutive_timeouts, op: ">=", value: 3}
`)
	s := metrics.NewStore(time.Hour)
	now := time.Now()
	ev := NewEvaluator(s, nil)
	sc := Scope{Target: "web-1", ServiceTargets: []string{"web-1"}}

	// No data at all -> unknown (not "healthy").
	if r := ev.Eval(rs[0], sc, now); r.State != Unknown {
		t.Fatalf("empty store: %v", r.State)
	}
	// 1% errors, low exception rate -> false
	for i := 0; i < 10; i++ {
		at := now.Add(-time.Duration(i) * time.Second)
		add(s, "web-1", "access.requests", 100, at)
		add(s, "web-1", "access.count_5xx", 1, at)
	}
	add(s, "web-1", "health.consecutive_timeouts", 1, now)
	if r := ev.Eval(rs[0], sc, now); r.State != False {
		t.Fatalf("healthy: %v %s", r.State, r.Detail)
	}
	// 3 consecutive timeouts -> true
	add(s, "web-1", "health.consecutive_timeouts", 3, now.Add(time.Millisecond))
	r := ev.Eval(rs[0], sc, now.Add(time.Millisecond))
	if r.State != True {
		t.Fatalf("timeouts: %v", r.State)
	}
	// Exception storm: 150 matches in 10s = 15/s
	later := now.Add(time.Second)
	add(s, "web-1", "health.consecutive_timeouts", 0, later)
	add(s, "web-1", "applog.match.exception", 150, later)
	if r := ev.Eval(rs[0], sc, later); r.State != True {
		t.Fatalf("exception rate: %v", r.State)
	}
}

func TestErrorRatioWeighted(t *testing.T) {
	rs := mustRules(t, `
      - name: err
        when: {metric: access.count_5xx, ratio_of: access.requests, window: 30s, op: ">", value: 2}
`)
	s := metrics.NewStore(time.Hour)
	now := time.Now()
	// One quiet second with 1/1 errors (100%) must not dominate 999 good requests.
	add(s, "web-1", "access.requests", 1, now.Add(-2*time.Second))
	add(s, "web-1", "access.count_5xx", 1, now.Add(-2*time.Second))
	add(s, "web-1", "access.requests", 999, now.Add(-time.Second))
	add(s, "web-1", "access.count_5xx", 0, now.Add(-time.Second))
	ev := NewEvaluator(s, nil)
	if r := ev.Eval(rs[0], Scope{Target: "web-1"}, now); r.State != False || r.Value > 0.2 {
		t.Fatalf("weighted ratio: %v %.3f", r.State, r.Value)
	}
}

func TestConsecutiveAndHysteresis(t *testing.T) {
	rs := mustRules(t, `
      - name: lat
        when: {metric: health.latency_ms, window: 5s, op: ">", value: 100, for: 3, reset_after: 2}
`)
	s := metrics.NewStore(time.Hour)
	ev := NewEvaluator(s, nil)
	sc := Scope{Target: "web-1"}
	base := time.Now()
	seq := []float64{500, 500, 50, 500} // one blip in between: reset_after=2 keeps the streak
	var last Result
	for i, v := range seq {
		at := base.Add(time.Duration(i) * time.Second)
		add(s, "web-1", "health.latency_ms", v, at)
		last = ev.Eval(rs[0], sc, at)
		if i < 3 && last.State == True {
			t.Fatalf("tripped too early at step %d", i)
		}
	}
	if last.State != True {
		t.Fatalf("expected trip after 3 breaches with a single-OK blip, got %v", last.State)
	}

	// With two OKs in a row the counter resets.
	ev2 := NewEvaluator(s, nil)
	s2 := metrics.NewStore(time.Hour)
	ev2.Store = s2
	for i, v := range []float64{500, 500, 50, 50, 500} {
		at := base.Add(time.Duration(i) * time.Second)
		add(s2, "web-1", "health.latency_ms", v, at)
		if r := ev2.Eval(rs[0], sc, at); r.State == True {
			t.Fatalf("should have reset, tripped at %d", i)
		}
	}
}

func TestBaselineIncrease(t *testing.T) {
	rs := mustRules(t, `
      - name: p99
        when: {metric: health.latency_ms, agg: p99, window: 1m, op: ">", baseline: {increase_pct: 200}, min_value: 50}
`)
	s := metrics.NewStore(time.Hour)
	now := time.Now()
	ev := NewEvaluator(s, nil)
	add(s, "web-1", "health.latency_ms", 100, now)
	if r := ev.Eval(rs[0], Scope{Target: "web-1"}, now); r.State != Unknown {
		t.Fatalf("no baseline should be unknown, got %v", r.State)
	}
	snap := &Snapshot{Values: map[string]float64{"health.latency_ms|p99": 40}}
	ev.Baseline = snap
	// 100 <= 40*3=120 -> false
	if r := ev.Eval(rs[0], Scope{Target: "web-1"}, now); r.State != False {
		t.Fatalf("100ms vs 120ms threshold: %v", r.State)
	}
	add(s, "web-1", "health.latency_ms", 130, now.Add(time.Millisecond))
	if r := ev.Eval(rs[0], Scope{Target: "web-1"}, now.Add(time.Millisecond)); r.State != True {
		t.Fatalf("130ms vs 120ms threshold: %v", r.State)
	}
	// min_value floor: baseline 2ms, now 10ms (+400%) but under 50ms -> not a breach
	snap.Values["health.latency_ms|p99"] = 2
	s2 := metrics.NewStore(time.Hour)
	add(s2, "web-1", "health.latency_ms", 10, now)
	ev2 := NewEvaluator(s2, snap)
	if r := ev2.Eval(rs[0], Scope{Target: "web-1"}, now); r.State != False {
		t.Fatalf("min_value floor ignored: %v", r.State)
	}
}

func TestCaptureAndControlBaseline(t *testing.T) {
	rs := mustRules(t, `
      - name: p99
        when: {metric: health.latency_ms, agg: p99, window: 30s, op: ">", baseline: {increase_pct: 100}}
`)
	s := metrics.NewStore(time.Hour)
	now := time.Now()
	for i := 0; i < 50; i++ {
		add(s, "web-2", "health.latency_ms", 20, now.Add(-time.Duration(i)*time.Second))
	}
	snap := Capture(s, "svc", rs, []string{"web-2"}, 5*time.Minute, now)
	if snap.Values["health.latency_ms|p99"] != 20 || snap.Samples != 50 {
		t.Fatalf("capture: %+v", snap)
	}
	ctl := &Control{Store: s, Targets: []string{"web-2"}}
	add(s, "web-1", "health.latency_ms", 45, now)
	ev := NewEvaluator(s, Chain{ctl, snap})
	if r := ev.Eval(rs[0], Scope{Target: "web-1"}, now); r.State != True {
		t.Fatalf("45 vs control 20 (+100%%): %v", r.State)
	}
}

func TestNotAndAll(t *testing.T) {
	rs := mustRules(t, `
      - name: dead
        when:
          all:
            - {metric: health.up, op: "==", value: 0}
            - not: {metric: applog.lines, agg: sum, window: 10s, op: ">", value: 0}
`)
	s := metrics.NewStore(time.Hour)
	now := time.Now()
	add(s, "web-1", "health.up", 0, now)
	ev := NewEvaluator(s, nil)
	if r := ev.Eval(rs[0], Scope{Target: "web-1"}, now); r.State != True {
		t.Fatalf("down and silent: %v", r.State)
	}
	add(s, "web-1", "applog.lines", 5, now.Add(time.Millisecond))
	if r := ev.Eval(rs[0], Scope{Target: "web-1"}, now.Add(time.Millisecond)); r.State != False {
		t.Fatalf("down but logging: %v", r.State)
	}
}

func TestNodeYAMLShape(t *testing.T) {
	var n config.Node
	if err := yaml.Unmarshal([]byte(`{any: [{metric: a.b, value: 1}, {all: [{metric: a.c, value: 2}]}]}`), &n); err != nil {
		t.Fatal(err)
	}
	if len(n.Any) != 2 || n.Any[1].All[0].Metric != "a.c" {
		t.Fatalf("decoded %+v", n)
	}
}

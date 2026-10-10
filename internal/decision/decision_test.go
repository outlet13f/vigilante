package decision

import (
	"context"
	"testing"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/metrics"
	"vigilante/internal/model"
)

func f(v float64) *float64 { return &v }

func errRule(action string) config.Rule {
	return config.Rule{Name: "err", Action: action, When: config.Node{Condition: config.Condition{
		Metric: "access.error_rate_5xx", Agg: "avg", Window: 10 * time.Second, Op: ">", Value: f(2),
		For: 1, ResetAfter: 1, Scope: "target", Absent: "unknown",
	}}}
}

func phase(rules []config.Rule, deployed, controls []string) Phase {
	return Phase{
		Name:     model.PhaseCanary,
		Cfg:      config.PhaseConfig{ObservationWindow: 150 * time.Millisecond, EvalInterval: 10 * time.Millisecond, OnInconclusive: "hold"},
		Rules:    rules,
		Deployed: deployed,
		Controls: controls,
	}
}

func feed(ctx context.Context, s *metrics.Store, target, metric string, v float64, source string) {
	go func() {
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		for {
			s.Add(model.Sample{Target: target, Metric: metric, Value: v, Time: time.Now(), Source: source})
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func TestFailFast(t *testing.T) {
	s := metrics.NewStore(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feed(ctx, s, "canary-1", "access.error_rate_5xx", 12, "")
	time.Sleep(20 * time.Millisecond)
	out := New(phase([]config.Rule{errRule("rollback")}, []string{"canary-1"}, nil), s, nil).Run(ctx)
	if out.Verdict != model.VerdictFail || len(out.Breaches) != 1 {
		t.Fatalf("got %s: %s", out.Verdict, out.Reason)
	}
	if out.EndedAt.Sub(out.StartedAt) > 100*time.Millisecond {
		t.Fatal("FAIL should end the phase immediately, not wait for the window")
	}
}

func TestPassAfterWindow(t *testing.T) {
	s := metrics.NewStore(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feed(ctx, s, "canary-1", "access.error_rate_5xx", 0.5, "")
	out := New(phase([]config.Rule{errRule("rollback")}, []string{"canary-1"}, nil), s, nil).Run(ctx)
	if out.Verdict != model.VerdictPass {
		t.Fatalf("got %s: %s", out.Verdict, out.Reason)
	}
}

func TestWarmupIgnoresEarlyErrors(t *testing.T) {
	s := metrics.NewStore(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Errors only in the first 40ms (JVM warm-up); window is evaluated over 10s
	// so use a short condition window here.
	r := errRule("rollback")
	r.When.Window = 20 * time.Millisecond
	fctx, stop := context.WithCancel(ctx)
	feed(fctx, s, "canary-1", "access.error_rate_5xx", 50, "")
	go func() {
		time.Sleep(40 * time.Millisecond)
		stop()
		feed(ctx, s, "canary-1", "access.error_rate_5xx", 0, "")
	}()
	p := phase([]config.Rule{r}, []string{"canary-1"}, nil)
	p.Cfg.Warmup = 80 * time.Millisecond
	out := New(p, s, nil).Run(ctx)
	if out.Verdict != model.VerdictPass {
		t.Fatalf("warm-up errors should be ignored, got %s: %s", out.Verdict, out.Reason)
	}
}

func TestEnvironmentalHold(t *testing.T) {
	s := metrics.NewStore(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feed(ctx, s, "canary-1", "access.error_rate_5xx", 30, "")
	feed(ctx, s, "control-1", "access.error_rate_5xx", 30, "") // old version is failing too
	out := New(phase([]config.Rule{errRule("rollback")}, []string{"canary-1"}, []string{"control-1"}), s, nil).Run(ctx)
	if out.Verdict != model.VerdictHold || !out.Breaches[0].Environmental {
		t.Fatalf("got %s: %s", out.Verdict, out.Reason)
	}
}

func TestObserverQuorum(t *testing.T) {
	s := metrics.NewStore(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feed(ctx, s, "canary-1", "access.error_rate_5xx", 30, "central")       // orchestrator sees errors
	feed(ctx, s, "canary-1", "access.error_rate_5xx", 0, "agent:canary-1") // on-host agent sees none
	p := phase([]config.Rule{errRule("rollback")}, []string{"canary-1"}, nil)
	p.Quorum = true
	out := New(p, s, nil).Run(ctx)
	if out.Verdict != model.VerdictHold {
		t.Fatalf("disagreeing observers must HOLD, got %s: %s", out.Verdict, out.Reason)
	}
	// Without quorum the mixed data averages to 15% -> FAIL.
	p.Quorum = false
	if out := New(p, s, nil).Run(ctx); out.Verdict != model.VerdictFail {
		t.Fatalf("without quorum: %s", out.Verdict)
	}
}

func TestInconclusivePolicies(t *testing.T) {
	for policy, want := range map[string]model.Verdict{"hold": model.VerdictInconclusive, "pass": model.VerdictPass, "rollback": model.VerdictFail} {
		s := metrics.NewStore(time.Hour)
		p := phase([]config.Rule{errRule("rollback")}, []string{"canary-1"}, nil)
		p.Cfg.OnInconclusive = policy
		if out := New(p, s, nil).Run(context.Background()); out.Verdict != want {
			t.Errorf("policy %s: got %s want %s", policy, out.Verdict, want)
		}
	}
}

func TestNotifyRuleDoesNotFail(t *testing.T) {
	s := metrics.NewStore(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feed(ctx, s, "canary-1", "access.error_rate_5xx", 30, "")
	out := New(phase([]config.Rule{errRule("notify")}, []string{"canary-1"}, nil), s, nil).Run(ctx)
	if out.Verdict != model.VerdictPass || len(out.Warnings) != 1 {
		t.Fatalf("got %s warnings=%d", out.Verdict, len(out.Warnings))
	}
}

type fakeObserver struct{ degraded bool }

func (o fakeObserver) Degraded(from, to time.Time) (bool, string) {
	return o.degraded, "scheduling lag: woke 4s late"
}

// While the observer is degraded, breaches built on probe failures are held;
// breaches the target itself reported (log, access log) still fail.
func TestObserverDegradedHoldsProbeFailures(t *testing.T) {
	down := config.Rule{Name: "down", Action: "rollback", When: config.Node{Condition: config.Condition{
		Metric: "http.consecutive_failures", Agg: "last", Window: 10 * time.Second, Op: ">=", Value: f(3), For: 1, ResetAfter: 1, Scope: "target", Absent: "unknown",
	}}}
	s := metrics.NewStore(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feed(ctx, s, "canary-1", "http.consecutive_failures", 5, "")
	time.Sleep(20 * time.Millisecond)

	eng := New(phase([]config.Rule{down}, []string{"canary-1"}, nil), s, nil)
	eng.Observer = fakeObserver{degraded: true}
	out := eng.Run(ctx)
	if out.Verdict != model.VerdictHold || len(out.Breaches) == 0 || out.Breaches[0].Action != "hold" {
		t.Fatalf("degraded observer: %s %s", out.Verdict, out.Reason)
	}

	eng = New(phase([]config.Rule{down}, []string{"canary-1"}, nil), s, nil)
	eng.Observer = fakeObserver{degraded: false}
	if out := eng.Run(ctx); out.Verdict != model.VerdictFail {
		t.Fatalf("healthy observer: %s %s", out.Verdict, out.Reason)
	}

	feed(ctx, s, "canary-1", "access.error_rate_5xx", 12, "")
	time.Sleep(20 * time.Millisecond)
	eng = New(phase([]config.Rule{errRule("rollback")}, []string{"canary-1"}, nil), s, nil)
	eng.Observer = fakeObserver{degraded: true}
	if out := eng.Run(ctx); out.Verdict != model.VerdictFail {
		t.Fatalf("access-log breach must not be held for the observer: %s %s", out.Verdict, out.Reason)
	}

	// The same metric name from a probe the target reports through (an
	// access log's latency_ms) is judged; from a probe the orchestrator
	// measures (http) it is held.
	slow := func(metric string) config.Rule {
		return config.Rule{Name: "slow", Action: "rollback", When: config.Node{Condition: config.Condition{
			Metric: metric, Agg: "last", Window: 10 * time.Second, Op: ">", Value: f(500), For: 1, ResetAfter: 1, Scope: "target", Absent: "unknown",
		}}}
	}
	feed(ctx, s, "canary-1", "access.latency_ms", 900, "")
	feed(ctx, s, "canary-1", "http.latency_ms", 900, "")
	time.Sleep(20 * time.Millisecond)
	for metric, want := range map[string]model.Verdict{"access.latency_ms": model.VerdictFail, "http.latency_ms": model.VerdictHold} {
		p := phase([]config.Rule{slow(metric)}, []string{"canary-1"}, nil)
		p.ObserverProbes = map[string]bool{"http": true}
		eng = New(p, s, nil)
		eng.Observer = fakeObserver{degraded: true}
		if out := eng.Run(ctx); out.Verdict != want {
			t.Fatalf("%s: got %s, want %s (%s)", metric, out.Verdict, want, out.Reason)
		}
	}
}

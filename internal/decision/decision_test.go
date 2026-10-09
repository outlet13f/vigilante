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

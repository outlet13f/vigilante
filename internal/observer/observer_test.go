package observer

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/model"
)

func testGuard(t *testing.T) *Guard {
	t.Helper()
	cfg := &config.Config{}
	for i := range 4 {
		svc := config.Service{Name: fmt.Sprintf("svc-%d", i)}
		for j := range 5 {
			svc.Targets = append(svc.Targets, fmt.Sprintf("t-%d-%d", i, j))
		}
		cfg.Services = append(cfg.Services, svc)
	}
	cfg.Safety.ObserverGuard = config.ObserverGuard{MaxLag: time.Second, LoopbackTimeout: time.Second, TimeoutShare: 0.5, MinServices: 3, Grace: time.Minute}
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func sample(target string, timingOut bool, at time.Time) model.Sample {
	v := 0.0
	if timingOut {
		v = 2
	}
	return model.Sample{Target: target, Metric: "http.consecutive_timeouts", Value: v, Time: at, Source: model.SourceCentral}
}

func TestDegradedSpansAndGrace(t *testing.T) {
	g := testGuard(t)
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	if ok, _ := g.Degraded(t0.Add(-time.Hour), t0); ok {
		t.Fatal("degraded before any report")
	}
	g.Report(t0, "scheduling lag: woke 3s late")
	g.Report(t0.Add(2*time.Second), "scheduling lag: woke 2s late")
	// Open span: anything up to now overlaps.
	if ok, why := g.Degraded(t0.Add(10*time.Minute), t0.Add(10*time.Minute)); !ok || !strings.Contains(why, "3s late") {
		t.Fatalf("open span: %v %q", ok, why)
	}
	g.Clear(t0.Add(5 * time.Second))
	// Evidence gathered within the grace period after recovery is still held.
	if ok, _ := g.Degraded(t0.Add(30*time.Second), t0.Add(30*time.Second)); !ok {
		t.Fatal("within grace")
	}
	// Evidence gathered after grace is trusted.
	if ok, _ := g.Degraded(t0.Add(70*time.Second), t0.Add(80*time.Second)); ok {
		t.Fatal("after grace")
	}
	// A long lookback reaches back into the span.
	if ok, _ := g.Degraded(t0.Add(-time.Minute), t0.Add(10*time.Minute)); !ok {
		t.Fatal("lookback")
	}
}

func TestSpreadNeedsSeveralServices(t *testing.T) {
	g := testGuard(t)
	release := g.Start()
	defer release()
	now := time.Now()
	// One bad release: every target of svc-0 times out, others are fine.
	for i := range 4 {
		for j := range 5 {
			g.Observe(sample(fmt.Sprintf("t-%d-%d", i, j), i == 0, now))
		}
	}
	if r := g.Spread(now); r != "" {
		t.Fatalf("one service timing out is the release's problem, not the observer's: %s", r)
	}
	// Timeouts on most targets across three services: the observer.
	for i := range 3 {
		for j := range 5 {
			g.Observe(sample(fmt.Sprintf("t-%d-%d", i, j), true, now))
		}
	}
	if r := g.Spread(now); !strings.Contains(r, "15 of 20 targets across 3 services") {
		t.Fatalf("spread: %q", r)
	}
	// Agent samples and other metrics do not count; stale entries expire.
	if r := g.Spread(now.Add(spreadWindow + time.Second)); r != "" {
		t.Fatalf("stale samples: %q", r)
	}
	g.Observe(model.Sample{Target: "t-3-0", Metric: "http.consecutive_timeouts", Value: 1, Time: now, Source: "agent:t-3-0"})
	if len(g.probes) != 0 {
		t.Fatal("agent sample counted")
	}
}

func TestLoopbackAndStartStop(t *testing.T) {
	l, err := newLoopback()
	if err != nil {
		t.Skip("no loopback:", err)
	}
	if r := l.check(time.Second); r != "" {
		t.Fatalf("healthy loopback: %s", r)
	}
	l.Close()
	if r := l.check(200 * time.Millisecond); r == "" {
		t.Fatal("closed loopback reported healthy")
	}

	g := testGuard(t)
	r1, r2 := g.Start(), g.Start()
	r1()
	r1() // idempotent
	if g.users != 1 || g.stop == nil {
		t.Fatalf("users %d", g.users)
	}
	r2()
	if g.users != 0 || g.stop != nil {
		t.Fatalf("not stopped: users %d", g.users)
	}
	// A healthy idle process does not report itself degraded.
	release := g.Start()
	time.Sleep(1200 * time.Millisecond)
	release()
	if ok, why := g.Degraded(time.Now().Add(-time.Hour), time.Now()); ok {
		t.Fatalf("idle process degraded: %s", why)
	}
}

func TestDisabled(t *testing.T) {
	g := testGuard(t)
	g.cfg.Disabled = true
	g.Report(time.Now(), "x")
	if ok, _ := g.Degraded(time.Now().Add(-time.Hour), time.Now()); ok {
		t.Fatal("disabled guard held")
	}
	g.Start()()
}

package orchestrator

// Chaos scenarios (M5-4). Leader loss during a rollback is in ha_test.go
// (TestHAFailoverFinishesInterruptedRollback) and observer disagreement in
// internal/decision (TestObserverQuorum); these cover the state store and
// the load balancer failing under a running rollback.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vigilante/internal/audit"
	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/store"
)

// flakyStore fails writes while down, like a database that went away.
type flakyStore struct {
	store.Store
	down     atomic.Bool
	refused  atomic.Int64
	accepted atomic.Int64
}

func (f *flakyStore) Append(ctx context.Context, e journal.Entry) error {
	if f.down.Load() {
		f.refused.Add(1)
		return errors.New("dial tcp 10.0.0.50:5432: connect: connection refused")
	}
	f.accepted.Add(1)
	return f.Store.Append(ctx, e)
}

func (f *flakyStore) Ping(ctx context.Context) error {
	if f.down.Load() {
		return errors.New("connection refused")
	}
	return f.Store.Ping(ctx)
}

// The store goes away before a bad canary and comes back after the
// rollback: the rollback is not held up, and nothing it recorded is lost.
func TestChaosStoreOutageDuringRollback(t *testing.T) {
	path := t.TempDir() + "/journal.jsonl"
	inner, err := store.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inner.Close() }) // an injected store is not closed by the engine
	fs := &flakyStore{Store: inner}
	h := newHarness(t, opts{store: fs, journal: path})
	d := h.deploy("d-outage")
	fs.down.Store(true)
	h.app1.healthy.Store(false)
	if err := h.e.Watch(context.Background(), d, model.PhaseCanary); err != nil {
		t.Fatal(err)
	}
	if d.State != model.StateRolledBack {
		t.Fatalf("the rollback must not wait for the store: %s %s", d.State, d.Reason)
	}
	queued := h.e.PendingWrites()
	if queued == 0 || fs.refused.Load() == 0 {
		t.Fatalf("writes during the outage should be queued (queued %d, refused %d)", queued, fs.refused.Load())
	}
	if err := h.e.Journal.Ping(context.Background()); err == nil {
		t.Fatal("store reported reachable while down")
	}

	fs.down.Store(false)
	if !h.e.Flush(10 * time.Second) {
		t.Fatalf("queued writes not flushed: %d left", h.e.PendingWrites())
	}
	// Everything reached the journal, in order, with an intact hash chain.
	st, err := journal.Replay(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Deployments["d-outage"]; got == nil || got.State != model.StateRolledBack || st.StepsDone["d-outage"]["app-1"] != 5 {
		t.Fatalf("journal after the outage: %+v steps %v", got, st.StepsDone["d-outage"])
	}
	r, err := audit.VerifyFile(path)
	if err != nil || !r.OK {
		t.Fatalf("hash chain after the outage: %+v %v", r, err)
	}
	if int(fs.accepted.Load()) < queued {
		t.Fatalf("accepted %d < queued %d", fs.accepted.Load(), queued)
	}
	// A fresh engine on the same journal sees the finished rollback (nothing to resume).
	e2, err := New(h.cfg, Options{Log: h.e.Log, Store: inner, Runners: h.e.runners})
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	if got, _ := e2.Deployment("d-outage"); got.State != model.StateRolledBack {
		t.Fatalf("reloaded state %s", got.State)
	}
	if n := e2.Resume(context.Background()); n != 0 {
		t.Fatalf("resumed %d rollbacks after a completed one", n)
	}
}

// flakyLB wraps the fake nginx host: the first n writes fail or stall.
func flakyLB(h *harness, failures int, stall time.Duration) *atomic.Int64 {
	var calls atomic.Int64
	var mu sync.Mutex
	inner := h.lb.fn
	h.lb.fn = func(cmd, stdin string) (string, error) {
		if strings.Contains(cmd, "nginx -t") {
			n := calls.Add(1)
			if int(n) <= failures {
				if stall > 0 {
					time.Sleep(stall)
				}
				return "", errors.New("connection reset by peer")
			}
		}
		mu.Lock()
		defer mu.Unlock()
		return inner(cmd, stdin)
	}
	return &calls
}

// The LB API errors once, then answers: the step is retried and the
// rollback completes with the target back in rotation.
func TestChaosLoadBalancerTransientErrors(t *testing.T) {
	h := newHarness(t, opts{})
	calls := flakyLB(h, 1, 0)
	h.app1.healthy.Store(false)
	d := h.deploy("d-lb-flaky")
	if err := h.e.Watch(context.Background(), d, model.PhaseCanary); err != nil {
		t.Fatal(err)
	}
	if d.State != model.StateRolledBack || len(h.lb.down()) != 0 {
		t.Fatalf("state %s, left drained %v: %s", d.State, h.lb.down(), d.Reason)
	}
	if calls.Load() < 3 { // failed drain + retried drain + enable
		t.Fatalf("LB writes %d", calls.Load())
	}
	if !hasEvent(d, "retry", "traffic.drain attempt 1/2 failed") {
		t.Fatalf("retry not recorded: %+v", d.Events)
	}
}

// A slow LB API is cut off by the step timeout and retried.
func TestChaosLoadBalancerLatency(t *testing.T) {
	h := newHarness(t, opts{})
	sv, _ := h.cfg.Service("order")
	sv.Rollback.StepTimeout = 300 * time.Millisecond
	flakyLB(h, 1, time.Second)
	h.app1.healthy.Store(false)
	d := h.deploy("d-lb-slow")
	start := time.Now()
	if err := h.e.Watch(context.Background(), d, model.PhaseCanary); err != nil {
		t.Fatal(err)
	}
	if d.State != model.StateRolledBack {
		t.Fatalf("state %s: %s", d.State, d.Reason)
	}
	if !hasEvent(d, "retry", "traffic.drain attempt 1/2 failed") {
		t.Fatalf("timed-out drain not retried: %+v", d.Events)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("rollback waited on the slow LB: %s", time.Since(start))
	}
}

// The LB is down for good: the target is rolled back in place (better than
// not at all), but it cannot be put back in rotation, so the result is
// ROLLBACK_FAILED and a person is called rather than a silent success.
func TestChaosLoadBalancerDown(t *testing.T) {
	h := newHarness(t, opts{})
	h.lb.fn = func(string, string) (string, error) { return "", errors.New("dial tcp 10.0.0.100:22: i/o timeout") }
	h.app1.healthy.Store(false)
	d := h.deploy("d-lb-down")
	if err := h.e.Watch(context.Background(), d, model.PhaseCanary); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.host.joined(), "ln -sfn '/opt/app/releases/v1'") {
		t.Fatal("the application must still be rolled back in place")
	}
	if !hasEvent(d, "step-failed", "drain failed, continuing in place") {
		t.Fatalf("drain failure not recorded: %+v", d.Events)
	}
	if d.State != model.StateRollbackFailed || !strings.Contains(d.Reason, "traffic.enable") {
		t.Fatalf("an unreachable LB must surface: %s %s", d.State, d.Reason)
	}
}

func hasEvent(d *model.Deployment, kind, contains string) bool {
	for _, ev := range d.Events {
		if ev.Kind == kind && strings.Contains(ev.Message, contains) {
			return true
		}
	}
	return false
}

package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vigilante/internal/ha"
	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/safety"
	"vigilante/internal/store"
	"vigilante/internal/store/pgtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Stop()
	os.Exit(code)
}

// cutStore simulates a node losing its database connection.
type cutStore struct {
	store.Store
	cut atomic.Bool
}

var errCut = errors.New("simulated partition")

func (c *cutStore) TryLease(ctx context.Context, k, o string, ttl time.Duration) (bool, error) {
	if c.cut.Load() {
		return false, errCut
	}
	return c.Store.TryLease(ctx, k, o, ttl)
}

func (c *cutStore) LeaseHolder(ctx context.Context, k string) (string, error) {
	if c.cut.Load() {
		return "", errCut
	}
	return c.Store.LeaseHolder(ctx, k)
}

func (c *cutStore) Append(ctx context.Context, e journal.Entry) error {
	if c.cut.Load() {
		return errCut
	}
	return c.Store.Append(ctx, e)
}

// haNode wires an engine to an elector exactly as `vigilante server` does.
func haNode(t *testing.T, e *Engine, st store.Store, id string, ttl time.Duration) *ha.Elector {
	el := &ha.Elector{Store: st, NodeID: id, Advertise: "http://" + id, TTL: ttl, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	el.OnElected = func(ctx context.Context) {
		if err := e.Reload(ctx); err != nil {
			t.Errorf("%s reload: %v", id, err)
			return
		}
		e.SetActive(true)
		e.Resume(ctx)
	}
	el.OnDemoted = func() { e.SetActive(false) }
	e.SetActive(false)
	st.Fence(ha.LeaderKey, el.Owner())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { el.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return el
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A rollback interrupted on the leader (after drain + symlink switch) is
// finished by the node that takes over, without repeating completed steps.
func TestHAFailoverFinishesInterruptedRollback(t *testing.T) {
	dsn := pgtest.DSN(t)
	open := func() *cutStore {
		s, err := store.OpenPostgres(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return &cutStore{Store: s}
	}
	drained := "upstream order {\n  server 10.0.0.1:8080 down;\n  server 10.0.0.2:8080;\n}\n"
	ttl := 1500 * time.Millisecond

	sa := open()
	a := newHarness(t, opts{store: sa, owner: "node-a", upstream: drained})
	elA := haNode(t, a.e, sa, "node-a", ttl)
	waitUntil(t, 3*time.Second, "node-a to lead", func() bool { return elA.IsLeader() && a.e.Active() })

	// node-a starts a rollback, drains app-1 and switches the symlink, then dies.
	d := a.deploy("ha-1")
	d.Targets = []string{"app-1"}
	a.e.setState(d, model.StateRollingBack, "rollback in progress on node-a")
	for step := 0; step < 2; step++ {
		if err := sa.Append(context.Background(), journal.Entry{Kind: journal.KindStepDone, DeployID: "ha-1", Target: "app-1", Step: step}); err != nil {
			t.Fatal(err)
		}
	}

	sb := open()
	b := newHarness(t, opts{store: sb, owner: "node-b", upstream: drained, app1: a.app1, app2: a.app2})
	elB := haNode(t, b.e, sb, "node-b", ttl)
	time.Sleep(ttl / 2)
	if elB.IsLeader() || b.e.Active() {
		t.Fatal("node-b must stay passive while node-a leads")
	}

	sa.cut.Store(true)
	waitUntil(t, 5*ttl, "node-b to finish the rollback", func() bool {
		got, ok := b.e.Deployment("ha-1")
		return ok && got.State == model.StateRolledBack
	})
	if strings.Contains(b.host.joined(), "ln -sfn") {
		t.Fatal("node-b repeated the symlink switch node-a had completed")
	}
	if len(b.lb.down()) != 0 {
		t.Fatalf("app-1 left out of rotation: %v", b.lb.down())
	}
	if b.e.Breaker.State().State != safety.Closed {
		t.Fatal("a resumed rollback is not a failure")
	}

	waitUntil(t, 2*ttl, "node-a to stand down", func() bool { return !a.e.Active() })
	if err := a.e.Watch(context.Background(), a.deploy("ha-2"), model.PhaseCanary); !errors.Is(err, ErrInactive) {
		t.Fatalf("demoted node started a phase: %v", err)
	}
}

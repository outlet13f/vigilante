package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/safety"
	"vigilante/internal/store/pgtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Stop()
	os.Exit(code)
}

// backends runs the same contract against every store implementation.
func backends(t *testing.T) map[string]func(t *testing.T) Store {
	return map[string]func(t *testing.T) Store{
		"file": func(t *testing.T) Store {
			s, err := OpenFile(filepath.Join(t.TempDir(), "journal.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			return s
		},
		"postgres": func(t *testing.T) Store {
			s, err := OpenPostgres(context.Background(), pgtest.DSN(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			return s
		},
	}
}

func TestAppendAndLoad(t *testing.T) {
	for name, open := range backends(t) {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			ctx := context.Background()
			d := &model.Deployment{ID: "d1", Service: "order", Version: "v2", State: model.StateRollingBack}
			now := time.Now().UTC().Truncate(time.Millisecond)
			for _, e := range []journal.Entry{
				{Kind: journal.KindDeployment, Deployment: d},
				{Kind: journal.KindRollbackStart, Service: "order", DeployID: "d1", Time: now},
				{Kind: journal.KindStepDone, DeployID: "d1", Target: "app-1", Step: 0},
				{Kind: journal.KindStepDone, DeployID: "d1", Target: "app-1", Step: 1},
				{Kind: journal.KindCircuit, Circuit: &safety.CircuitState{State: safety.Open, Reason: "test"}},
			} {
				if err := s.Append(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			st, err := s.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if st.Deployments["d1"].State != model.StateRollingBack || st.StepsDone["d1"]["app-1"] != 2 ||
				len(st.Rollbacks["order"]) != 1 || st.Circuit.State != safety.Open || len(st.InFlight()) != 1 {
				t.Fatalf("state %+v", st)
			}
		})
	}
}

func TestLeases(t *testing.T) {
	for name, open := range backends(t) {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			ctx := context.Background()
			ok, err := s.TryLease(ctx, "service:order", "A", time.Minute)
			if err != nil || !ok {
				t.Fatalf("A acquire: %v %v", ok, err)
			}
			if ok, _ := s.TryLease(ctx, "service:order", "B", time.Minute); ok {
				t.Fatal("B must not take a lease A holds")
			}
			if ok, _ := s.TryLease(ctx, "service:order", "A", time.Minute); !ok {
				t.Fatal("A must be able to renew")
			}
			if h, _ := s.LeaseHolder(ctx, "service:order"); h != "A" {
				t.Fatalf("holder %q", h)
			}
			if err := s.ReleaseLease(ctx, "service:order", "B"); err != nil {
				t.Fatal(err)
			}
			if h, _ := s.LeaseHolder(ctx, "service:order"); h != "A" {
				t.Fatal("B released a lease it does not hold")
			}
			_ = s.ReleaseLease(ctx, "service:order", "A")
			if ok, _ := s.TryLease(ctx, "service:order", "B", 300*time.Millisecond); !ok {
				t.Fatal("B after release")
			}
			time.Sleep(450 * time.Millisecond)
			if h, _ := s.LeaseHolder(ctx, "service:order"); h != "" {
				t.Fatalf("expired lease still held by %q", h)
			}
			if ok, _ := s.TryLease(ctx, "service:order", "C", time.Minute); !ok {
				t.Fatal("C must take over an expired lease")
			}
		})
	}
}

func TestLeaseRaceHasOneWinner(t *testing.T) {
	for name, open := range backends(t) {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			var wins atomic.Int32
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					if ok, err := s.TryLease(context.Background(), "leader", string(rune('a'+i)), time.Minute); err == nil && ok {
						wins.Add(1)
					}
				}(i)
			}
			wg.Wait()
			if wins.Load() != 1 {
				t.Fatalf("%d winners", wins.Load())
			}
		})
	}
}

// A node that lost the leader lease can no longer record decisions.
func TestPostgresFencing(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	a, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenPostgres(ctx, dsn) // second node; migrations are idempotent
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if ok, _ := a.TryLease(ctx, "leader", "node-a", 300*time.Millisecond); !ok {
		t.Fatal("node-a should lead")
	}
	a.Fence("leader", "node-a")
	if err := a.Append(ctx, journal.Entry{Kind: journal.KindAudit, Message: "a while leader"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(450 * time.Millisecond) // node-a misses its renewals
	if ok, _ := b.TryLease(ctx, "leader", "node-b", time.Minute); !ok {
		t.Fatal("node-b should take over")
	}
	b.Fence("leader", "node-b")
	if err := a.Append(ctx, journal.Entry{Kind: journal.KindAudit, Message: "a after demotion"}); !errors.Is(err, ErrFenced) {
		t.Fatalf("demoted node wrote: %v", err)
	}
	if err := b.Append(ctx, journal.Entry{Kind: journal.KindAudit, Message: "b as leader"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.TryLease(ctx, "leader", "node-a", time.Minute); ok {
		t.Fatal("node-a must not steal the lease back")
	}
}

package ha

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"vigilante/internal/journal"
	"vigilante/internal/store"
	"vigilante/internal/store/pgtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Stop()
	os.Exit(code)
}

// partitionable cuts a node off from the database on demand.
type partitionable struct {
	store.Store
	cut atomic.Bool
}

var errPartition = errors.New("simulated network partition")

func (p *partitionable) TryLease(ctx context.Context, k, o string, ttl time.Duration) (bool, error) {
	if p.cut.Load() {
		return false, errPartition
	}
	return p.Store.TryLease(ctx, k, o, ttl)
}

func (p *partitionable) LeaseHolder(ctx context.Context, k string) (string, error) {
	if p.cut.Load() {
		return "", errPartition
	}
	return p.Store.LeaseHolder(ctx, k)
}

func (p *partitionable) Append(ctx context.Context, e journal.Entry) error {
	if p.cut.Load() {
		return errPartition
	}
	return p.Store.Append(ctx, e)
}

type node struct {
	el       *Elector
	st       *partitionable
	elected  atomic.Int32
	demoted  atomic.Int32
	cancel   context.CancelFunc
	finished chan struct{}
}

func startNode(t *testing.T, dsn, id string, ttl time.Duration) *node {
	t.Helper()
	s, err := store.OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	n := &node{st: &partitionable{Store: s}, finished: make(chan struct{})}
	n.el = &Elector{Store: n.st, NodeID: id, Advertise: "http://" + id + ":8088", TTL: ttl,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnElected: func(context.Context) { n.elected.Add(1) },
		OnDemoted: func() { n.demoted.Add(1) }}
	n.st.Fence(LeaderKey, n.el.Owner())
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	go func() { n.el.Run(ctx); close(n.finished) }()
	t.Cleanup(func() { cancel(); <-n.finished })
	return n
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestFailoverOnPartition(t *testing.T) {
	dsn := pgtest.DSN(t)
	ttl := 1500 * time.Millisecond
	a := startNode(t, dsn, "node-a", ttl)
	waitFor(t, 3*time.Second, "node-a to lead", a.el.IsLeader)
	b := startNode(t, dsn, "node-b", ttl)
	waitFor(t, 3*time.Second, "node-b to learn the leader", func() bool { _, addr := b.el.Leader(); return addr == "http://node-a:8088" })
	if b.el.IsLeader() {
		t.Fatal("two leaders")
	}
	if err := a.st.Append(context.Background(), journal.Entry{Kind: journal.KindAudit, Message: "leader write"}); err != nil {
		t.Fatalf("leader write refused: %v", err)
	}

	a.st.cut.Store(true) // node-a loses the database
	waitFor(t, 4*ttl, "node-b to take over", b.el.IsLeader)
	waitFor(t, 2*ttl, "node-a to step down", func() bool { return !a.el.IsLeader() })
	if a.demoted.Load() != 1 || b.elected.Load() != 1 {
		t.Fatalf("callbacks: a demoted %d, b elected %d", a.demoted.Load(), b.elected.Load())
	}

	a.st.cut.Store(false) // partition heals: node-a must not write as leader
	if err := a.st.Append(context.Background(), journal.Entry{Kind: journal.KindAudit, Message: "stale leader write"}); !errors.Is(err, store.ErrFenced) {
		t.Fatalf("demoted node wrote: %v", err)
	}
	time.Sleep(ttl)
	if a.el.IsLeader() || !b.el.IsLeader() {
		t.Fatal("leadership flapped back after the partition healed")
	}
}

func TestGracefulShutdownHandsOverFast(t *testing.T) {
	dsn := pgtest.DSN(t)
	ttl := 3 * time.Second
	a := startNode(t, dsn, "node-a", ttl)
	waitFor(t, 3*time.Second, "node-a to lead", a.el.IsLeader)
	b := startNode(t, dsn, "node-b", ttl)
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	a.cancel() // graceful: releases the lease
	<-a.finished
	waitFor(t, ttl, "node-b to take over", b.el.IsLeader)
	if took := time.Since(start); took > ttl/3+500*time.Millisecond {
		t.Fatalf("handover took %s; a released lease should be taken at the next renewal tick", took)
	}
}

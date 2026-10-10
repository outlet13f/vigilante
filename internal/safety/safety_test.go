package safety

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vigilante/internal/config"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestBreakerLifecycle(t *testing.T) {
	c := &clock{t: time.Now()}
	var persisted []CircuitStateName
	b := NewBreaker(config.CircuitBreaker{FailureThreshold: 2, Window: time.Hour, OpenDuration: 10 * time.Minute}, nil)
	b.Now = c.now
	b.Persist = func(s CircuitState) { persisted = append(persisted, s.State) }

	if err := b.Allow(); err != nil {
		t.Fatal(err)
	}
	b.Failure("step app.rollback failed")
	if b.State().State != Closed {
		t.Fatal("one failure must not open")
	}
	b.Failure("again")
	if b.State().State != Open {
		t.Fatal("threshold reached, expected OPEN")
	}
	if err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("OPEN must deny, got %v", err)
	}
	c.t = c.t.Add(11 * time.Minute)
	if err := b.Allow(); err != nil {
		t.Fatalf("HALF_OPEN must allow one trial: %v", err)
	}
	if err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("HALF_OPEN must allow only one trial")
	}
	b.Failure("trial failed")
	if s := b.State(); s.State != Open {
		t.Fatalf("failed trial must re-open, got %s", s.State)
	}
	c.t = c.t.Add(11 * time.Minute)
	_ = b.Allow()
	b.Success()
	if b.State().State != Closed {
		t.Fatal("successful trial must close")
	}
	if len(persisted) == 0 {
		t.Fatal("state changes must be persisted")
	}
}

func TestBreakerWindowPrunes(t *testing.T) {
	c := &clock{t: time.Now()}
	b := NewBreaker(config.CircuitBreaker{FailureThreshold: 2, Window: time.Minute}, nil)
	b.Now = c.now
	b.Failure("a")
	c.t = c.t.Add(2 * time.Minute)
	b.Failure("b")
	if b.State().State != Closed {
		t.Fatal("failures outside the window must not count")
	}
}

func TestManualTripNeedsReset(t *testing.T) {
	b := NewBreaker(config.CircuitBreaker{FailureThreshold: 3, Window: time.Hour}, nil)
	b.Trip("change freeze")
	if err := b.Allow(); err == nil {
		t.Fatal("tripped breaker must deny")
	}
	b.Reset()
	if err := b.Allow(); err != nil {
		t.Fatal(err)
	}
}

func TestGuardLockAndFlapping(t *testing.T) {
	c := &clock{t: time.Now()}
	g := NewGuard(config.Flapping{MaxRollbacksPerHour: 2, Cooldown: 5 * time.Minute}, nil)
	g.Now = c.now
	leases := &memLeases{m: map[string]string{}}
	g.Leases, g.Owner = leases, "proc-1"
	rel, err := g.Acquire("order-api")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Acquire("order-api"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquire: %v", err)
	}
	// Another process or server node (fresh Guard, same lease store) is also excluded.
	g2 := NewGuard(config.Flapping{}, nil)
	g2.Leases, g2.Owner = leases, "proc-2"
	if _, err := g2.Acquire("order-api"); !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "proc-1") {
		t.Fatalf("cross-process acquire: %v", err)
	}
	rel()
	if _, err := g2.Acquire("order-api"); err != nil {
		t.Fatalf("after release: %v", err)
	}

	g.Record("order-api", c.t)
	if err := g.Check("order-api"); !errors.Is(err, ErrCooldown) {
		t.Fatalf("cooldown: %v", err)
	}
	c.t = c.t.Add(6 * time.Minute)
	if err := g.Check("order-api"); err != nil {
		t.Fatal(err)
	}
	g.Record("order-api", c.t)
	c.t = c.t.Add(6 * time.Minute)
	if err := g.Check("order-api"); !errors.Is(err, ErrFlapping) {
		t.Fatalf("flapping: %v", err)
	}
}

func TestDrainBatch(t *testing.T) {
	cases := []struct {
		pool, enabled, drain int
		cfg                  config.BlastRadius
		want                 int
	}{
		{10, 10, 2, config.BlastRadius{MinHealthy: 1}, 2},
		{10, 10, 6, config.BlastRadius{MinHealthyPercent: 50}, 5},
		{2, 2, 2, config.BlastRadius{MinHealthy: 1}, 1},
		{2, 1, 1, config.BlastRadius{MinHealthy: 1}, 0}, // last member standing is never drained
	}
	for _, c := range cases {
		if got, _ := DrainBatch(c.pool, c.enabled, c.drain, c.cfg); got != c.want {
			t.Errorf("DrainBatch(%d,%d,%d,%+v) = %d want %d", c.pool, c.enabled, c.drain, c.cfg, got, c.want)
		}
	}
}

// downLeases is a lease store that cannot be reached until up is set.
type downLeases struct {
	memLeases
	up    atomic.Bool
	tries atomic.Int64
}

func (l *downLeases) TryLease(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	l.tries.Add(1)
	if !l.up.Load() {
		return false, errors.New("dial tcp 10.0.0.50:5432: connect: connection refused")
	}
	return l.memLeases.TryLease(ctx, key, owner, ttl)
}

func TestGuardStoreUnreachable(t *testing.T) {
	leases := &downLeases{memLeases: memLeases{m: map[string]string{}}}
	newGuard := func(proceed bool) *Guard {
		g := NewGuard(config.Flapping{}, nil)
		g.Leases, g.Owner, g.TTL = leases, "proc-1", 300*time.Millisecond
		g.LeaseWait, g.ProceedUnleased = 300*time.Millisecond, proceed
		return g
	}

	// on_unavailable: fail refuses after retrying for LeaseWait.
	g := newGuard(false)
	start := time.Now()
	if _, err := g.Acquire("order-api"); !errors.Is(err, ErrLeaseUnavailable) {
		t.Fatalf("fail mode: %v", err)
	}
	if time.Since(start) < 250*time.Millisecond || leases.tries.Load() < 2 {
		t.Fatalf("gave up after %s and %d tries; should retry for LeaseWait", time.Since(start), leases.tries.Load())
	}
	if _, err := g.Acquire("order-api"); !errors.Is(err, ErrLeaseUnavailable) {
		t.Fatalf("a refused acquire must not leave the local lock behind: %v", err)
	}

	// on_unavailable: proceed hands back the local lock and says so.
	g = newGuard(true)
	var unleased, conflict atomic.Value
	g.Unleased = func(s string, err error) { unleased.Store(s) }
	g.Conflict = func(s, holder string) { conflict.Store(holder) }
	rel, err := g.Acquire("order-api")
	if err != nil || unleased.Load() != "order-api" {
		t.Fatalf("proceed mode: %v (unleased %v)", err, unleased.Load())
	}
	if _, err := g.Acquire("order-api"); !errors.Is(err, ErrLocked) {
		t.Fatalf("local exclusion still applies: %v", err)
	}
	// The store comes back with another process holding the lease.
	leases.memLeases.m["service:order-api"] = "proc-2"
	leases.up.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for conflict.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if conflict.Load() != "proc-2" {
		t.Fatalf("conflict not reported: %v", conflict.Load())
	}
	rel()
	if leases.memLeases.m["service:order-api"] != "proc-2" {
		t.Fatal("release must not drop another process's lease")
	}

	// The store comes back free: the background loop takes the lease.
	delete(leases.memLeases.m, "service:order-api")
	leases.up.Store(false)
	rel, err = g.Acquire("order-api")
	if err != nil {
		t.Fatal(err)
	}
	leases.up.Store(true)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		leases.mu.Lock()
		got := leases.m["service:order-api"]
		leases.mu.Unlock()
		if got == "proc-1" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	leases.mu.Lock()
	got := leases.m["service:order-api"]
	leases.mu.Unlock()
	if got != "proc-1" {
		t.Fatalf("lease not taken once the store came back: %q", got)
	}
	rel()
}

type memLeases struct {
	mu sync.Mutex
	m  map[string]string
}

func (l *memLeases) TryLease(_ context.Context, key, owner string, _ time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.m[key]; ok && cur != owner {
		return false, nil
	}
	l.m[key] = owner
	return true, nil
}

func (l *memLeases) ReleaseLease(_ context.Context, key, owner string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m[key] == owner {
		delete(l.m, key)
	}
	return nil
}

func (l *memLeases) LeaseHolder(_ context.Context, key string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m[key], nil
}

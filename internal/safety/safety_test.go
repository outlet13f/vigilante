package safety

import (
	"errors"
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
	g.LockDir = t.TempDir()
	rel, err := g.Acquire("order-api")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Acquire("order-api"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquire: %v", err)
	}
	// A second process (fresh Guard, same lock dir) is also excluded.
	g2 := NewGuard(config.Flapping{}, nil)
	g2.LockDir = g.LockDir
	if _, err := g2.Acquire("order-api"); !errors.Is(err, ErrLocked) {
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

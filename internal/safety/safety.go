// Package safety holds the guard rails around automatic rollback: the global
// circuit breaker (stop automation when rollbacks themselves fail), per-service
// rollback locks, a flapping guard, and blast-radius math for traffic drains.
package safety

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"vigilante/internal/config"
)

var (
	ErrCircuitOpen = errors.New("circuit breaker OPEN: automated rollback actions are frozen")
	ErrLocked      = errors.New("another rollback for this service is in progress")
	ErrFlapping    = errors.New("flapping guard: too many automatic rollbacks for this service")
	ErrCooldown    = errors.New("rollback cooldown active for this service")
)

type CircuitStateName string

const (
	Closed   CircuitStateName = "CLOSED"
	Open     CircuitStateName = "OPEN"
	HalfOpen CircuitStateName = "HALF_OPEN"
)

// CircuitState is persisted to the journal so an OPEN breaker survives
// restarts and is shared by successive CI invocations.
type CircuitState struct {
	State        CircuitStateName `json:"state"`
	Failures     []time.Time      `json:"failures,omitempty"`
	OpenedAt     time.Time        `json:"opened_at,omitempty"`
	Reason       string           `json:"reason,omitempty"`
	HalfOpenUsed bool             `json:"half_open_used,omitempty"`
}

// Breaker is the global "emergency stop".
//
//	CLOSED    --(N rollback failures within window | manual trip)--> OPEN
//	OPEN      --(open_duration elapsed)--> HALF_OPEN   (open_duration: 0 => manual reset only)
//	HALF_OPEN --(one trial action succeeds)--> CLOSED
//	HALF_OPEN --(trial fails)--> OPEN
//	any       --(manual reset with token)--> CLOSED
type Breaker struct {
	mu      sync.Mutex
	cfg     config.CircuitBreaker
	st      CircuitState
	Persist func(CircuitState)
	Now     func() time.Time
}

func NewBreaker(cfg config.CircuitBreaker, initial *CircuitState) *Breaker {
	b := &Breaker{cfg: cfg, Now: time.Now, st: CircuitState{State: Closed}}
	if initial != nil {
		b.st = *initial
	}
	return b
}

func (b *Breaker) persist() {
	if b.Persist != nil {
		b.Persist(b.st)
	}
}

func (b *Breaker) maybeHalfOpen() {
	if b.st.State == Open && b.cfg.OpenDuration > 0 && b.Now().Sub(b.st.OpenedAt) >= b.cfg.OpenDuration {
		b.st.State, b.st.HalfOpenUsed = HalfOpen, false
		b.persist()
	}
}

// Allow must be called before every automatic, state-changing action.
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpen()
	switch b.st.State {
	case Open:
		return fmt.Errorf("%w (since %s: %s)", ErrCircuitOpen, b.st.OpenedAt.Format(time.RFC3339), b.st.Reason)
	case HalfOpen:
		if b.st.HalfOpenUsed {
			return fmt.Errorf("%w (half-open trial already in flight)", ErrCircuitOpen)
		}
		b.st.HalfOpenUsed = true
		b.persist()
	}
	return nil
}

func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.State == HalfOpen {
		b.st = CircuitState{State: Closed}
		b.persist()
	}
}

func (b *Breaker) Failure(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.Now()
	if b.st.State == HalfOpen {
		b.open(now, "half-open trial failed: "+reason)
		return
	}
	cutoff := now.Add(-b.cfg.Window)
	kept := b.st.Failures[:0]
	for _, t := range b.st.Failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	b.st.Failures = append(kept, now)
	if len(b.st.Failures) >= b.cfg.FailureThreshold && b.st.State == Closed {
		b.open(now, fmt.Sprintf("%d rollback failures within %s; last: %s", len(b.st.Failures), b.cfg.Window, reason))
		return
	}
	b.persist()
}

func (b *Breaker) open(now time.Time, reason string) {
	b.st.State, b.st.OpenedAt, b.st.Reason, b.st.HalfOpenUsed = Open, now, reason, false
	b.persist()
}

// Trip is the manual kill switch.
func (b *Breaker) Trip(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.open(b.Now(), "manual: "+reason)
}

// Reset closes the breaker (operator action).
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st = CircuitState{State: Closed}
	b.persist()
}

func (b *Breaker) State() CircuitState {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpen()
	st := b.st
	st.Failures = append([]time.Time(nil), b.st.Failures...)
	return st
}

// Leases is the part of the state store a Guard needs (store.Store has it).
type Leases interface {
	TryLease(ctx context.Context, key, owner string, ttl time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, key, owner string) error
	LeaseHolder(ctx context.Context, key string) (string, error)
}

// Guard enforces one rollback per service at a time (in-process, and through
// state-store leases across processes and server nodes) plus flapping and
// cooldown limits.
type Guard struct {
	mu      sync.Mutex
	cfg     config.Flapping
	locks   map[string]bool
	history map[string][]time.Time
	// Leases makes the lock visible to other processes; nil = in-process only.
	Leases Leases
	// Owner identifies this process in lease records.
	Owner string
	// TTL bounds how long a crashed holder blocks the service; the lease is
	// renewed every TTL/3 while held, so long rollbacks keep it.
	TTL time.Duration
	// LeaseWait is how long Acquire keeps retrying a state store it cannot
	// reach before giving up on the lease.
	LeaseWait time.Duration
	// ProceedUnleased lets the rollback go ahead under the in-process lock
	// alone once LeaseWait has passed: a bad release keeps hurting users while
	// the store is down, and rollback steps are idempotent. Unleased (when set)
	// is told, so the decision is visible. False refuses the rollback instead.
	ProceedUnleased bool
	Unleased        func(service string, err error)
	// Conflict is called when the store comes back during an unleased
	// rollback and another process already holds the service lease.
	Conflict func(service, holder string)
	Now      func() time.Time
}

func NewGuard(cfg config.Flapping, history map[string][]time.Time) *Guard {
	if history == nil {
		history = map[string][]time.Time{}
	}
	host, _ := os.Hostname()
	return &Guard{cfg: cfg, locks: map[string]bool{}, history: history,
		Owner: fmt.Sprintf("%s/%d", host, os.Getpid()), TTL: 2 * time.Minute, Now: time.Now}
}

// Acquire takes the service's rollback lock. ErrLocked means another rollback
// holds it here or in another process; a store that stays unreachable for
// LeaseWait either fails the call or, with ProceedUnleased, hands back a lock
// that keeps trying to take the lease in the background.
func (g *Guard) Acquire(service string) (release func(), err error) {
	g.mu.Lock()
	if g.locks[service] {
		g.mu.Unlock()
		return nil, ErrLocked
	}
	g.locks[service] = true
	g.mu.Unlock()
	unlock := func() {
		g.mu.Lock()
		delete(g.locks, service)
		g.mu.Unlock()
	}
	if g.Leases == nil {
		return unlock, nil
	}
	key := "service:" + service
	held, err := g.tryLease(key)
	switch {
	case err == nil && !held:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		holder, _ := g.Leases.LeaseHolder(ctx, key)
		cancel()
		unlock()
		return nil, fmt.Errorf("%w (held by %s)", ErrLocked, holder)
	case err != nil && !g.ProceedUnleased:
		unlock()
		return nil, fmt.Errorf("%w: %v", ErrLeaseUnavailable, err)
	case err != nil && g.Unleased != nil:
		g.Unleased(service, err)
	}
	stopRenew := g.renew(service, key, held)
	return func() {
		unlock()
		stopRenew()
	}, nil
}

// ErrLeaseUnavailable means the state store could not be reached to take the
// cross-process rollback lock.
var ErrLeaseUnavailable = errors.New("state store unreachable: cannot take the service rollback lease")

// tryLease retries store errors with backoff for up to LeaseWait. A definite
// answer (taken or held elsewhere) returns at once.
func (g *Guard) tryLease(key string) (bool, error) {
	deadline := time.Now().Add(g.LeaseWait)
	backoff := 250 * time.Millisecond
	for {
		attempt := min(5*time.Second, max(time.Until(deadline), time.Second))
		ctx, cancel := context.WithTimeout(context.Background(), attempt)
		ok, err := g.Leases.TryLease(ctx, key, g.Owner, g.TTL)
		cancel()
		if err == nil || time.Until(deadline) <= 0 {
			return ok, err
		}
		time.Sleep(min(backoff, max(time.Until(deadline), 0)))
		backoff = min(backoff*2, 2*time.Second)
	}
}

// renew keeps a held lease alive until the returned stop function runs, then
// releases it. Without the lease (store down at Acquire) it keeps trying to
// take it, and reports a conflict if another process got there first.
func (g *Guard) renew(service, key string, held bool) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	interval := g.TTL / 3
	if !held {
		interval = min(interval, 5*time.Second)
	}
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		conflict := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rctx, rcancel := context.WithTimeout(ctx, 10*time.Second)
				ok, err := g.Leases.TryLease(rctx, key, g.Owner, g.TTL)
				if err == nil && !ok && !conflict {
					conflict = true
					if g.Conflict != nil {
						holder, _ := g.Leases.LeaseHolder(rctx, key)
						g.Conflict(service, holder)
					}
				}
				rcancel()
			}
		}
	}()
	return func() {
		cancel()
		<-done
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = g.Leases.ReleaseLease(rctx, key, g.Owner)
		rcancel()
	}
}

// Check rejects an automatic rollback when the service is flapping (the
// "previous" version may be broken too, and ping-ponging makes it worse).
func (g *Guard) Check(service string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.Now()
	recent := 0
	var last time.Time
	for _, t := range g.history[service] {
		if now.Sub(t) < time.Hour {
			recent++
		}
		if t.After(last) {
			last = t
		}
	}
	if g.cfg.MaxRollbacksPerHour > 0 && recent >= g.cfg.MaxRollbacksPerHour {
		return fmt.Errorf("%w: %d in the last hour (max %d)", ErrFlapping, recent, g.cfg.MaxRollbacksPerHour)
	}
	if g.cfg.Cooldown > 0 && !last.IsZero() && now.Sub(last) < g.cfg.Cooldown {
		return fmt.Errorf("%w: last rollback %s ago (cooldown %s)", ErrCooldown, now.Sub(last).Round(time.Second), g.cfg.Cooldown)
	}
	return nil
}

func (g *Guard) Record(service string, t time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.history[service] = append(g.history[service], t)
}

// DrainBatch computes how many of the members to drain may be taken out of
// rotation at once without the pool falling below min_healthy /
// min_healthy_percent. 0 means draining is refused entirely: the rollback then
// proceeds in place, one target at a time, without touching the LB.
func DrainBatch(poolSize, enabled, toDrain int, cfg config.BlastRadius) (batch int, reason string) {
	minRequired := cfg.MinHealthy
	if pct := int(math.Ceil(float64(poolSize*cfg.MinHealthyPercent) / 100)); pct > minRequired {
		minRequired = pct
	}
	allowed := enabled - minRequired
	switch {
	case allowed <= 0:
		return 0, fmt.Sprintf("draining refused: %d/%d members enabled, at least %d must stay in rotation", enabled, poolSize, minRequired)
	case allowed < toDrain:
		return allowed, fmt.Sprintf("draining in batches of %d to keep >= %d of %d members in rotation", allowed, minRequired, poolSize)
	}
	return toDrain, ""
}

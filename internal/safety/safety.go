// Package safety holds the guard rails around automatic rollback: the global
// circuit breaker (stop automation when rollbacks themselves fail), per-service
// rollback locks, a flapping guard, and blast-radius math for traffic drains.
package safety

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
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

// Guard enforces one rollback per service at a time (in-process and, via lock
// files, across concurrent CI jobs) plus flapping / cooldown limits.
type Guard struct {
	mu      sync.Mutex
	cfg     config.Flapping
	locks   map[string]bool
	history map[string][]time.Time
	LockDir string        // "" disables cross-process locks
	Stale   time.Duration // lock files older than this are considered abandoned
	Now     func() time.Time
}

func NewGuard(cfg config.Flapping, history map[string][]time.Time) *Guard {
	if history == nil {
		history = map[string][]time.Time{}
	}
	return &Guard{cfg: cfg, locks: map[string]bool{}, history: history, Stale: 30 * time.Minute, Now: time.Now}
}

func (g *Guard) Acquire(service string) (release func(), err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.locks[service] {
		return nil, ErrLocked
	}
	var lockFile string
	if g.LockDir != "" {
		lockFile = filepath.Join(g.LockDir, "vigilante-"+sanitize(service)+".lock")
		if err := g.lockFile(lockFile); err != nil {
			return nil, err
		}
	}
	g.locks[service] = true
	return func() {
		g.mu.Lock()
		delete(g.locks, service)
		g.mu.Unlock()
		if lockFile != "" {
			os.Remove(lockFile)
		}
	}, nil
}

func (g *Guard) lockFile(path string) error {
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d %s\n", os.Getpid(), g.Now().Format(time.RFC3339))
			return f.Close()
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if fi, serr := os.Stat(path); serr == nil && g.Now().Sub(fi.ModTime()) > g.Stale {
			os.Remove(path) // abandoned by a crashed process
			continue
		}
		owner, _ := os.ReadFile(path)
		return fmt.Errorf("%w (lock %s held by %s)", ErrLocked, path, strings.TrimSpace(string(owner)))
	}
	return ErrLocked
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == ' ' {
			return '_'
		}
		return r
	}, s)
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

package transport

import (
	"context"
	"sync"

	"vigilante/internal/telemetry"
)

// SSH session budget per target. sshd caps sessions per connection
// (MaxSessions, 10 by default); beyond it a new command fails. Log streams
// hold a session for as long as they run, so many log probes could leave
// no room for the rollback command at the moment it matters. Every SSH
// target therefore has a budget (connection.max_sessions, default 8) of
// which a share (connection.reserved_sessions, default 2) only urgent work
// may use: rollback steps, traffic drain and enable. Collection waits for a
// free session instead of failing.
const (
	DefaultMaxSessions      = 8
	DefaultReservedSessions = 2
)

type urgentKey struct{}

// Urgent marks ctx as rollback work: it may use the reserved sessions.
func Urgent(ctx context.Context) context.Context { return context.WithValue(ctx, urgentKey{}, true) }

// IsUrgent reports whether ctx was marked by Urgent.
func IsUrgent(ctx context.Context) bool {
	u, _ := ctx.Value(urgentKey{}).(bool)
	return u
}

type sessionLimiter struct {
	mu       sync.Mutex
	inUse    int
	max      int
	reserved int
	wake     chan struct{}
}

func newSessionLimiter(max, reserved int) *sessionLimiter {
	if max <= 0 {
		max = DefaultMaxSessions
	}
	if reserved < 0 || reserved >= max {
		reserved = 0
	}
	return &sessionLimiter{max: max, reserved: reserved, wake: make(chan struct{})}
}

// acquire takes a session slot, waiting while the budget is used up.
func (l *sessionLimiter) acquire(ctx context.Context) error {
	urgent := IsUrgent(ctx)
	waited := false
	for {
		l.mu.Lock()
		limit := l.max - l.reserved
		if urgent {
			limit = l.max
		}
		if l.inUse < limit {
			l.inUse++
			l.mu.Unlock()
			return nil
		}
		wake := l.wake
		l.mu.Unlock()
		if !waited {
			waited = true
			p := "normal"
			if urgent {
				p = "urgent"
			}
			telemetry.SSHWaits.Inc(p)
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (l *sessionLimiter) release() {
	l.mu.Lock()
	l.inUse--
	close(l.wake)
	l.wake = make(chan struct{})
	l.mu.Unlock()
}

func (l *sessionLimiter) used() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inUse
}

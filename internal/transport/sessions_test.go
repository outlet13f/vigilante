package transport

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionBudgetKeepsRoomForRollback(t *testing.T) {
	l := newSessionLimiter(4, 2)
	ctx := context.Background()
	// Collection fills its share (4 - 2 reserved)...
	for i := 0; i < 2; i++ {
		if err := l.acquire(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// ...and the next collector waits rather than taking the reserve.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := l.acquire(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("collection took a reserved session: %v", err)
	}
	// Rollback work gets the reserved sessions at once.
	urgent := Urgent(ctx)
	for i := 0; i < 2; i++ {
		if err := l.acquire(urgent); err != nil {
			t.Fatal(err)
		}
	}
	if l.used() != 4 {
		t.Fatalf("in use %d", l.used())
	}
	// A waiting collector proceeds when a session is released.
	got := make(chan error, 1)
	go func() { got <- l.acquire(ctx) }()
	time.Sleep(20 * time.Millisecond)
	l.release() // an urgent one finishes: 3 in use, still above the collectors' share
	select {
	case err := <-got:
		t.Fatalf("collector entered while rollback holds the reserve: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	l.release()
	l.release() // now 1 in use
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("collector not woken")
	}
}

func TestSessionBudgetDefaults(t *testing.T) {
	l := newSessionLimiter(0, 99) // unset max, nonsense reserve
	if l.max != DefaultMaxSessions || l.reserved != 0 {
		t.Fatalf("%+v", l)
	}
}

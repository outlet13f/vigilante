// Package store is the durable state behind the orchestrator: an append-only
// event log (every decision, rollback step and circuit change, replayed into
// journal.State on start) plus leases, which back both the per-service rollback
// locks and HA leader election.
//
// Two backends: "file" (the JSONL journal and lock files, for one node or a CI
// job) and "postgres" (shared by every server node; leases use the database
// clock, and a leader's writes are fenced so a node that lost leadership can
// no longer record decisions).
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/journal"
)

// ErrFenced is returned by Append when the store is fenced to a lease this
// node no longer holds (it was demoted; another node is leader now).
var ErrFenced = errors.New("store: write rejected, this node no longer holds the leader lease")

type Store interface {
	// Append durably records one event.
	Append(ctx context.Context, e journal.Entry) error
	// Load replays every event into a fresh state.
	Load(ctx context.Context) (*journal.State, error)
	// TryLease acquires key for owner, or renews it if owner already holds
	// it. It returns false while another owner holds an unexpired lease.
	TryLease(ctx context.Context, key, owner string, ttl time.Duration) (bool, error)
	// ReleaseLease drops key if owner holds it.
	ReleaseLease(ctx context.Context, key, owner string) error
	// LeaseHolder reports the current unexpired holder of key ("" if none).
	LeaseHolder(ctx context.Context, key string) (string, error)
	// Fence makes Append fail with ErrFenced unless owner holds key.
	// An empty key turns fencing off.
	Fence(key, owner string)
	Describe() string
	Close() error
}

// Open returns the backend configured in server.state.
func Open(ctx context.Context, cfg *config.Config) (Store, error) {
	switch st := cfg.Server.State; st.Backend {
	case "", "file":
		return OpenFile(cfg.Server.JournalPath)
	case "postgres":
		dsn := st.DSN
		if st.DSNEnv != "" {
			dsn = os.Getenv(st.DSNEnv)
		}
		if dsn == "" {
			return nil, fmt.Errorf("server.state: postgres DSN is empty (set %s)", st.DSNEnv)
		}
		return OpenPostgres(ctx, dsn)
	default:
		return nil, fmt.Errorf("unknown state backend %q", st.Backend)
	}
}

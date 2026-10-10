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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/journal"
	"vigilante/internal/secrets"
)

// ErrFenced is returned by Append when the store is fenced to a lease this
// node no longer holds (it was demoted; another node is leader now).
var ErrFenced = errors.New("store: write rejected, this node no longer holds the leader lease")

type Store interface {
	// Append durably records one event.
	Append(ctx context.Context, e journal.Entry) error
	// Load replays every event into a fresh state.
	Load(ctx context.Context) (*journal.State, error)
	// Scan visits every stored entry in order with its position (sequence
	// number or line), for audit verification and queries.
	Scan(ctx context.Context, fn func(pos int64, e journal.Entry) error) error
	// Prune removes entries older than before: they are written to archive
	// first (as JSONL, still verifiable on their own), then replaced by one
	// anchor entry that carries the state they built and the hash the
	// remaining chain links to. Returns how many entries were removed.
	Prune(ctx context.Context, before time.Time, archive io.Writer) (int, error)
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
	// SetChainKey makes Append add each entry's MAC (journal.Entry.MAC,
	// audit.chain_key_ref). nil turns it off.
	SetChainKey(key []byte)
	// Ping checks the backend is reachable (readiness).
	Ping(ctx context.Context) error
	Describe() string
	Close() error
}

// anchorFor builds the anchor that replaces a pruned prefix. It carries the
// last pruned entry's hash and MAC (the MAC is of that hash, so it holds).
func anchorFor(pruned []journal.Entry, before time.Time) journal.Entry {
	last := pruned[len(pruned)-1]
	return journal.Entry{
		Kind: journal.KindAnchor, Time: time.Now(), Hash: last.Hash, MAC: last.MAC, Actor: "system", Source: "cli", Action: "audit.prune",
		Message:   fmt.Sprintf("pruned %d entries older than %s; chain continues from %s", len(pruned), before.Format(time.RFC3339), last.Hash),
		Compacted: journal.Compact(pruned, before),
	}
}

func writeArchive(w io.Writer, entries []journal.Entry) error {
	enc := json.NewEncoder(w)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

// Open returns the backend configured in server.state, keyed with
// audit.chain_key_ref when it is set.
func Open(ctx context.Context, cfg *config.Config) (Store, error) {
	key, err := ChainKey(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var s Store
	switch st := cfg.Server.State; st.Backend {
	case "", "file":
		s, err = OpenFile(cfg.Server.JournalPath)
	case "postgres":
		dsn, derr := PostgresDSN(ctx, cfg)
		if derr != nil {
			return nil, derr
		}
		s, err = OpenPostgresWith(ctx, dsn, st.AutoMigrate == nil || *st.AutoMigrate)
	default:
		return nil, fmt.Errorf("unknown state backend %q", st.Backend)
	}
	if err != nil {
		return nil, err
	}
	s.SetChainKey(key)
	return s, nil
}

// MinChainKey is the shortest accepted audit.chain_key_ref value, in bytes.
const MinChainKey = 32

// ChainKey resolves audit.chain_key_ref (nil when it is not set).
func ChainKey(ctx context.Context, cfg *config.Config) ([]byte, error) {
	return ResolveChainKey(ctx, cfg.Audit.ChainKeyRef)
}

// ResolveChainKey resolves a chain key reference (nil for "").
func ResolveChainKey(ctx context.Context, ref string) ([]byte, error) {
	if ref == "" {
		return nil, nil
	}
	v, err := secrets.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("audit.chain_key_ref: %w", err)
	}
	if len(v) < MinChainKey {
		return nil, fmt.Errorf("audit.chain_key_ref: the key must be at least %d bytes (got %d)", MinChainKey, len(v))
	}
	return []byte(v), nil
}

// PostgresDSN resolves server.state's connection string.
func PostgresDSN(ctx context.Context, cfg *config.Config) (string, error) {
	st := cfg.Server.State
	if st.Backend != "postgres" {
		return "", fmt.Errorf("server.state.backend is %q, not postgres: the file store has no schema", st.Backend)
	}
	dsn := st.DSN
	switch {
	case st.DSNRef != "":
		v, err := secrets.Resolve(ctx, st.DSNRef)
		if err != nil {
			return "", fmt.Errorf("server.state: %w", err)
		}
		dsn = v
	case st.DSNEnv != "":
		dsn = os.Getenv(st.DSNEnv)
	}
	if dsn == "" {
		return "", fmt.Errorf("server.state: postgres DSN is empty (set %s)", st.DSNEnv)
	}
	return dsn, nil
}

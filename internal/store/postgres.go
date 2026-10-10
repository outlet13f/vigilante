package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"vigilante/internal/journal"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationLock is the advisory lock key serialising schema migrations when
// several nodes start at once.
const migrationLock = 72_105_118_105 // "vigi"

type pgStore struct {
	pool *pgxpool.Pool
	desc string

	mu         sync.RWMutex
	fenceKey   string
	fenceOwner string
	chainKey   []byte
}

// OpenPostgres connects, applies pending migrations and returns the store.
func OpenPostgres(ctx context.Context, dsn string) (Store, error) {
	return OpenPostgresWith(ctx, dsn, true)
}

// OpenPostgresWith is OpenPostgres; without autoMigrate it refuses a schema
// with pending migrations (server.state.auto_migrate: false).
func OpenPostgresWith(ctx context.Context, dsn string, autoMigrate bool) (Store, error) {
	pool, err := connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	ms, err := loadMigrations(migrationsFS)
	if err == nil {
		_, err = migrateUp(ctx, pool, ms, autoMigrate)
	}
	if err != nil {
		pool.Close()
		return nil, err
	}
	cfg := pool.Config()
	desc := fmt.Sprintf("postgres://%s:%d/%s", cfg.ConnConfig.Host, cfg.ConnConfig.Port, cfg.ConnConfig.Database)
	return &pgStore{pool: pool, desc: desc}, nil
}

func (p *pgStore) Describe() string { return p.desc }

func (p *pgStore) Close() error {
	p.pool.Close()
	return nil
}

func (p *pgStore) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func (p *pgStore) Fence(key, owner string) {
	p.mu.Lock()
	p.fenceKey, p.fenceOwner = key, owner
	p.mu.Unlock()
}

// SetChainKey: the MAC is part of the JSON body, so the schema is unchanged.
func (p *pgStore) SetChainKey(key []byte) {
	p.mu.Lock()
	p.chainKey = key
	p.mu.Unlock()
}

// chainLock serialises appends so every entry links to the one before it,
// even when several processes write to the same database.
const chainLock = 72_105_118_106

// Append seals the event onto the hash chain and inserts it. Reading the
// chain head and inserting happen in one transaction under an advisory lock;
// when fenced, the insert only happens if this node still holds the fence
// lease (checked in the same statement, on the database clock).
func (p *pgStore) Append(ctx context.Context, e journal.Entry) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	p.mu.RLock()
	key, owner, chainKey := p.fenceKey, p.fenceOwner, p.chainKey
	p.mu.RUnlock()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", chainLock); err != nil {
		return err
	}
	var head *string
	err = tx.QueryRow(ctx, "SELECT body->>'hash' FROM vigilante_events ORDER BY seq DESC LIMIT 1").Scan(&head)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	prev := ""
	if head != nil {
		prev = *head
	}
	e.Seal(prev, chainKey)
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO vigilante_events (at, kind, service, deployment_id, body)
		SELECT $1, $2, $3, $4, $5
		WHERE $6 = '' OR EXISTS (
			SELECT 1 FROM vigilante_leases WHERE key = $6 AND owner = $7 AND expires_at > now())`,
		e.Time, e.Kind, serviceOf(e), deploymentOf(e), body, key, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrFenced
	}
	return tx.Commit(ctx)
}

// Prune archives and deletes the oldest entries in one transaction under the
// chain lock, and puts the anchor at the last removed sequence number, so the
// anchor still sorts before every remaining entry.
func (p *pgStore) Prune(ctx context.Context, before time.Time, archive io.Writer) (int, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", chainLock); err != nil {
		return 0, err
	}
	// The prefix ends before the first entry that is new enough to keep.
	var keepFrom *int64
	if err := tx.QueryRow(ctx, "SELECT min(seq) FROM vigilante_events WHERE at >= $1", before).Scan(&keepFrom); err != nil {
		return 0, err
	}
	q := "SELECT seq, body FROM vigilante_events ORDER BY seq"
	args := []any{}
	if keepFrom != nil {
		q = "SELECT seq, body FROM vigilante_events WHERE seq < $1 ORDER BY seq"
		args = append(args, *keepFrom)
	}
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	var pruned []journal.Entry
	var lastSeq int64
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&lastSeq, &body); err != nil {
			rows.Close()
			return 0, err
		}
		var e journal.Entry
		if err := json.Unmarshal(body, &e); err != nil {
			rows.Close()
			return 0, fmt.Errorf("seq %d: %w", lastSeq, err)
		}
		pruned = append(pruned, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(pruned) == 0 {
		return 0, nil
	}
	if err := writeArchive(archive, pruned); err != nil {
		return 0, err
	}
	anchor := anchorFor(pruned, before)
	body, err := json.Marshal(anchor)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM vigilante_events WHERE seq <= $1", lastSeq); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO vigilante_events (seq, at, kind, body) VALUES ($1, $2, $3, $4)`,
		lastSeq, anchor.Time, anchor.Kind, body); err != nil {
		return 0, err
	}
	return len(pruned), tx.Commit(ctx)
}

func (p *pgStore) Scan(ctx context.Context, fn func(pos int64, e journal.Entry) error) error {
	rows, err := p.pool.Query(ctx, "SELECT seq, body FROM vigilante_events ORDER BY seq")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var body []byte
		if err := rows.Scan(&seq, &body); err != nil {
			return err
		}
		var e journal.Entry
		if err := json.Unmarshal(body, &e); err != nil {
			return fmt.Errorf("seq %d: unreadable entry: %w", seq, err)
		}
		if err := fn(seq, e); err != nil {
			return err
		}
	}
	return rows.Err()
}

func serviceOf(e journal.Entry) string {
	if e.Service != "" {
		return e.Service
	}
	if e.Deployment != nil {
		return e.Deployment.Service
	}
	return ""
}

func deploymentOf(e journal.Entry) string {
	if e.DeployID != "" {
		return e.DeployID
	}
	if e.Deployment != nil {
		return e.Deployment.ID
	}
	return ""
}

func (p *pgStore) Load(ctx context.Context) (*journal.State, error) {
	rows, err := p.pool.Query(ctx, "SELECT body FROM vigilante_events ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	st := journal.NewState()
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var e journal.Entry
		if err := json.Unmarshal(body, &e); err != nil {
			st.Corrupt++
			continue
		}
		st.Apply(e)
	}
	return st, rows.Err()
}

// TryLease is a single upsert: it takes the row if it is free, expired or
// already ours, and otherwise changes nothing. Expiry uses now() on the
// database, so node clocks never decide who holds a lease.
func (p *pgStore) TryLease(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	var got string
	err := p.pool.QueryRow(ctx, `
		INSERT INTO vigilante_leases (key, owner, expires_at)
		VALUES ($1, $2, now() + $3 * interval '1 millisecond')
		ON CONFLICT (key) DO UPDATE
			SET owner = EXCLUDED.owner, expires_at = EXCLUDED.expires_at
			WHERE vigilante_leases.owner = EXCLUDED.owner OR vigilante_leases.expires_at <= now()
		RETURNING owner`, key, owner, ttl.Milliseconds()).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return got == owner, nil
}

func (p *pgStore) ReleaseLease(ctx context.Context, key, owner string) error {
	_, err := p.pool.Exec(ctx, "DELETE FROM vigilante_leases WHERE key = $1 AND owner = $2", key, owner)
	return err
}

func (p *pgStore) LeaseHolder(ctx context.Context, key string) (string, error) {
	var owner string
	err := p.pool.QueryRow(ctx, "SELECT owner FROM vigilante_leases WHERE key = $1 AND expires_at > now()", key).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return owner, err
}

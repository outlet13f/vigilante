package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
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
}

// OpenPostgres connects, applies pending migrations and returns the store.
func OpenPostgres(ctx context.Context, dsn string) (Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	desc := fmt.Sprintf("postgres://%s:%d/%s", cfg.ConnConfig.Host, cfg.ConnConfig.Port, cfg.ConnConfig.Database)
	return &pgStore{pool: pool, desc: desc}, nil
}

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	files, _ := fs.Glob(migrationsFS, "migrations/*.sql")
	sort.Strings(files)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLock); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS vigilante_schema (
		version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	for _, f := range files {
		base := strings.TrimPrefix(f, "migrations/")
		v, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", base)
		}
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM vigilante_schema WHERE version=$1)", v).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, _ := migrationsFS.ReadFile(f)
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %s: %w", base, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO vigilante_schema(version) VALUES ($1)", v); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (p *pgStore) Describe() string { return p.desc }

func (p *pgStore) Close() error {
	p.pool.Close()
	return nil
}

func (p *pgStore) Fence(key, owner string) {
	p.mu.Lock()
	p.fenceKey, p.fenceOwner = key, owner
	p.mu.Unlock()
}

// Append inserts the event; when fenced, the insert only happens if this
// node still holds the fence lease (checked in the same statement, on the
// database clock).
func (p *pgStore) Append(ctx context.Context, e journal.Entry) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	p.mu.RLock()
	key, owner := p.fenceKey, p.fenceOwner
	p.mu.RUnlock()
	tag, err := p.pool.Exec(ctx, `
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
	return nil
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

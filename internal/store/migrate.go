package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Schema migrations for the PostgreSQL store.
//
// Files are migrations/NNN_name.sql, applied in order inside one transaction
// under an advisory lock (nodes starting together migrate once). Optional
// NNN_name.down.sql reverts NNN for a downgrade (`vigilante store migrate
// --down-to`). Policy (docs/08-upgrade.md): within a major version a
// migration only adds, so a node still running the previous release keeps
// working during a rolling upgrade. A migration that an older binary cannot
// live with carries the line "-- vigilante:breaking"; it is recorded in the
// schema table, and a binary that does not know it refuses to start instead
// of corrupting state (the downgrade guard).

// BinaryVersion is recorded with each migration this process applies; set by main.
var BinaryVersion = "dev"

const breakingMarker = "-- vigilante:breaking"

type Migration struct {
	Version  int
	Name     string
	Up, Down string // Down is empty when the migration cannot be reverted
	Breaking bool
}

// AppliedMigration is a row of vigilante_schema.
type AppliedMigration struct {
	Version   int       `json:"version"`
	AppliedAt time.Time `json:"applied_at"`
	Breaking  bool      `json:"breaking"`
	By        string    `json:"applied_by"` // binary version
}

// SchemaStatus compares the database with the migrations this binary carries.
type SchemaStatus struct {
	Applied []AppliedMigration `json:"applied"`
	Pending []int              `json:"pending"` // known here, not applied yet
	Unknown []AppliedMigration `json:"unknown"` // applied by a newer binary
	Latest  int                `json:"latest"`  // newest migration this binary knows
}

// ErrNewerSchema: the database was migrated by a newer release with a
// breaking change this binary does not understand.
var ErrNewerSchema = errors.New("state store schema is newer than this binary")

// ErrPendingMigrations: auto_migrate is off and the schema is behind.
var ErrPendingMigrations = errors.New("state store schema migrations are pending")

func loadMigrations(fsys fs.FS) ([]Migration, error) {
	files, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	byVersion := map[int]*Migration{}
	for _, f := range files {
		base := path.Base(f)
		down := strings.HasSuffix(base, ".down.sql")
		num, rest, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %s: name must be NNN_name.sql", base)
		}
		body, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		m := byVersion[v]
		if m == nil {
			m = &Migration{Version: v}
			byVersion[v] = m
		}
		if down {
			m.Down = string(body)
			continue
		}
		if m.Up != "" {
			return nil, fmt.Errorf("migration %d: two files", v)
		}
		m.Name = strings.TrimSuffix(rest, ".sql")
		m.Up = string(body)
		m.Breaking = strings.Contains(m.Up, breakingMarker)
	}
	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.Up == "" {
			return nil, fmt.Errorf("migration %d: down file without up file", m.Version)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1, 2, 3...: found %d at position %d", m.Version, i+1)
		}
	}
	return out, nil
}

// lockSchema starts a transaction holding the migration lock, with the
// schema table in place (older databases get its newer columns).
func lockSchema(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	for _, q := range []string{
		"SELECT pg_advisory_xact_lock(" + strconv.Itoa(migrationLock) + ")",
		`CREATE TABLE IF NOT EXISTS vigilante_schema (
			version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`,
		`ALTER TABLE vigilante_schema ADD COLUMN IF NOT EXISTS breaking boolean NOT NULL DEFAULT false`,
		`ALTER TABLE vigilante_schema ADD COLUMN IF NOT EXISTS applied_by text NOT NULL DEFAULT ''`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
	}
	return tx, nil
}

func schemaStatus(ctx context.Context, q pgx.Tx, ms []Migration) (SchemaStatus, error) {
	st := SchemaStatus{Applied: []AppliedMigration{}, Pending: []int{}, Unknown: []AppliedMigration{}}
	if len(ms) > 0 {
		st.Latest = ms[len(ms)-1].Version
	}
	rows, err := q.Query(ctx, "SELECT version, applied_at, breaking, applied_by FROM vigilante_schema ORDER BY version")
	if err != nil {
		return st, err
	}
	applied := map[int]bool{}
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.AppliedAt, &a.Breaking, &a.By); err != nil {
			rows.Close()
			return st, err
		}
		st.Applied = append(st.Applied, a)
		applied[a.Version] = true
		if a.Version > st.Latest {
			st.Unknown = append(st.Unknown, a)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}
	for _, m := range ms {
		if !applied[m.Version] {
			st.Pending = append(st.Pending, m.Version)
		}
	}
	return st, nil
}

// guard refuses a schema that a newer release changed incompatibly.
func (st SchemaStatus) guard() error {
	for _, u := range st.Unknown {
		if u.Breaking {
			return fmt.Errorf("%w: migration %d (applied by vigilante %s) is not known to this binary (%s, knows up to %d) and older releases cannot use it; "+
				"run vigilante %s or newer, or revert with `vigilante store migrate --down-to %d` from that release",
				ErrNewerSchema, u.Version, u.By, BinaryVersion, st.Latest, u.By, st.Latest)
		}
	}
	return nil
}

// migrateUp applies pending migrations (or, without auto, only checks).
func migrateUp(ctx context.Context, pool *pgxpool.Pool, ms []Migration, auto bool) (applied []int, err error) {
	tx, err := lockSchema(ctx, pool)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	st, err := schemaStatus(ctx, tx, ms)
	if err != nil {
		return nil, err
	}
	if err := st.guard(); err != nil {
		return nil, err
	}
	if len(st.Pending) > 0 && !auto {
		return nil, fmt.Errorf("%w: %v (server.state.auto_migrate is off: run `vigilante store migrate` first)", ErrPendingMigrations, st.Pending)
	}
	pending := map[int]bool{}
	for _, v := range st.Pending {
		pending[v] = true
	}
	for _, m := range ms {
		if !pending[m.Version] {
			continue
		}
		if _, err := tx.Exec(ctx, m.Up); err != nil {
			return nil, fmt.Errorf("migration %03d_%s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO vigilante_schema(version, breaking, applied_by) VALUES ($1, $2, $3)",
			m.Version, m.Breaking, BinaryVersion); err != nil {
			return nil, err
		}
		applied = append(applied, m.Version)
	}
	return applied, tx.Commit(ctx)
}

// migrateDown reverts every applied migration above to, newest first. All
// of them must have a down file; nothing changes otherwise.
func migrateDown(ctx context.Context, pool *pgxpool.Pool, ms []Migration, to int) (reverted []int, err error) {
	if to < 1 {
		return nil, errors.New("--down-to must be at least 1 (the first migration holds the data and is never reverted)")
	}
	tx, err := lockSchema(ctx, pool)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	st, err := schemaStatus(ctx, tx, ms)
	if err != nil {
		return nil, err
	}
	if len(st.Unknown) > 0 {
		return nil, fmt.Errorf("migration %d was applied by vigilante %s, which this binary does not know: downgrade with that release's binary",
			st.Unknown[len(st.Unknown)-1].Version, st.Unknown[len(st.Unknown)-1].By)
	}
	known := map[int]Migration{}
	for _, m := range ms {
		known[m.Version] = m
	}
	for i := len(st.Applied) - 1; i >= 0; i-- {
		v := st.Applied[i].Version
		if v <= to {
			break
		}
		m := known[v]
		if m.Down == "" {
			return nil, fmt.Errorf("migration %03d_%s cannot be reverted (no down file)", m.Version, m.Name)
		}
		if _, err := tx.Exec(ctx, m.Down); err != nil {
			return nil, fmt.Errorf("revert %03d_%s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM vigilante_schema WHERE version=$1", v); err != nil {
			return nil, err
		}
		reverted = append(reverted, v)
	}
	return reverted, tx.Commit(ctx)
}

// ---------------------------------------------------------------- CLI entry points

func connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
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
	return pool, nil
}

// Schema reports the schema of the database at dsn.
func Schema(ctx context.Context, dsn string) (SchemaStatus, error) {
	ms, err := loadMigrations(migrationsFS)
	if err != nil {
		return SchemaStatus{}, err
	}
	pool, err := connect(ctx, dsn)
	if err != nil {
		return SchemaStatus{}, err
	}
	defer pool.Close()
	tx, err := lockSchema(ctx, pool)
	if err != nil {
		return SchemaStatus{}, err
	}
	defer tx.Rollback(ctx)
	return schemaStatus(ctx, tx, ms)
}

// Migrate applies pending migrations, or with downTo > 0 reverts to that version.
func Migrate(ctx context.Context, dsn string, downTo int) (changed []int, err error) {
	ms, err := loadMigrations(migrationsFS)
	if err != nil {
		return nil, err
	}
	pool, err := connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	if downTo > 0 {
		return migrateDown(ctx, pool, ms, downTo)
	}
	return migrateUp(ctx, pool, ms, true)
}

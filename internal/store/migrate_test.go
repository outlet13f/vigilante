package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"

	"vigilante/internal/store/pgtest"
)

func migrationSet(t *testing.T, files map[string]string) []Migration {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys["migrations/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	ms, err := loadMigrations(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

var releases = map[string]string{
	"001_init.sql":        "CREATE TABLE t_events (seq bigserial PRIMARY KEY);",
	"002_extra.sql":       "ALTER TABLE t_events ADD COLUMN note text;",
	"002_extra.down.sql":  "ALTER TABLE t_events DROP COLUMN note;",
	"003_rename.sql":      "-- vigilante:breaking (old binaries read column note)\nALTER TABLE t_events RENAME COLUMN note TO remark;",
	"003_rename.down.sql": "ALTER TABLE t_events RENAME COLUMN remark TO note;",
}

func only(files map[string]string, upTo string) map[string]string {
	out := map[string]string{}
	for k, v := range files {
		if k[:3] <= upTo {
			out[k] = v
		}
	}
	return out
}

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := connect(context.Background(), pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestLoadMigrations(t *testing.T) {
	ms := migrationSet(t, releases)
	if len(ms) != 3 || ms[1].Name != "extra" || ms[1].Down == "" || ms[0].Down != "" || ms[1].Breaking || !ms[2].Breaking {
		t.Fatalf("parsed %+v", ms)
	}
	for name, files := range map[string]map[string]string{
		"gap":       {"001_a.sql": "x", "003_c.sql": "x"},
		"down only": {"001_a.sql": "x", "002_b.down.sql": "x"},
		"bad name":  {"one_a.sql": "x"},
	} {
		fsys := fstest.MapFS{}
		for f, b := range files {
			fsys["migrations/"+f] = &fstest.MapFile{Data: []byte(b)}
		}
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := loadMigrations(migrationsFS); err != nil {
		t.Fatalf("shipped migrations: %v", err)
	}
}

func TestMigrateUpgradeDowngradeAndGuard(t *testing.T) {
	ctx := context.Background()
	p := pool(t)
	t.Cleanup(func() { BinaryVersion = "dev" })
	v1, v2, v3 := migrationSet(t, only(releases, "001")), migrationSet(t, only(releases, "002")), migrationSet(t, releases)

	// Release 1 installs, release 2 upgrades (additive).
	BinaryVersion = "1.0.0"
	if got, err := migrateUp(ctx, p, v1, true); err != nil || len(got) != 1 {
		t.Fatalf("install: %v %v", got, err)
	}
	// auto_migrate off: release 2 refuses until migrated explicitly.
	if _, err := migrateUp(ctx, p, v2, false); !errors.Is(err, ErrPendingMigrations) {
		t.Fatalf("pending without auto_migrate: %v", err)
	}
	BinaryVersion = "1.1.0"
	if got, err := migrateUp(ctx, p, v2, true); err != nil || len(got) != 1 || got[0] != 2 {
		t.Fatalf("upgrade: %v %v", got, err)
	}
	if got, _ := migrateUp(ctx, p, v2, true); len(got) != 0 {
		t.Fatalf("second run must be a no-op: %v", got)
	}

	// Rolling upgrade: a node still on release 1 keeps running on the additive schema.
	BinaryVersion = "1.0.0"
	if _, err := migrateUp(ctx, p, v1, true); err != nil {
		t.Fatalf("release 1 on an additive newer schema: %v", err)
	}

	// Release 3 makes a breaking change: release 2 must refuse to start.
	BinaryVersion = "2.0.0"
	if _, err := migrateUp(ctx, p, v3, true); err != nil {
		t.Fatal(err)
	}
	BinaryVersion = "1.1.0"
	_, err := migrateUp(ctx, p, v2, true)
	if !errors.Is(err, ErrNewerSchema) || !strings.Contains(err.Error(), "2.0.0") || !strings.Contains(err.Error(), "--down-to 2") {
		t.Fatalf("downgrade guard: %v", err)
	}
	// Reverting needs the binary that knows the migration.
	if _, err := migrateDown(ctx, p, v2, 1); err == nil {
		t.Fatal("release 2 reverted a migration it does not know")
	}

	// Release 3 reverts to 2, after which release 2 starts again.
	BinaryVersion = "2.0.0"
	if got, err := migrateDown(ctx, p, v3, 2); err != nil || len(got) != 1 || got[0] != 3 {
		t.Fatalf("down to 2: %v %v", got, err)
	}
	BinaryVersion = "1.1.0"
	if _, err := migrateUp(ctx, p, v2, true); err != nil {
		t.Fatalf("release 2 after the revert: %v", err)
	}
	var col string
	if err := p.QueryRow(ctx, "SELECT column_name FROM information_schema.columns WHERE table_name='t_events' AND column_name IN ('note','remark')").Scan(&col); err != nil || col != "note" {
		t.Fatalf("column after revert: %q %v", col, err)
	}

	// The first migration holds the data and is never reverted.
	if _, err := migrateDown(ctx, p, v2, 0); err == nil {
		t.Fatal("reverting everything was allowed")
	}
	tx, _ := lockSchema(ctx, p)
	st, err := schemaStatus(ctx, tx, v2)
	_ = tx.Rollback(ctx)
	if err != nil || len(st.Applied) != 2 || st.Applied[1].By != "1.1.0" || len(st.Pending) != 0 || len(st.Unknown) != 0 {
		t.Fatalf("status %+v %v", st, err)
	}
}

func TestDowngradeIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	p := pool(t)
	files := map[string]string{
		"001_a.sql": "CREATE TABLE t1 (id int);",
		"002_b.sql": "CREATE TABLE t2 (id int);", // no down file
		"003_c.sql": "CREATE TABLE t3 (id int);", "003_c.down.sql": "DROP TABLE t3;",
	}
	ms := migrationSet(t, files)
	if _, err := migrateUp(ctx, p, ms, true); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateDown(ctx, p, ms, 1); err == nil || !strings.Contains(err.Error(), "002_b cannot be reverted") {
		t.Fatalf("irreversible step: %v", err)
	}
	var n int
	_ = p.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name='t3'").Scan(&n)
	if n != 1 {
		t.Fatal("a failed downgrade must change nothing (003 was reverted)")
	}
}

// Databases created before the schema table had its newer columns upgrade in place.
func TestSchemaTableFromOlderRelease(t *testing.T) {
	ctx := context.Background()
	p := pool(t)
	if _, err := p.Exec(ctx, `CREATE TABLE t_events (seq bigserial PRIMARY KEY);
		CREATE TABLE vigilante_schema (version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO vigilante_schema(version) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	ms := migrationSet(t, only(releases, "002"))
	got, err := migrateUp(ctx, p, ms, true)
	if err != nil || len(got) != 1 || got[0] != 2 {
		t.Fatalf("upgrade of an old schema table: %v %v", got, err)
	}
}

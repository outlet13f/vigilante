// Package pgtest gives tests a real, empty PostgreSQL database.
//
// With VIGILANTE_TEST_PG_DSN set (a superuser DSN, e.g. in CI) it uses that
// server; otherwise it starts an embedded PostgreSQL once per test binary
// (downloaded on first use and cached). Each DSN call creates a fresh
// database that is dropped when the test ends. If neither is possible the
// test is skipped, never failed. Call Stop from TestMain.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
)

var (
	once     sync.Once
	adminDSN string
	startErr error
	embedded *embeddedpostgres.EmbeddedPostgres
	tmpDir   string
)

func start() {
	if dsn := os.Getenv("VIGILANTE_TEST_PG_DSN"); dsn != "" {
		adminDSN = dsn
		return
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		startErr = err
		return
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	tmpDir, err = os.MkdirTemp("", "vigilante-pg-")
	if err != nil {
		startErr = err
		return
	}
	embedded = embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(port)).
		RuntimePath(filepath.Join(tmpDir, "runtime")).
		DataPath(filepath.Join(tmpDir, "data")).
		StartTimeout(90 * time.Second).
		Logger(nil))
	if err := embedded.Start(); err != nil {
		startErr = err
		embedded = nil
		return
	}
	adminDSN = fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
}

// DSN returns a DSN for a new empty database, or skips the test.
func DSN(t testing.TB) string {
	t.Helper()
	once.Do(start)
	if startErr != nil {
		t.Skipf("no PostgreSQL for tests (set VIGILANTE_TEST_PG_DSN): %v", startErr)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Skipf("PostgreSQL unreachable: %v", err)
	}
	defer admin.Close(ctx)
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "vigilante_t_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if c, err := pgx.Connect(context.Background(), adminDSN); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	u, _ := url.Parse(adminDSN)
	u.Path = "/" + name
	return u.String()
}

// Stop shuts down the embedded server (if one was started).
func Stop() {
	if embedded != nil {
		_ = embedded.Stop()
	}
	if tmpDir != "" {
		_ = os.RemoveAll(tmpDir)
	}
}

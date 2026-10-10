package probe

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"vigilante/internal/config"
	"vigilante/internal/secrets"
	"vigilante/internal/tmpl"
)

func init() { Register("db", newDB) }

// DB probe verifies that a freshly deployed instance's database is reachable
// with a full connection pool: it acquires pool_size connections at the same
// time (catching max_connections / pool exhaustion), then runs the query on each.
// Metrics: up, pool_acquire_ms, query_ms, pool_acquired, consecutive_failures.
//
// For JDBC pools inside the app (HikariCP etc.) prefer an http probe on the
// actuator endpoint with json_path: components.db.status.
type dbProbe struct {
	spec config.Probe
	db   *sql.DB
	fc   failureCounter
}

var sqlDrivers = map[string]string{"postgres": "pgx", "mysql": "mysql"}

func newDB(spec config.Probe, env Env) (Probe, error) {
	dsn := spec.DB.DSN
	switch {
	case spec.DB.DSNRef != "":
		v, err := secrets.Resolve(context.Background(), spec.DB.DSNRef)
		if err != nil {
			return nil, err
		}
		dsn = v
	case spec.DB.DSNEnv != "":
		dsn = os.Getenv(spec.DB.DSNEnv)
		if dsn == "" {
			return nil, fmt.Errorf("env %s is empty", spec.DB.DSNEnv)
		}
	}
	dsn, err := tmpl.Render(dsn, env.Data)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(sqlDrivers[spec.DB.Driver], dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(spec.DB.PoolSize)
	db.SetMaxIdleConns(0) // force fresh connections every round: we test connectability, not reuse
	return &dbProbe{spec: spec, db: db}, nil
}

func (p *dbProbe) Run(ctx context.Context, emit Emit) error {
	defer p.db.Close()
	return poll(ctx, p.spec.Interval, func(ctx context.Context) {
		acquired, acquire, query, err := p.round(ctx)
		emit("pool_acquired", float64(acquired))
		if err == nil {
			emit("pool_acquire_ms", ms(acquire))
			emit("query_ms", ms(query))
		}
		p.fc.observe(emit, err == nil, err != nil && isTimeout(err))
	})
}

func (p *dbProbe) Close() error { return p.db.Close() }

func (p *dbProbe) Check(ctx context.Context) error {
	_, _, _, err := p.round(ctx)
	return err
}

func (p *dbProbe) round(ctx context.Context) (acquired int, acquire, query time.Duration, err error) {
	ctx, cancel := context.WithTimeout(ctx, p.spec.Timeout)
	defer cancel()
	n := p.spec.DB.PoolSize
	conns := make([]*sql.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	start := time.Now()
	type res struct {
		c   *sql.Conn
		err error
	}
	ch := make(chan res, n)
	for range n {
		go func() {
			c, err := p.db.Conn(ctx)
			ch <- res{c, err}
		}()
	}
	var firstErr error
	for range n {
		r := <-ch
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		conns = append(conns, r.c)
	}
	acquire = time.Since(start)
	if firstErr != nil {
		return len(conns), acquire, 0, fmt.Errorf("pool acquire %d/%d: %w", len(conns), n, firstErr)
	}
	qs := time.Now()
	for _, c := range conns {
		var one any
		if err := c.QueryRowContext(ctx, p.spec.DB.Query).Scan(&one); err != nil {
			return len(conns), acquire, 0, fmt.Errorf("query: %w", err)
		}
	}
	return len(conns), acquire, time.Since(qs) / time.Duration(len(conns)), nil
}

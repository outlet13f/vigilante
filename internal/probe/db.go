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

// DB probe verifies that a freshly deployed instance's database is reachable.
// Every interval it runs the query on one reused connection (cheap for the
// database); every pool_check_interval (default 1m) it acquires pool_size new
// connections at the same time (catching max_connections / pool exhaustion)
// and runs the query on each.
// Metrics: up, query_ms, pool_acquire_ms and pool_acquired (full checks),
// consecutive_failures.
//
// For JDBC pools inside the app (HikariCP etc.) prefer an http probe on the
// actuator endpoint with json_path: components.db.status.
type dbProbe struct {
	spec     config.Probe
	db       *sql.DB // the full check: fresh connections every time
	light    *sql.DB // one connection, kept open
	every    time.Duration
	lastFull time.Time
	fc       failureCounter
}

// DefaultPoolCheckInterval is how often the full-pool check runs.
const DefaultPoolCheckInterval = time.Minute

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
	db.SetMaxIdleConns(0) // force fresh connections every full check: we test connectability, not reuse
	light, err := sql.Open(sqlDrivers[spec.DB.Driver], dsn)
	if err != nil {
		db.Close()
		return nil, err
	}
	light.SetMaxOpenConns(1)
	light.SetMaxIdleConns(1)
	every := DefaultPoolCheckInterval
	if spec.DB.PoolCheckInterval != nil {
		every = *spec.DB.PoolCheckInterval
	}
	return &dbProbe{spec: spec, db: db, light: light, every: every}, nil
}

func (p *dbProbe) Run(ctx context.Context, emit Emit) error {
	defer p.Close()
	return poll(ctx, p.spec.Interval, func(ctx context.Context) {
		now := time.Now()
		var err error
		if p.every == 0 || p.lastFull.IsZero() || now.Sub(p.lastFull) >= p.every {
			var acquired int
			var acquire, query time.Duration
			acquired, acquire, query, err = p.round(ctx)
			p.lastFull = now
			emit("pool_acquired", float64(acquired))
			if err == nil {
				emit("pool_acquire_ms", ms(acquire))
				emit("query_ms", ms(query))
			}
		} else {
			var query time.Duration
			if query, err = p.ping(ctx); err == nil {
				emit("query_ms", ms(query))
			}
		}
		p.fc.observe(emit, err == nil, err != nil && isTimeout(err))
	})
}

// ping runs the query on the kept connection.
func (p *dbProbe) ping(ctx context.Context) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, p.spec.Timeout)
	defer cancel()
	start := time.Now()
	var one any
	if err := p.light.QueryRowContext(ctx, p.spec.DB.Query).Scan(&one); err != nil {
		return 0, fmt.Errorf("query: %w", err)
	}
	return time.Since(start), nil
}

func (p *dbProbe) Close() error {
	p.light.Close()
	return p.db.Close()
}

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

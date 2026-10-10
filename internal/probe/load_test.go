package probe

import (
	"os"
	"strings"
	"testing"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/store/pgtest"
	"vigilante/internal/transport"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Stop()
	os.Exit(code)
}

// remote_grep filters on the target; the probe then has no line count.
func TestLogRemoteGrep(t *testing.T) {
	if got := RemoteFollowCmd("/var/log/app.log", "OutOfMemory|FATAL"); got != "tail -n0 -F '/var/log/app.log' 2>/dev/null | grep --line-buffered -E 'OutOfMemory|FATAL'" {
		t.Fatalf("command %q", got)
	}
	m := &transport.Mock{Lines: []string{"FATAL disk full", "FATAL again"}}
	spec := config.Probe{ID: "app", Type: "log", Interval: time.Second, Log: &config.LogProbe{Path: "/var/log/app.log",
		Patterns: map[string]string{"fatal": "FATAL"}, RemoteGrep: "FATAL|OutOfMemory"}}
	k := runFor(t, 1500*time.Millisecond, []Job{{Target: config.Target{Name: "a"}, Spec: spec}}, m)
	if sum(k.values("app.match.fatal")) != 2 {
		t.Fatalf("matches %v", k.values("app.match.fatal"))
	}
	if len(k.values("app.lines")) != 0 {
		t.Fatal("lines must not be emitted when lines are filtered on the target")
	}
	if !strings.Contains(m.Joined(), "grep --line-buffered -E 'FATAL|OutOfMemory'") {
		t.Fatalf("stream command: %s", m.Joined())
	}
	// Without remote_grep everything is streamed and counted.
	m2 := &transport.Mock{Lines: []string{"a", "FATAL b"}}
	spec.Log.RemoteGrep = ""
	k2 := runFor(t, 1500*time.Millisecond, []Job{{Target: config.Target{Name: "a"}, Spec: spec}}, m2)
	if sum(k2.values("app.lines")) != 2 || strings.Contains(m2.Joined(), "grep") {
		t.Fatalf("lines %v, command %s", k2.values("app.lines"), m2.Joined())
	}
}

// The DB probe reuses one connection between full-pool checks.
func TestDBProbeFullCheckCadence(t *testing.T) {
	dsn := pgtest.DSN(t)
	every := 400 * time.Millisecond
	spec := config.Probe{ID: "db", Type: "db", Interval: 50 * time.Millisecond, Timeout: 2 * time.Second,
		DB: &config.DBProbe{Driver: "postgres", DSN: dsn, PoolSize: 3, Query: "SELECT 1", PoolCheckInterval: &every}}
	k := runFor(t, 1200*time.Millisecond, []Job{{Target: config.Target{Name: "a"}, Spec: spec}}, nil)
	full, queries := k.values("db.pool_acquired"), k.values("db.query_ms")
	if len(full) < 2 || len(full) > 4 {
		t.Fatalf("full pool checks: %d (want about one per 400ms)", len(full))
	}
	for _, n := range full {
		if n != 3 {
			t.Fatalf("pool acquired %v", full)
		}
	}
	if len(queries) < 3*len(full) {
		t.Fatalf("light queries %d vs full checks %d: the light path is not running", len(queries), len(full))
	}
	if u := k.values("db.up"); len(u) == 0 || u[len(u)-1] != 1 {
		t.Fatalf("up %v", u)
	}
}

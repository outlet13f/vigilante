package audit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"vigilante/internal/config"
	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/safety"
	"vigilante/internal/store"
	"vigilante/internal/store/pgtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Stop()
	os.Exit(code)
}

type backend struct {
	st     store.Store
	tamper func(t *testing.T, pos int64, how string) // how: "edit" | "delete"
	put    func(t *testing.T, pos int64, e journal.Entry)
}

// recompute is an attacker with write access to the store but not the chain
// key: it changes entries and then recomputes every hash so the unkeyed
// chain verifies again. MACs stay as fn leaves them.
func (b backend) recompute(t *testing.T, fn func(i int, e *journal.Entry)) {
	t.Helper()
	var pos []int64
	var all []journal.Entry
	b.st.Scan(context.Background(), func(p int64, e journal.Entry) error {
		pos, all = append(pos, p), append(all, e)
		return nil
	})
	prev := ""
	for i := range all {
		fn(i, &all[i])
		if all[i].Kind == journal.KindAnchor {
			prev = all[i].Hash
		} else if all[i].Hash != "" {
			all[i].Chain(prev)
			prev = all[i].Hash
		}
		b.put(t, pos[i], all[i])
	}
}

func backends(t *testing.T) map[string]func(t *testing.T) backend {
	return map[string]func(t *testing.T) backend{
		"file": func(t *testing.T) backend {
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			st, err := store.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			return backend{st: st, tamper: func(t *testing.T, pos int64, how string) {
				b, _ := os.ReadFile(path)
				lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
				i := pos - 1
				switch how {
				case "edit":
					lines[i] = strings.Replace(lines[i], `"actor":"user:alice"`, `"actor":"user:mallory"`, 1)
				case "delete":
					lines = append(lines[:i], lines[i+1:]...)
				}
				os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
			}, put: func(t *testing.T, pos int64, e journal.Entry) {
				b, _ := os.ReadFile(path)
				lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
				line, _ := json.Marshal(e)
				lines[pos-1] = string(line)
				os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
			}}
		},
		"postgres": func(t *testing.T) backend {
			dsn := pgtest.DSN(t)
			st, err := store.OpenPostgres(context.Background(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			return backend{st: st, tamper: func(t *testing.T, pos int64, how string) {
				c, err := pgx.Connect(context.Background(), dsn)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close(context.Background())
				q := `UPDATE vigilante_events SET body = jsonb_set(body, '{actor}', '"user:mallory"') WHERE seq = $1`
				if how == "delete" {
					q = `DELETE FROM vigilante_events WHERE seq = $1`
				}
				if _, err := c.Exec(context.Background(), q, pos); err != nil {
					t.Fatal(err)
				}
			}, put: func(t *testing.T, pos int64, e journal.Entry) {
				c, err := pgx.Connect(context.Background(), dsn)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close(context.Background())
				body, _ := json.Marshal(e)
				if _, err := c.Exec(context.Background(), `UPDATE vigilante_events SET body = $2 WHERE seq = $1`, pos, body); err != nil {
					t.Fatal(err)
				}
			}}
		},
	}
}

func seed(t *testing.T, st store.Store, n int, at time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		e := journal.Entry{Kind: journal.KindAudit, Time: at.Add(time.Duration(i) * time.Second),
			Actor: "user:alice", Source: "api", Action: "rollback.manual", Service: "order", DeployID: "d1"}
		if err := st.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
}

func positions(t *testing.T, st store.Store) []int64 {
	var out []int64
	st.Scan(context.Background(), func(pos int64, _ journal.Entry) error { out = append(out, pos); return nil })
	return out
}

func TestChainDetectsTampering(t *testing.T) {
	for name, open := range backends(t) {
		for _, how := range []string{"edit", "delete"} {
			t.Run(name+"/"+how, func(t *testing.T) {
				b := open(t)
				seed(t, b.st, 5, time.Now())
				r, err := Verify(context.Background(), b.st, nil)
				if err != nil || !r.OK || r.Checked != 5 {
					t.Fatalf("intact chain: %+v %v", r, err)
				}
				pos := positions(t, b.st)
				b.tamper(t, pos[2], how)
				r, err = Verify(context.Background(), b.st, nil)
				if err != nil {
					t.Fatal(err)
				}
				want := map[string]string{"edit": "was modified", "delete": "removed or reordered"}[how]
				if r.OK || !strings.Contains(r.Broken, want) {
					t.Fatalf("tampering (%s) not detected: %+v", how, r)
				}
			})
		}
	}
}

func TestPruneArchivesAndKeepsState(t *testing.T) {
	for name, open := range backends(t) {
		t.Run(name, func(t *testing.T) {
			b := open(t)
			ctx := context.Background()
			old := time.Now().Add(-400 * 24 * time.Hour)
			app := func(e journal.Entry) {
				if err := b.st.Append(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			// Old history: a finished deployment, the last good one, an
			// in-flight rollback, and the circuit opening.
			app(journal.Entry{Kind: journal.KindDeployment, Time: old, Deployment: &model.Deployment{ID: "d0", Service: "order", Version: "v0", State: model.StateRolledBack, UpdatedAt: old}})
			app(journal.Entry{Kind: journal.KindDeployment, Time: old.Add(time.Minute), Deployment: &model.Deployment{ID: "d1", Service: "order", Version: "v1", State: model.StateSucceeded, UpdatedAt: old.Add(time.Minute)}})
			app(journal.Entry{Kind: journal.KindDeployment, Time: old.Add(2 * time.Minute), Deployment: &model.Deployment{ID: "d2", Service: "order", Version: "v2", State: model.StateRollingBack, UpdatedAt: old.Add(2 * time.Minute)}})
			app(journal.Entry{Kind: journal.KindStepDone, Time: old.Add(3 * time.Minute), DeployID: "d2", Target: "app-1", Step: 1})
			app(journal.Entry{Kind: journal.KindCircuit, Time: old.Add(4 * time.Minute), Circuit: &safety.CircuitState{State: safety.Open, Reason: "two failures"}})
			seed(t, b.st, 3, time.Now()) // recent audit records stay

			var archive bytes.Buffer
			n, err := b.st.Prune(ctx, time.Now().Add(-365*24*time.Hour), &archive)
			if err != nil || n != 5 {
				t.Fatalf("pruned %d: %v", n, err)
			}
			if r, err := VerifyReader(&archive, nil); err != nil || !r.OK || r.Checked != 5 {
				t.Fatalf("archive must verify on its own: %+v %v", r, err)
			}
			if r, err := Verify(ctx, b.st, nil); err != nil || !r.OK || r.Checked != 3 {
				t.Fatalf("chain after prune: %+v %v", r, err)
			}
			st, err := b.st.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, kept := st.Deployments["d0"]; kept {
				t.Error("a finished, superseded deployment should be compacted away")
			}
			if st.Deployments["d1"] == nil || st.Deployments["d2"] == nil || st.StepsDone["d2"]["app-1"] != 2 {
				t.Errorf("last-good and in-flight deployments must survive: %+v", st.Deployments)
			}
			if st.Circuit == nil || st.Circuit.State != safety.Open {
				t.Error("circuit state lost by pruning")
			}
			seed(t, b.st, 1, time.Now()) // appends keep chaining after the anchor
			if r, _ := Verify(ctx, b.st, nil); !r.OK || r.Checked != 4 {
				t.Fatalf("append after prune: %+v", r)
			}
		})
	}
}

func TestLegacyEntriesBeforeChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	os.WriteFile(path, []byte(`{"time":"2026-01-01T00:00:00Z","kind":"audit","message":"pre-chain"}`+"\n"), 0o644)
	st, err := store.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seed(t, st, 2, time.Now())
	r, err := Verify(context.Background(), st, nil)
	if err != nil || !r.OK || r.Legacy != 1 || r.Checked != 2 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestSyslogExporter(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	lines := make(chan string, 10)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	x, err := NewExporter(config.SyslogExport{Address: "tcp://" + l.Addr().String()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go x.Run(ctx)

	d := &model.Deployment{ID: "d9", Service: "order", State: model.StateRollingBack}
	x.Send(journal.Entry{Kind: journal.KindDeployment, Time: time.Now(), Deployment: d})
	x.Send(journal.Entry{Kind: journal.KindDeployment, Time: time.Now(), Deployment: d}) // same state: not resent
	x.Send(journal.Entry{Kind: journal.KindAudit, Time: time.Now(), Actor: "user:bob", Action: "denied", Reason: `POST /v1/circuit/reset: needs "admin"`})
	got := []string{}
	for len(got) < 2 {
		select {
		case l := <-lines:
			got = append(got, l)
		case <-time.After(5 * time.Second):
			t.Fatalf("received %d lines: %v", len(got), got)
		}
	}
	if !strings.HasPrefix(got[0], "<110>1 ") || !strings.Contains(got[0], "deployment.rolling_back") || !strings.Contains(got[0], `deployment="d9"`) {
		t.Errorf("deployment line: %s", got[0])
	}
	if !strings.HasPrefix(got[1], "<108>1 ") || !strings.Contains(got[1], `actor="user:bob"`) {
		t.Errorf("denied line (warning severity): %s", got[1])
	}
	select {
	case extra := <-lines:
		t.Errorf("unchanged deployment state was resent: %s", extra)
	case <-time.After(200 * time.Millisecond):
	}
	cef := FormatCEF(journal.Entry{Kind: journal.KindAudit, Action: "rollback.manual", Actor: "user:a=b", Reason: "pipe | and = sign"})
	if !strings.HasPrefix(cef, "CEF:0|Vigilante|vigilante|1|rollback.manual|pipe \\| and = sign|") || !strings.Contains(cef, `suser=user:a\=b`) {
		t.Errorf("cef escaping: %s", cef)
	}
}

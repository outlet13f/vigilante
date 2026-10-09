package orchestrator

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/journal"
	"vigilante/internal/model"
)

func opsEngine(t *testing.T, path string) *Engine {
	t.Helper()
	cfg, err := config.Parse([]byte(`
version: v1
server: {journal_path: ` + filepath.ToSlash(path) + `}
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true", on: local}}}
services:
  - name: svc
    targets: [a]
    probes: [{id: h, type: tcp, tcp: {address: "127.0.0.1:1"}}]
    rules: [{name: down, when: {metric: h.consecutive_failures, op: ">=", value: 3}}]
    rollback: {executor: x}
`))
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(cfg, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// A new leader (or a restarted node) still answers idempotent retries and
// reports operations the previous process started.
func TestOperationsAndIdempotencySurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	e1 := opsEngine(t, path)
	d, _ := e1.Create("d1", "svc", "v2", "v1")
	done := e1.StartOperation(model.OpRollback, "svc", d, "", "user:alice")
	e1.FinishOperation(done.ID, model.OpCompleted, &model.OperationResult{State: model.StateRolledBack}, "")
	orphan := e1.StartOperation(model.OpObservation, "svc", d, model.PhaseCanary, "sa:ci")
	e1.RememberIdempotent(&model.IdemRecord{Key: "k1", Fingerprint: "f1", Status: 202, Location: "/v2/operations/" + orphan.ID, CreatedAt: time.Now().UTC()})
	e1.RememberIdempotent(&model.IdemRecord{Key: "old", Fingerprint: "f", Status: 200, CreatedAt: time.Now().Add(-25 * time.Hour)})
	// While e1 runs the orphan, it is running.
	if op, _ := e1.Operation(orphan.ID); op.Status != model.OpRunning {
		t.Fatalf("live operation: %+v", op)
	}
	e1.Close()

	e2 := opsEngine(t, path)
	defer e2.Close()
	if r, ok := e2.Idempotent("k1"); !ok || r.Status != 202 || r.Location == "" {
		t.Fatalf("idempotency record lost: %+v %v", r, ok)
	}
	if _, ok := e2.Idempotent("old"); ok {
		t.Fatal("records older than 24h must not replay")
	}
	if op, ok := e2.Operation(done.ID); !ok || op.Status != model.OpCompleted || op.Result.State != model.StateRolledBack {
		t.Fatalf("finished operation: %+v", op)
	}
	// The orphan's deployment has not moved: still running.
	if op, _ := e2.Operation(orphan.ID); op.Status != model.OpRunning {
		t.Fatalf("orphan before its deployment moves: %+v", op)
	}
	// Once the deployment reaches an outcome, the orphan reads as completed.
	time.Sleep(10 * time.Millisecond)
	d2 := e2.Live("d1")
	e2.setState(d2, model.StateRolledBack, "finished by the new leader")
	op, _ := e2.Operation(orphan.ID)
	if op.Status != model.OpCompleted || op.Result.State != model.StateRolledBack || *op.Result.ExitCode != 2 {
		t.Fatalf("orphan after its deployment finished: %+v", op)
	}
	if ops := e2.Operations(); len(ops) != 2 || ops[0].ID != orphan.ID {
		t.Fatalf("newest first: %+v", ops)
	}
}

func TestCompactKeepsRunningOperationsAndFreshKeys(t *testing.T) {
	now := time.Now()
	running := &model.Operation{ID: "op_run", Status: model.OpRunning, CreatedAt: now.Add(-48 * time.Hour)}
	finished := &model.Operation{ID: "op_done", Status: model.OpCompleted, CreatedAt: now.Add(-48 * time.Hour)}
	pruned := []journal.Entry{
		{Kind: journal.KindOperation, Operation: running},
		{Kind: journal.KindOperation, Operation: finished},
		{Kind: journal.KindIdempotency, Idem: &model.IdemRecord{Key: "fresh", CreatedAt: now.Add(-time.Hour)}},
		{Kind: journal.KindIdempotency, Idem: &model.IdemRecord{Key: "stale", CreatedAt: now.Add(-30 * time.Hour)}},
	}
	st := journal.NewState()
	for _, e := range journal.Compact(pruned, now) {
		st.Apply(e)
	}
	if _, ok := st.Operations["op_run"]; !ok {
		t.Error("running operation dropped")
	}
	if _, ok := st.Operations["op_done"]; ok {
		t.Error("finished operation kept")
	}
	if _, ok := st.Idempotency["fresh"]; !ok {
		t.Error("fresh idempotency key dropped")
	}
	if _, ok := st.Idempotency["stale"]; ok {
		t.Error("stale idempotency key kept")
	}
}

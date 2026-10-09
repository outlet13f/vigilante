package orchestrator

import (
	"context"
	"testing"

	"vigilante/internal/model"
	"vigilante/internal/telemetry"
)

func TestTelemetryFollowsVerdictsAndRollbacks(t *testing.T) {
	verdicts := telemetry.Verdicts.Value("order", "canary", string(model.VerdictFail))
	rolledBack := telemetry.Rollbacks.Value("order", "rolled_back")
	blocked := telemetry.Rollbacks.Value("order", "blocked")
	triggers := telemetry.RollbackTrigger.Count()
	durations := telemetry.RollbackDuration.Count("rolled_back")
	evals := telemetry.Evaluation.Count()
	samples := telemetry.ProbeSamples.Value("http")
	appends := telemetry.StoreAppend.Count("file")

	h := newHarness(t, opts{maxPerHour: 1})
	h.app1.healthy.Store(false)
	d := h.deploy("t1")
	_ = h.e.Watch(context.Background(), d, model.PhaseCanary)
	if d.State != model.StateRolledBack {
		t.Fatalf("state %s", d.State)
	}
	h.app1.healthy.Store(false)
	_ = h.e.Watch(context.Background(), h.deploy("t2"), model.PhaseCanary) // flapping guard blocks

	for name, ok := range map[string]bool{
		"verdict FAIL counted twice":           telemetry.Verdicts.Value("order", "canary", string(model.VerdictFail)) == verdicts+2,
		"one rolled_back rollback":             telemetry.Rollbacks.Value("order", "rolled_back") == rolledBack+1,
		"one blocked rollback":                 telemetry.Rollbacks.Value("order", "blocked") == blocked+1,
		"trigger latency observed once":        telemetry.RollbackTrigger.Count() == triggers+1,
		"rollback duration observed once":      telemetry.RollbackDuration.Count("rolled_back") == durations+1,
		"evaluations timed":                    telemetry.Evaluation.Count() > evals,
		"http probe samples counted":           telemetry.ProbeSamples.Value("http") > samples,
		"store writes timed on the file store": telemetry.StoreAppend.Count("file") > appends,
	} {
		if !ok {
			t.Error(name)
		}
	}
}

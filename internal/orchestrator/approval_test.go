package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vigilante/internal/model"
)

const approveMode = "      mode: approve\n      approval: {timeout: 10m, drain_first: true}"

// failCanary runs a canary that fails in approve mode.
func failCanary(t *testing.T, h *harness, id string) *model.Deployment {
	t.Helper()
	h.app1.healthy.Store(false)
	d := h.deploy(id)
	if err := h.e.Watch(context.Background(), d, model.PhaseCanary); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestApproveModeWaitsDrainsAndApproves(t *testing.T) {
	h := newHarness(t, opts{escalation: approveMode})
	d := failCanary(t, h, "a1")

	if d.State != model.StateAwaitApproval || model.ExitCode(d) != 3 {
		t.Fatalf("state %s: %s", d.State, d.Reason)
	}
	pr, ok := h.e.PendingRollback("a1")
	if !ok || len(pr.Targets) != 1 || pr.Targets[0] != "app-1" || len(pr.Drained) != 1 || !pr.ExpiresAt.After(time.Now().Add(9*time.Minute)) {
		t.Fatalf("pending %+v", pr)
	}
	if down := h.lb.down(); len(down) != 1 || down[0] != "10.0.0.1:8080" {
		t.Fatalf("failing target should be isolated while waiting: %v", down)
	}
	if strings.Contains(h.host.joined(), "ln -sfn") {
		t.Fatal("nothing may be rolled back before approval")
	}

	if err := h.e.DecideRollback(context.Background(), d, "user:bob", true, "confirmed on the dashboard"); err != nil {
		t.Fatal(err)
	}
	got, _ := h.e.Deployment("a1")
	if got.State != model.StateRolledBack || got.ApprovedBy != "user:bob" || got.PendingRollback != nil {
		t.Fatalf("after approval: %s by %q pending=%v: %s", got.State, got.ApprovedBy, got.PendingRollback, got.Reason)
	}
	if !strings.Contains(h.host.joined(), "ln -sfn '/opt/app/releases/v1'") || len(h.lb.down()) != 0 {
		t.Fatalf("rollback did not run or left the target drained: %v", h.lb.down())
	}
	if err := h.e.DecideRollback(context.Background(), d, "user:bob", true, ""); !errors.Is(err, ErrNoPendingRollback) {
		t.Fatalf("second decision: %v", err)
	}
}

func TestApproveModeRejectRestoresTraffic(t *testing.T) {
	h := newHarness(t, opts{escalation: approveMode})
	d := failCanary(t, h, "r1")
	if err := h.e.DecideRollback(context.Background(), d, "user:carol", false, "false positive: upstream blip"); err != nil {
		t.Fatal(err)
	}
	got, _ := h.e.Deployment("r1")
	if got.State != model.StateHeld || !strings.Contains(got.Reason, "rejected by user:carol") || !strings.Contains(got.Reason, "re-enabled [app-1]") {
		t.Fatalf("after reject: %s: %s", got.State, got.Reason)
	}
	if len(h.lb.down()) != 0 || strings.Contains(h.host.joined(), "ln -sfn") {
		t.Fatalf("reject must restore traffic and not roll back: %v", h.lb.down())
	}
}

func TestApprovalTimeouts(t *testing.T) {
	// hold: still waiting, operators alerted once
	h := newHarness(t, opts{escalation: approveMode})
	d := failCanary(t, h, "t1")
	later := time.Now().Add(11 * time.Minute)
	h.e.ExpireApprovals(context.Background(), later)
	h.e.ExpireApprovals(context.Background(), later.Add(time.Minute))
	got, _ := h.e.Deployment("t1")
	pr, _ := h.e.PendingRollback("t1")
	if got.State != model.StateAwaitApproval || pr == nil || !pr.Escalated {
		t.Fatalf("hold on timeout: %s %+v", got.State, pr)
	}
	if n := strings.Count(eventsText(got), "still waiting"); n != 1 {
		t.Fatalf("escalation must be announced once, got %d", n)
	}
	// It can still be approved after the timeout.
	if err := h.e.DecideRollback(context.Background(), d, "user:dave", true, ""); err != nil {
		t.Fatal(err)
	}

	// rollback: automatic after the timeout
	h2 := newHarness(t, opts{escalation: "      mode: approve\n      approval: {timeout: 1m, on_timeout: rollback}"})
	failCanary(t, h2, "t2")
	h2.e.ExpireApprovals(context.Background(), time.Now().Add(2*time.Minute))
	got, _ = h2.e.Deployment("t2")
	if got.State != model.StateRolledBack || got.ApprovedBy != "" || !strings.Contains(got.Reason, "restored") {
		t.Fatalf("on_timeout rollback: %s: %s", got.State, got.Reason)
	}
	if !strings.Contains(eventsText(got), "rolling back automatically") {
		t.Fatalf("events: %s", eventsText(got))
	}
}

func TestPendingApprovalSurvivesRestart(t *testing.T) {
	h := newHarness(t, opts{escalation: approveMode})
	failCanary(t, h, "p1")
	h.e.Close()

	h2 := newHarness(t, opts{escalation: approveMode, journal: h.journal, app1: h.app1, app2: h.app2})
	pr, ok := h2.e.PendingRollback("p1")
	if !ok || len(pr.Drained) != 1 {
		t.Fatalf("pending rollback lost on restart: %+v", pr)
	}
	if err := h2.e.DecideRollback(context.Background(), h2.e.Live("p1"), "user:erin", true, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := h2.e.Deployment("p1"); got.State != model.StateRolledBack {
		t.Fatalf("after restart and approval: %s: %s", got.State, got.Reason)
	}
}

func eventsText(d *model.Deployment) string {
	var b strings.Builder
	for _, ev := range d.Events {
		b.WriteString(ev.Message + "\n")
	}
	return b.String()
}

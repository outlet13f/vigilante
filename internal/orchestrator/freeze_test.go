package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vigilante/internal/model"
)

// freezeNow adds an always-active config freeze to the harness.
func freezeNow(h *harness, allowRollback bool) {
	h.e.Cfg.ChangeFreeze = nil
	f := model.Freeze{Name: "incident-42", Reason: "DB migration", StartsAt: time.Now().Add(-time.Minute), EndsAt: time.Now().Add(time.Hour),
		AllowRollback: allowRollback, Services: []string{}, Teams: []string{}}
	h.e.CreateFreeze(&f)
}

func TestFreezeBlocksNewPhasesUnlessOverridden(t *testing.T) {
	h := newHarness(t, opts{})
	freezeNow(h, true)
	d := h.deploy("f1")
	err := h.e.Watch(context.Background(), d, model.PhaseCanary)
	if !errors.Is(err, ErrBlocked) || !errors.Is(err, ErrFrozen) || !strings.Contains(err.Error(), "incident-42") || !strings.Contains(err.Error(), "DB migration") {
		t.Fatalf("watch during a freeze: %v", err)
	}
	if f := h.e.ActiveFreeze("order", time.Now()); f == nil || f.Source != "api" || !f.AllowRollback {
		t.Fatalf("active freeze %+v", f)
	}
	h.e.SetFreezeOverride(d, "user:admin", "hotfix for the outage")
	if err := h.e.Watch(context.Background(), d, model.PhaseCanary); err != nil {
		t.Fatalf("override must let the phase start: %v", err)
	}
	if got, _ := h.e.Deployment("f1"); !strings.Contains(got.FreezeOverride, "hotfix") || !strings.Contains(got.FreezeOverride, "user:admin") {
		t.Fatalf("override not recorded: %q", got.FreezeOverride)
	}
}

func TestFreezeRollbackPolicy(t *testing.T) {
	// allow_rollback (default): a failing canary still rolls back.
	h := newHarness(t, opts{})
	d := h.deploy("r1")
	h.e.SetFreezeOverride(d, "user:admin", "emergency fix")
	freezeNow(h, true)
	h.app1.healthy.Store(false)
	_ = h.e.Watch(context.Background(), d, model.PhaseCanary)
	if got, _ := h.e.Deployment("r1"); got.State != model.StateRolledBack {
		t.Fatalf("allowed rollback: %s %s", got.State, got.Reason)
	}

	// allow_rollback: false: automatic rollback is refused and the failing
	// target isolated; manual rollback still works.
	h2 := newHarness(t, opts{})
	d2 := h2.deploy("r2")
	h2.e.SetFreezeOverride(d2, "user:admin", "emergency fix")
	freezeNow(h2, false)
	h2.app1.healthy.Store(false)
	_ = h2.e.Watch(context.Background(), d2, model.PhaseCanary)
	got, _ := h2.e.Deployment("r2")
	if got.State != model.StateRollbackFailed || !strings.Contains(got.Reason, "forbids automatic rollback") || len(h2.lb.down()) != 1 {
		t.Fatalf("frozen rollback: %s %s down=%v", got.State, got.Reason, h2.lb.down())
	}
	if err := h2.e.Rollback(context.Background(), d2, RollbackOptions{Reason: "operator decision", Manual: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h2.e.Deployment("r2"); got.State != model.StateRolledBack {
		t.Fatalf("manual rollback during a freeze: %s", got.State)
	}
}

func TestDeclaredFreezesPersistAndEnd(t *testing.T) {
	h := newHarness(t, opts{})
	freezeNow(h, true)
	ws := h.e.Freezes(time.Now())
	if len(ws) != 1 || !ws[0].Active || ws[0].Source != "api" {
		t.Fatalf("freezes %+v", ws)
	}
	id := ws[0].ID
	h.e.Close()

	h2 := newHarness(t, opts{journal: h.journal, app1: h.app1, app2: h.app2})
	if f := h2.e.ActiveFreeze("order", time.Now()); f == nil || f.ID != id {
		t.Fatalf("freeze lost on restart: %+v", f)
	}
	if _, err := h2.e.EndFreeze(id, "user:admin"); err != nil {
		t.Fatal(err)
	}
	if f := h2.e.ActiveFreeze("order", time.Now()); f != nil {
		t.Fatalf("ended freeze still active: %+v", f)
	}
	if err := h2.e.FreezeGate("order", ""); err != nil {
		t.Fatalf("gate after end: %v", err)
	}
	if _, err := h2.e.EndFreeze("config:nope", "user:admin"); err == nil {
		t.Fatal("config windows cannot be ended through the API")
	}
}

package pilot

import (
	"strings"
	"testing"
	"time"

	"vigilante/internal/model"
)

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// dep builds a deployment from "STATE: reason" / "verdict FAIL: ..." steps,
// one second apart unless a step starts with "+Ns ".
func dep(id, service string, dry bool, fb *model.Feedback, steps ...string) *model.Deployment {
	d := &model.Deployment{ID: id, Service: service, CreatedAt: t0, DryRun: dry, Feedback: fb}
	at := t0
	for _, s := range steps {
		gap := time.Second
		if strings.HasPrefix(s, "+") {
			n, rest, _ := strings.Cut(s[1:], " ")
			gap, _ = time.ParseDuration(n)
			s = rest
		}
		at = at.Add(gap)
		if msg, ok := strings.CutPrefix(s, "verdict "); ok {
			d.Events = append(d.Events, model.Event{Time: at, Kind: "verdict", Message: msg})
		} else {
			d.Events = append(d.Events, model.Event{Time: at, Kind: "state", Message: s})
		}
	}
	return d
}

func TestComputeVerdictsTimingAndGate(t *testing.T) {
	ok := &model.Feedback{Outcome: model.FeedbackCorrect}
	deps := []*model.Deployment{
		// Passed all three phases.
		dep("p1", "web", false, nil, "OBSERVING: phase canary", "+60s PROMOTED: no breaches", "OBSERVING: phase rolling", "+60s PROMOTED: ok", "OBSERVING: phase full", "+60s SUCCEEDED: ok"),
		// Failed in canary after 40s, rolled back in 20s; assessed correct.
		dep("f1", "web", false, ok, "OBSERVING: phase canary", "+40s verdict FAIL: 2 rule breach(es)", "ROLLING_BACK: rolling back", "+20s ROLLED_BACK: 1 target(s) restored"),
		// Dry run: would have rolled back; nobody assessed it yet.
		dep("d1", "api", true, nil, "OBSERVING: phase canary", "+30s verdict FAIL: breach", "ROLLING_BACK: rolling back", "+1s ROLLED_BACK: 1 target(s) restored"),
		// Held: control group failing too (environmental); assessed correct.
		dep("h1", "api", false, ok, "OBSERVING: phase canary", "+60s HELD: unresolved hold condition at end of window: cpu — also firing on control target c1 (environmental)"),
		// Approve mode: failed, approval took 5 minutes, rollback failed.
		dep("a1", "api", false, nil, "OBSERVING: phase canary", "+50s verdict FAIL: breach", "AWAITING_APPROVAL: waiting", "+300s ROLLING_BACK: approved", "+10s ROLLBACK_FAILED: exec failed"),
		// A false positive that was rejected in approve mode.
		dep("fp", "web", false, &model.Feedback{Outcome: model.FeedbackFalsePositive, Note: "probe port firewalled"},
			"OBSERVING: phase canary", "+20s verdict FAIL: breach", "AWAITING_APPROVAL: waiting", "+60s HELD: rollback rejected by user:bob"),
		// Registered only (mark-good): not part of the quality figures.
		dep("mg", "web", false, nil, "SUCCEEDED: marked good"),
		// Still observing.
		dep("o1", "web", false, nil, "OBSERVING: phase canary"),
	}
	r := Compute(deps, Options{})
	if r.Deployments != 7 || r.Unobserved != 1 || r.Observations != 9 {
		t.Fatalf("counts: deployments %d unobserved %d observations %d", r.Deployments, r.Unobserved, r.Observations)
	}
	want := Verdicts{Pass: 3, Fail: 4, Hold: 1, InProgress: 1}
	if r.Verdicts != want {
		t.Fatalf("verdicts %+v, want %+v", r.Verdicts, want)
	}
	if r.HoldCauses["environmental"] != 1 || len(r.HoldCauses) != 1 {
		t.Fatalf("hold causes %v (a rejected approval is not a held verdict)", r.HoldCauses)
	}
	rb := r.Rollbacks
	if rb.Completed != 1 || rb.DryRun != 1 || rb.Failed != 1 || rb.ApprovedByOps != 1 || rb.Rejected != 1 || rb.Manual != 0 {
		t.Fatalf("rollbacks %+v", rb)
	}
	if r.TimeToFail.Count != 4 || r.TimeToFail.Max != 50 || r.RollbackTime.Count != 3 || r.ApprovalWait.Max != 300 {
		t.Fatalf("timing ttf %+v rb %+v aw %+v", r.TimeToFail, r.RollbackTime, r.ApprovalWait)
	}
	if r.Feedback.FalsePositive != 1 || r.Feedback.Correct != 2 {
		t.Fatalf("feedback %+v", r.Feedback)
	}
	review := map[string]bool{}
	for _, it := range r.NeedsReview {
		review[it.ID] = true
	}
	if !review["d1"] || !review["a1"] || len(review) != 2 {
		t.Fatalf("needs review %v", r.NeedsReview)
	}
	if r.GatePassed {
		t.Fatal("7 deployments with a false positive cannot pass the gate")
	}
	md := r.Markdown()
	for _, s := range []string{"Release gate: **NOT MET**", "false positive: probe port firewalled", "rollback failed: exec failed", "| web |", "environmental 1"} {
		if !strings.Contains(md, s) {
			t.Errorf("markdown lacks %q:\n%s", s, md)
		}
	}
}

func TestGatePasses(t *testing.T) {
	var deps []*model.Deployment
	for i := 0; i < 30; i++ {
		deps = append(deps, dep("p"+string(rune('a'+i)), "web", false, nil, "OBSERVING: phase canary", "+60s SUCCEEDED: ok"))
	}
	deps = append(deps, dep("f", "web", false, &model.Feedback{Outcome: model.FeedbackCorrect, Incident: "INC1"},
		"OBSERVING: phase canary", "+40s verdict FAIL: breach", "ROLLING_BACK: x", "+20s ROLLED_BACK: restored"))
	r := Compute(deps, Options{})
	if !r.GatePassed {
		t.Fatalf("gate should pass: %+v", r.Gate)
	}
	// Lab scenario runs are not production verdicts.
	labRun := dep("lab-1", "web", false, nil, "OBSERVING: phase canary", "+5s verdict FAIL: injected", "ROLLING_BACK: x", "+5s ROLLED_BACK: restored")
	labRun.CreatedBy = "lab:F5 VE 17.1"
	if r := Compute(append(deps, labRun), Options{}); r.Deployments != 31 || len(r.NeedsReview) != 0 {
		t.Fatalf("lab run counted: %d deployments, review %v", r.Deployments, r.NeedsReview)
	}
	// Filters: service and period.
	if r := Compute(deps, Options{Services: []string{"api"}}); r.Deployments != 0 {
		t.Fatalf("service filter: %d", r.Deployments)
	}
	if r := Compute(deps, Options{Since: t0.Add(time.Hour)}); r.Deployments != 0 {
		t.Fatalf("period filter: %d", r.Deployments)
	}
}

func TestHoldCause(t *testing.T) {
	for reason, want := range map[string]string{
		"observer disagreement: breached from [agent], healthy from [central]": "observer_disagreement",
		"insufficient evidence (3 samples, 0/4 rule evaluations known)":        "insufficient_evidence",
		"unresolved hold condition at end of window: disk":                     "hold_rule",
		"circuit open": "other",
	} {
		if got := HoldCause(reason); got != want {
			t.Errorf("%q: %s, want %s", reason, got, want)
		}
	}
}

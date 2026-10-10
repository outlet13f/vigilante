// Package pilot measures decision quality from the state store: what the
// engine decided in each observed phase, how long detection and rollback
// took, and how people assessed the verdicts (deployment feedback). It is
// the M8 pilot report and the evidence for the release gate.
package pilot

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"vigilante/internal/model"
)

// Gate is the release gate on decision quality (docs/05-roadmap.md, M8-2).
type Gate struct {
	MinDeployments    int     `json:"min_deployments"`
	MaxFalsePositives int     `json:"max_false_positives"`
	MaxFalseNegatives int     `json:"max_false_negatives"`
	MaxHoldRate       float64 `json:"max_hold_rate"`
	MinRealRollbacks  int     `json:"min_real_rollbacks"`
}

// DefaultGate: 30 deployments or more, no false positive, no false
// negative, at most 10% of observed phases held, one real rollback.
var DefaultGate = Gate{MinDeployments: 30, MaxFalsePositives: 0, MaxFalseNegatives: 0, MaxHoldRate: 0.10, MinRealRollbacks: 1}

type Options struct {
	Since, Until time.Time // zero = unbounded; a deployment counts by its creation time
	Services     []string  // empty = all
	Gate         Gate
}

// Stats summarises durations.
type Stats struct {
	Count  int     `json:"count"`
	Median float64 `json:"median_seconds"`
	P90    float64 `json:"p90_seconds"`
	Max    float64 `json:"max_seconds"`
}

func stats(ds []time.Duration) Stats {
	if len(ds) == 0 {
		return Stats{}
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	at := func(q float64) float64 {
		i := int(q*float64(len(ds)-1) + 0.5)
		return ds[i].Seconds()
	}
	return Stats{Count: len(ds), Median: at(0.5), P90: at(0.9), Max: ds[len(ds)-1].Seconds()}
}

type Verdicts struct {
	Pass       int `json:"pass"`
	Fail       int `json:"fail"`
	Hold       int `json:"hold"`
	Aborted    int `json:"aborted"`
	InProgress int `json:"in_progress"`
}

type Feedback struct {
	Assessed      int `json:"assessed"`
	Correct       int `json:"correct"`
	FalsePositive int `json:"false_positive"`
	FalseNegative int `json:"false_negative"`
	Unclear       int `json:"unclear"`
}

type Rollbacks struct {
	Completed     int `json:"completed"` // real rollbacks that restored the previous version
	Failed        int `json:"failed"`    // ROLLBACK_FAILED
	DryRun        int `json:"dry_run"`   // would have rolled back (dry_run)
	Manual        int `json:"manual"`    // started by a person, not a verdict
	ApprovedByOps int `json:"approved"`  // approve mode: approved
	Rejected      int `json:"rejected"`  // approve mode: rejected (kept the new version)
	PendingNow    int `json:"awaiting_approval"`
}

type GateCheck struct {
	Name   string `json:"name"`
	Target string `json:"target"`
	Actual string `json:"actual"`
	OK     bool   `json:"ok"`
}

// Item names a deployment that needs attention in the report.
type Item struct {
	ID      string `json:"id"`
	Service string `json:"service"`
	What    string `json:"what"`
}

type Report struct {
	From, To     time.Time            `json:"-"`
	Period       string               `json:"period"`
	Deployments  int                  `json:"deployments"`  // observed at least once
	Unobserved   int                  `json:"unobserved"`   // registered, never observed (mark-good, CI never started a phase)
	Observations int                  `json:"observations"` // observed phases
	Verdicts     Verdicts             `json:"verdicts"`
	HoldRate     float64              `json:"hold_rate"`
	HoldCauses   map[string]int       `json:"hold_causes"`
	Feedback     Feedback             `json:"feedback"`
	Rollbacks    Rollbacks            `json:"rollbacks"`
	TimeToFail   Stats                `json:"time_to_fail"`      // phase observation start -> FAIL verdict
	RollbackTime Stats                `json:"rollback_duration"` // rollback start -> restored or failed
	ApprovalWait Stats                `json:"approval_wait"`     // AWAITING_APPROVAL -> decision
	PerService   map[string]*Verdicts `json:"per_service"`
	NeedsReview  []Item               `json:"needs_review"` // failed or held deployments nobody assessed
	Findings     []Item               `json:"findings"`     // false positives, false negatives, failed rollbacks
	Gate         []GateCheck          `json:"gate"`
	GatePassed   bool                 `json:"gate_passed"`
}

// HoldCause classifies the reason of a HELD verdict.
func HoldCause(reason string) string {
	r := strings.ToLower(reason)
	switch {
	case strings.Contains(r, "observer disagreement"):
		return "observer_disagreement" // quorum: the probes disagree
	case strings.Contains(r, "environmental"):
		return "environmental" // also failing on the control group
	case strings.Contains(r, "insufficient evidence"):
		return "insufficient_evidence"
	case strings.Contains(r, "unresolved hold condition"):
		return "hold_rule"
	}
	return "other"
}

type step struct {
	at     time.Time
	state  model.State
	reason string
}

func timeline(d *model.Deployment) (steps []step, fails []time.Time) {
	for _, ev := range d.Events {
		switch ev.Kind {
		case "state":
			st, reason, _ := strings.Cut(ev.Message, ": ")
			steps = append(steps, step{ev.Time, model.State(st), reason})
		case "verdict":
			if strings.HasPrefix(ev.Message, "FAIL:") {
				fails = append(fails, ev.Time)
			}
		}
	}
	return steps, fails
}

// Compute builds the report from the latest snapshot of each deployment.
func Compute(deps []*model.Deployment, opt Options) *Report {
	if opt.Gate == (Gate{}) {
		opt.Gate = DefaultGate
	}
	r := &Report{HoldCauses: map[string]int{}, PerService: map[string]*Verdicts{}, From: opt.Since, To: opt.Until}
	keep := func(d *model.Deployment) bool {
		if len(opt.Services) > 0 && !contains(opt.Services, d.Service) {
			return false
		}
		if !opt.Since.IsZero() && d.CreatedAt.Before(opt.Since) {
			return false
		}
		return opt.Until.IsZero() || d.CreatedAt.Before(opt.Until)
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].CreatedAt.Before(deps[j].CreatedAt) })
	var ttf, rbt, aw []time.Duration
	for _, d := range deps {
		if !keep(d) {
			continue
		}
		steps, fails := timeline(d)
		observed := false
		failed, held := false, false
		for i, s := range steps {
			next := func() *step {
				if i+1 < len(steps) {
					return &steps[i+1]
				}
				return nil
			}()
			switch s.state {
			case model.StateObserving:
				observed = true
				r.Observations++
				pv := r.PerService[d.Service]
				if pv == nil {
					pv = &Verdicts{}
					r.PerService[d.Service] = pv
				}
				end := time.Time{}
				if next != nil {
					end = next.at
				}
				var failAt time.Time
				for _, f := range fails {
					if !f.Before(s.at) && (end.IsZero() || !f.After(end)) {
						failAt = f
						break
					}
				}
				switch {
				case !failAt.IsZero():
					r.Verdicts.Fail++
					pv.Fail++
					failed = true
					ttf = append(ttf, failAt.Sub(s.at))
				case next == nil:
					r.Verdicts.InProgress++
					pv.InProgress++
				case next.state == model.StatePromoted || next.state == model.StateSucceeded:
					r.Verdicts.Pass++
					pv.Pass++
				case next.state == model.StateHeld:
					r.Verdicts.Hold++
					pv.Hold++
					held = true
					r.HoldCauses[HoldCause(next.reason)]++
				case next.state == model.StateAborted:
					r.Verdicts.Aborted++
					pv.Aborted++
				}
			case model.StateRollingBack:
				if i == 0 || steps[i-1].state != model.StateObserving && steps[i-1].state != model.StateAwaitApproval {
					r.Rollbacks.Manual++
				}
				if next != nil && (next.state == model.StateRolledBack || next.state == model.StateRollbackFailed) {
					rbt = append(rbt, next.at.Sub(s.at))
				}
			case model.StateRolledBack:
				if d.DryRun {
					r.Rollbacks.DryRun++
				} else {
					r.Rollbacks.Completed++
				}
			case model.StateRollbackFailed:
				r.Rollbacks.Failed++
				r.Findings = append(r.Findings, Item{d.ID, d.Service, "rollback failed: " + trim(s.reason)})
			case model.StateAwaitApproval:
				if next == nil {
					r.Rollbacks.PendingNow++
					break
				}
				aw = append(aw, next.at.Sub(s.at))
				if next.state == model.StateRollingBack {
					r.Rollbacks.ApprovedByOps++
				} else if next.state == model.StateHeld && strings.HasPrefix(next.reason, "rollback rejected") {
					r.Rollbacks.Rejected++
				}
			}
		}
		if !observed {
			r.Unobserved++
			continue
		}
		r.Deployments++
		if fb := d.Feedback; fb != nil {
			r.Feedback.Assessed++
			switch fb.Outcome {
			case model.FeedbackCorrect:
				r.Feedback.Correct++
			case model.FeedbackFalsePositive:
				r.Feedback.FalsePositive++
				r.Findings = append(r.Findings, Item{d.ID, d.Service, "false positive: " + trim(fb.Note)})
			case model.FeedbackFalseNegative:
				r.Feedback.FalseNegative++
				r.Findings = append(r.Findings, Item{d.ID, d.Service, "false negative (" + fb.Incident + "): " + trim(fb.Note)})
			case model.FeedbackUnclear:
				r.Feedback.Unclear++
			}
		} else if failed || held {
			what := "held"
			if failed {
				what = "failed"
			}
			r.NeedsReview = append(r.NeedsReview, Item{d.ID, d.Service, what + ": was the verdict right? (vigilante feedback)"})
		}
	}
	if r.Observations > 0 {
		r.HoldRate = float64(r.Verdicts.Hold) / float64(r.Observations)
	}
	r.TimeToFail, r.RollbackTime, r.ApprovalWait = stats(ttf), stats(rbt), stats(aw)
	r.Period = period(opt.Since, opt.Until)
	r.gate(opt.Gate)
	return r
}

func (r *Report) gate(g Gate) {
	add := func(name, target, actual string, ok bool) {
		r.Gate = append(r.Gate, GateCheck{name, target, actual, ok})
	}
	add("observed deployments", fmt.Sprintf(">= %d", g.MinDeployments), fmt.Sprint(r.Deployments), r.Deployments >= g.MinDeployments)
	add("false positives", fmt.Sprintf("<= %d", g.MaxFalsePositives), fmt.Sprint(r.Feedback.FalsePositive), r.Feedback.FalsePositive <= g.MaxFalsePositives)
	add("false negatives", fmt.Sprintf("<= %d", g.MaxFalseNegatives), fmt.Sprint(r.Feedback.FalseNegative), r.Feedback.FalseNegative <= g.MaxFalseNegatives)
	add("hold rate (held / observed phases)", fmt.Sprintf("<= %.0f%%", g.MaxHoldRate*100), fmt.Sprintf("%.1f%%", r.HoldRate*100), r.HoldRate <= g.MaxHoldRate)
	add("real rollbacks completed", fmt.Sprintf(">= %d", g.MinRealRollbacks), fmt.Sprint(r.Rollbacks.Completed), r.Rollbacks.Completed >= g.MinRealRollbacks)
	add("failed or held deployments assessed", "all", fmt.Sprintf("%d unassessed", len(r.NeedsReview)), len(r.NeedsReview) == 0)
	r.GatePassed = true
	for _, c := range r.Gate {
		r.GatePassed = r.GatePassed && c.OK
	}
}

func period(from, to time.Time) string {
	f, t := "beginning", "now"
	if !from.IsZero() {
		f = from.UTC().Format("2006-01-02")
	}
	if !to.IsZero() {
		t = to.UTC().Format("2006-01-02")
	}
	return f + " .. " + t
}

func trim(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Markdown renders the report for people (pilot status meetings, sales evidence).
func (r *Report) Markdown() string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	p("# Decision quality report (%s)", r.Period)
	p("")
	verdict := "NOT MET"
	if r.GatePassed {
		verdict = "MET"
	}
	p("Release gate: **%s**", verdict)
	p("")
	p("| Check | Target | Actual | |")
	p("|---|---|---|---|")
	for _, c := range r.Gate {
		mark := "no"
		if c.OK {
			mark = "yes"
		}
		p("| %s | %s | %s | %s |", c.Name, c.Target, c.Actual, mark)
	}
	p("")
	p("## Verdicts")
	p("")
	p("%d deployments observed in %d phases (%d registered but never observed).", r.Deployments, r.Observations, r.Unobserved)
	p("")
	p("| | Pass | Fail | Hold | Aborted | In progress |")
	p("|---|---|---|---|---|---|")
	p("| all | %d | %d | %d | %d | %d |", r.Verdicts.Pass, r.Verdicts.Fail, r.Verdicts.Hold, r.Verdicts.Aborted, r.Verdicts.InProgress)
	svcs := make([]string, 0, len(r.PerService))
	for s := range r.PerService {
		svcs = append(svcs, s)
	}
	sort.Strings(svcs)
	for _, s := range svcs {
		v := r.PerService[s]
		p("| %s | %d | %d | %d | %d | %d |", s, v.Pass, v.Fail, v.Hold, v.Aborted, v.InProgress)
	}
	if len(r.HoldCauses) > 0 {
		p("")
		causes := make([]string, 0, len(r.HoldCauses))
		for c, n := range r.HoldCauses {
			causes = append(causes, fmt.Sprintf("%s %d", c, n))
		}
		sort.Strings(causes)
		p("Hold causes: %s.", strings.Join(causes, ", "))
	}
	p("")
	p("## Assessment")
	p("")
	f := r.Feedback
	p("Assessed %d of %d: correct %d, false positive %d, false negative %d, unclear %d.", f.Assessed, r.Deployments, f.Correct, f.FalsePositive, f.FalseNegative, f.Unclear)
	p("")
	p("## Rollbacks and timing")
	p("")
	rb := r.Rollbacks
	p("Completed %d, failed %d, dry run (would have rolled back) %d, manual %d; approve mode: approved %d, rejected %d, waiting %d.",
		rb.Completed, rb.Failed, rb.DryRun, rb.Manual, rb.ApprovedByOps, rb.Rejected, rb.PendingNow)
	p("")
	p("| Duration (s) | Count | Median | P90 | Max |")
	p("|---|---|---|---|---|")
	for _, s := range []struct {
		name string
		st   Stats
	}{{"observation start -> FAIL", r.TimeToFail}, {"rollback start -> done", r.RollbackTime}, {"approval wait", r.ApprovalWait}} {
		p("| %s | %d | %.0f | %.0f | %.0f |", s.name, s.st.Count, s.st.Median, s.st.P90, s.st.Max)
	}
	for _, sec := range []struct {
		title string
		items []Item
	}{{"Findings", r.Findings}, {"Needs assessment", r.NeedsReview}} {
		if len(sec.items) == 0 {
			continue
		}
		p("")
		p("## %s", sec.title)
		p("")
		for _, it := range sec.items {
			p("- `%s` (%s): %s", it.ID, it.Service, it.What)
		}
	}
	return b.String()
}

// Package decision turns rule evaluations into a phase verdict. It owns the
// observation window, the warm-up grace period, the minimum-evidence check and
// the context checks that stop a rollback for problems the deployment did not
// cause (control group breaching too, or vantage points disagreeing).
package decision

import (
	"context"
	"fmt"
	"slices"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/metrics"
	"vigilante/internal/model"
	"vigilante/internal/rules"
	"vigilante/internal/telemetry"
)

// Phase is the fully resolved input of one observation phase.
type Phase struct {
	Name     model.Phase
	Cfg      config.PhaseConfig
	Rules    []config.Rule
	Deployed []string // targets running the new version
	Controls []string // untouched targets (optional)
	Quorum   bool     // require agreement between vantage points
}

// Evaluation is one tick's result.
type Evaluation struct {
	Time    time.Time
	Fail    []model.Breach // rollback-worthy, attributable to the deployment
	Hold    []model.Breach // environmental / observer disagreement / action: hold
	Warn    []model.Breach // action: notify
	Known   int            // rule x target evaluations with a definite answer
	Unknown int
}

// Outcome is the phase verdict.
type Outcome struct {
	Verdict   model.Verdict
	Reason    string
	Breaches  []model.Breach
	Warnings  []model.Breach
	Evals     int
	StartedAt time.Time
	EndedAt   time.Time
}

type Engine struct {
	Phase Phase
	Store *metrics.Store
	Eval  *rules.Evaluator
	// OnEval is called after each tick (status reporting, logging).
	OnEval func(Evaluation)
	// Now is injectable for tests.
	Now func() time.Time
}

func New(p Phase, store *metrics.Store, baseline rules.Baseline) *Engine {
	return &Engine{Phase: p, Store: store, Eval: rules.NewEvaluator(store, baseline), Now: time.Now}
}

func (e *Engine) activeRules() []config.Rule {
	if len(e.Phase.Cfg.Rules) == 0 {
		return e.Phase.Rules
	}
	var out []config.Rule
	for _, r := range e.Phase.Rules {
		if slices.Contains(e.Phase.Cfg.Rules, r.Name) {
			out = append(out, r)
		}
	}
	return out
}

// Evaluate runs one tick over every (active rule x deployed target), plus the
// control targets so their counters advance identically.
func (e *Engine) Evaluate(now time.Time) Evaluation {
	ev := Evaluation{Time: now}
	for _, rule := range e.activeRules() {
		// Control group first: a rule that fires on targets that never received
		// the new version is an environmental problem (DB down, upstream outage,
		// network), and rolling back would not help.
		controlFired := ""
		for _, ct := range e.Phase.Controls {
			r := e.Eval.Eval(rule, rules.Scope{Target: ct, ServiceTargets: e.Phase.Controls}, now)
			if r.State == rules.True && controlFired == "" {
				controlFired = ct
			}
		}
		for _, t := range e.Phase.Deployed {
			res, disagreement := e.evalTarget(rule, t, now)
			switch res.State {
			case rules.Unknown:
				ev.Unknown++
				continue
			case rules.False:
				ev.Known++
				if disagreement != "" {
					ev.Hold = append(ev.Hold, model.Breach{Rule: rule.Name, Target: t, Action: "hold", Detail: disagreement})
				}
				continue
			}
			ev.Known++
			b := model.Breach{Rule: rule.Name, Target: t, Detail: res.Detail, Action: rule.Action, Value: res.Value}
			switch {
			case rule.Action == "notify":
				ev.Warn = append(ev.Warn, b)
			case controlFired != "":
				b.Environmental = true
				b.Action = "hold"
				b.Detail += fmt.Sprintf(" — also firing on control target %s (environmental)", controlFired)
				ev.Hold = append(ev.Hold, b)
			case rule.Action == "hold":
				ev.Hold = append(ev.Hold, b)
			default:
				ev.Fail = append(ev.Fail, b)
			}
		}
	}
	return ev
}

// evalTarget applies observer quorum: when both the central orchestrator and
// an on-host agent report the rule's metrics, a breach only counts if every
// vantage point sees it. One-sided failures mean the observer is blind
// (network partition, broken SSH), not that the release is bad.
func (e *Engine) evalTarget(rule config.Rule, target string, now time.Time) (rules.Result, string) {
	sc := rules.Scope{Target: target, ServiceTargets: e.Phase.Deployed}
	if !e.Phase.Quorum {
		return e.Eval.Eval(rule, sc, now), ""
	}
	srcSet := map[string]bool{}
	for _, m := range rules.Metrics(rule.When) {
		for _, s := range e.Store.Sources(target, m, maxWindow(rule.When), now) {
			srcSet[s] = true
		}
	}
	if len(srcSet) < 2 {
		return e.Eval.Eval(rule, sc, now), ""
	}
	var srcs []string
	for s := range srcSet {
		srcs = append(srcs, s)
	}
	slices.Sort(srcs)
	var trues, falses []string
	var first rules.Result
	for _, s := range srcs {
		sc.Source = s
		r := e.Eval.Eval(rule, sc, now)
		switch r.State {
		case rules.True:
			if len(trues) == 0 {
				first = r
			}
			trues = append(trues, s)
		case rules.False:
			falses = append(falses, s)
		}
	}
	switch {
	case len(trues) > 0 && len(falses) == 0:
		return first, ""
	case len(trues) > 0:
		return rules.Result{State: rules.False}, fmt.Sprintf("observer disagreement: breached from %v, healthy from %v", trues, falses)
	case len(falses) > 0:
		return rules.Result{State: rules.False}, ""
	}
	return rules.Result{State: rules.Unknown}, ""
}

func maxWindow(n config.Node) time.Duration {
	w := n.Window
	for _, c := range n.Any {
		w = max(w, maxWindow(c))
	}
	for _, c := range n.All {
		w = max(w, maxWindow(c))
	}
	if n.Not != nil {
		w = max(w, maxWindow(*n.Not))
	}
	return w
}

// Run observes the phase until a verdict is reached or ctx is cancelled.
//
//   - During warmup nothing is judged (JVM warm-up, cache fill, connection
//     pool ramp) but samples are still collected.
//   - Any attributable rollback breach ends the phase immediately with FAIL.
//   - At the end of the window: too little evidence => INCONCLUSIVE (resolved
//     by on_inconclusive); unresolved environmental holds => HOLD; else PASS.
func (e *Engine) Run(ctx context.Context) Outcome {
	start := e.Now()
	out := Outcome{StartedAt: start}
	warmupEnd := start.Add(e.Phase.Cfg.Warmup)
	end := start.Add(e.Phase.Cfg.ObservationWindow)
	tick := time.NewTicker(e.Phase.Cfg.EvalInterval)
	defer tick.Stop()
	var last Evaluation
	seenWarn := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			out.Verdict, out.Reason, out.EndedAt = model.VerdictHold, "observation aborted: "+ctx.Err().Error(), e.Now()
			return out
		case <-tick.C:
		}
		now := e.Now()
		if now.Before(warmupEnd) {
			continue
		}
		evalStart := time.Now()
		last = e.Evaluate(now)
		telemetry.Evaluation.Since(evalStart)
		out.Evals++
		for _, w := range last.Warn {
			if k := w.Rule + "|" + w.Target; !seenWarn[k] {
				seenWarn[k] = true
				out.Warnings = append(out.Warnings, w)
			}
		}
		if e.OnEval != nil {
			e.OnEval(last)
		}
		if len(last.Fail) > 0 {
			out.Verdict, out.Breaches, out.EndedAt = model.VerdictFail, last.Fail, now
			out.Reason = fmt.Sprintf("%d rule breach(es): %s", len(last.Fail), summarize(last.Fail))
			return out
		}
		if !now.Before(end) {
			out.EndedAt = now
			return e.conclude(out, last, start)
		}
	}
}

func (e *Engine) conclude(out Outcome, last Evaluation, start time.Time) Outcome {
	samples := e.Store.Count(e.Phase.Deployed, start)
	insufficient := samples < e.Phase.Cfg.MinSamples || (last.Known == 0 && last.Unknown > 0)
	switch {
	case len(last.Hold) > 0:
		out.Verdict, out.Breaches = model.VerdictHold, last.Hold
		out.Reason = "unresolved hold condition at end of window: " + summarize(last.Hold)
	case insufficient:
		out.Reason = fmt.Sprintf("insufficient evidence (%d samples, %d/%d rule evaluations known)", samples, last.Known, last.Known+last.Unknown)
		switch e.Phase.Cfg.OnInconclusive {
		case "pass":
			out.Verdict = model.VerdictPass
		case "rollback":
			out.Verdict = model.VerdictFail
		default:
			out.Verdict = model.VerdictInconclusive
		}
	default:
		out.Verdict = model.VerdictPass
		out.Reason = fmt.Sprintf("no breaches in %s window (%d evaluations, %d samples)", e.Phase.Cfg.ObservationWindow, out.Evals, samples)
	}
	return out
}

func summarize(bs []model.Breach) string {
	s := ""
	for i, b := range bs {
		if i == 3 {
			s += fmt.Sprintf("; +%d more", len(bs)-3)
			break
		}
		if i > 0 {
			s += "; "
		}
		s += fmt.Sprintf("[%s@%s] %s", b.Rule, b.Target, b.Detail)
	}
	return s
}

// Package rules evaluates the multi-metric rule trees from vigilante.yaml
// against the metrics store. Evaluation is tri-state (true / false / unknown)
// so that "no data" is never silently treated as "healthy" or "broken".
package rules

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/metrics"
)

type Tri int

const (
	Unknown Tri = iota
	False
	True
)

func (t Tri) String() string {
	return [...]string{"unknown", "false", "true"}[t]
}

// Result of evaluating a node for one target.
type Result struct {
	State  Tri
	Detail string // human readable, e.g. "health.latency_ms p99=412.0 > 300.0 (3/3)"
	Value  float64
}

// Baseline supplies reference values for baseline-relative conditions.
type Baseline interface {
	Lookup(c config.Condition, now time.Time) (float64, bool)
}

// Evaluator holds the consecutive-breach counters. One Evaluator must see
// exactly one evaluation per tick per (rule, target, source) for `for:` and
// `reset_after:` to mean "consecutive evaluations".
type Evaluator struct {
	Store    *metrics.Store
	Baseline Baseline

	mu       sync.Mutex
	counters map[string]*counter
}

type counter struct {
	breaches int // consecutive raw breaches (held until reset)
	oks      int // consecutive raw OKs
}

func NewEvaluator(store *metrics.Store, baseline Baseline) *Evaluator {
	return &Evaluator{Store: store, Baseline: baseline, counters: map[string]*counter{}}
}

// Scope tells the evaluator which target is being judged and which targets
// make up the "service" for scope: service conditions.
type Scope struct {
	Target         string
	ServiceTargets []string
	Source         string // "" = all vantage points
}

// Eval evaluates a rule for one target. Every leaf is evaluated (no
// short-circuit) so all counters advance in lock-step.
func (e *Evaluator) Eval(rule config.Rule, sc Scope, now time.Time) Result {
	return e.node(rule.Name, rule.When, sc, now)
}

func (e *Evaluator) node(path string, n config.Node, sc Scope, now time.Time) Result {
	switch {
	case len(n.Any) > 0:
		res := Result{State: False}
		var parts []string
		for i, c := range n.Any {
			r := e.node(fmt.Sprintf("%s/any%d", path, i), c, sc, now)
			if r.State == True {
				parts = append(parts, r.Detail)
				if res.State != True {
					res.Value = r.Value
				}
				res.State = True
			} else if r.State == Unknown && res.State == False {
				res.State = Unknown
			}
		}
		res.Detail = strings.Join(parts, " OR ")
		return res
	case len(n.All) > 0:
		res := Result{State: True}
		var parts []string
		for i, c := range n.All {
			r := e.node(fmt.Sprintf("%s/all%d", path, i), c, sc, now)
			switch r.State {
			case False:
				res.State = False
			case Unknown:
				if res.State == True {
					res.State = Unknown
				}
			case True:
				parts = append(parts, r.Detail)
				res.Value = r.Value
			}
		}
		if res.State == True {
			res.Detail = strings.Join(parts, " AND ")
		}
		return res
	case n.Not != nil:
		r := e.node(path+"/not", *n.Not, sc, now)
		switch r.State {
		case True:
			return Result{State: False}
		case False:
			return Result{State: True, Detail: "NOT(" + path + ")"}
		}
		return r
	}
	return e.leaf(path, n.Condition, sc, now)
}

// Value computes the current aggregated value of a condition's metric.
func (e *Evaluator) Value(c config.Condition, sc Scope, now time.Time) (float64, bool) {
	targets := []string{sc.Target}
	if c.Scope == "service" {
		targets = sc.ServiceTargets
	}
	return valueOver(e.Store, c, targets, sc.Source, now)
}

func valueOver(store *metrics.Store, c config.Condition, targets []string, source string, now time.Time) (float64, bool) {
	collect := func(metric string) []metrics.Point {
		var pts []metrics.Point
		for _, t := range targets {
			pts = append(pts, store.Window(t, metric, c.Window, now, source)...)
		}
		return pts
	}
	pts := collect(c.Metric)
	if c.RatioOf != "" {
		num, _ := metrics.Aggregate(pts, "sum", c.Window)
		den, _ := metrics.Aggregate(collect(c.RatioOf), "sum", c.Window)
		if den == 0 {
			return 0, false // no traffic: a ratio is undefined, not zero
		}
		return 100 * num / den, true
	}
	return metrics.Aggregate(pts, c.Agg, c.Window)
}

func (e *Evaluator) leaf(path string, c config.Condition, sc Scope, now time.Time) Result {
	key := path + "|" + sc.Target + "|" + sc.Source
	if c.Scope == "service" {
		key = path + "|*|" + sc.Source
	}
	label := c.Metric
	if c.RatioOf != "" {
		label = c.Metric + "/" + c.RatioOf + "%"
	} else if c.Agg != "last" {
		label += " " + c.Agg
	}

	v, ok := e.Value(c, sc, now)
	var raw bool
	var threshold float64
	if !ok {
		switch c.Absent {
		case "breach":
			raw = true
		case "ok":
			raw = false
		default:
			return Result{State: Unknown, Detail: label + " no data"}
		}
	} else if c.Value != nil {
		threshold = *c.Value
		raw = compare(v, c.Op, threshold)
	} else {
		var b float64
		found := false
		if e.Baseline != nil {
			b, found = e.Baseline.Lookup(c, now)
		}
		if !found {
			return Result{State: Unknown, Detail: label + " no baseline"}
		}
		factor := c.Baseline.IncreasePct / 100
		if c.Op == "<" || c.Op == "<=" { // drop detection, e.g. throughput fell by 50%
			threshold = b * (1 - factor)
		} else {
			threshold = b * (1 + factor)
		}
		raw = compare(v, c.Op, threshold)
		if raw && c.MinValue != nil && (c.Op == ">" || c.Op == ">=") && v < *c.MinValue {
			raw = false // tiny absolute values never count, however large the relative jump
		}
	}

	e.mu.Lock()
	ctr, exists := e.counters[key]
	if !exists {
		ctr = &counter{}
		e.counters[key] = ctr
	}
	if raw {
		ctr.breaches++
		ctr.oks = 0
	} else {
		ctr.oks++
		if ctr.oks >= c.ResetAfter {
			ctr.breaches = 0
		}
	}
	streak := ctr.breaches
	e.mu.Unlock()

	if streak >= c.For && raw {
		detail := fmt.Sprintf("%s=%.2f %s %.2f", label, v, c.Op, threshold)
		if c.Baseline != nil {
			detail += fmt.Sprintf(" (baseline +%.0f%%)", c.Baseline.IncreasePct)
		}
		if !ok {
			detail = label + " absent"
		}
		if c.For > 1 {
			detail += fmt.Sprintf(" [%d consecutive]", streak)
		}
		return Result{State: True, Detail: detail, Value: v}
	}
	return Result{State: False, Value: v}
}

func compare(v float64, op string, t float64) bool {
	switch op {
	case ">":
		return v > t
	case ">=":
		return v >= t
	case "<":
		return v < t
	case "<=":
		return v <= t
	case "==":
		return v == t
	case "!=":
		return v != t
	}
	return false
}

// Metrics returns every metric referenced by a node (used for quorum checks).
func Metrics(n config.Node) []string {
	var out []string
	var walk func(n config.Node)
	walk = func(n config.Node) {
		for _, c := range n.Any {
			walk(c)
		}
		for _, c := range n.All {
			walk(c)
		}
		if n.Not != nil {
			walk(*n.Not)
		}
		if n.Metric != "" {
			out = append(out, n.Metric)
			if n.RatioOf != "" {
				out = append(out, n.RatioOf)
			}
		}
	}
	walk(n)
	return out
}

// BaselineConditions lists leaf conditions that compare against a baseline.
func BaselineConditions(rs []config.Rule) []config.Condition {
	var out []config.Condition
	var walk func(n config.Node)
	walk = func(n config.Node) {
		for _, c := range n.Any {
			walk(c)
		}
		for _, c := range n.All {
			walk(c)
		}
		if n.Not != nil {
			walk(*n.Not)
		}
		if n.Metric != "" && n.Baseline != nil {
			out = append(out, n.Condition)
		}
	}
	for _, r := range rs {
		walk(r.When)
	}
	return out
}

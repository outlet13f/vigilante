// Package lab runs the M8 lab scenario against real equipment and records
// the outcome for the compatibility matrix:
//
//	checkpoint (snapshot) -> bad deployment -> detection -> drain ->
//	restore -> back in traffic
//
// It drives the same engine as `vigilante watch`, so a passing run proves
// the production path, not a test double.
package lab

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/executor"
	"vigilante/internal/model"
	"vigilante/internal/orchestrator"
	"vigilante/internal/transport"
)

type Options struct {
	Service string
	Phase   model.Phase
	// Version and Previous name the bad and the good release; they matter to
	// executors that switch releases (symlink, container), not to snapshots.
	Version, Previous string
	// Inject deploys the bad version (any shell command); Reset, run after
	// every run, returns the environment to the good version if the
	// rollback did not.
	Inject, Reset string
	// Label names the equipment under test, e.g. "F5 BIG-IP VE 17.1".
	Label string
	// Baseline captures a baseline first for this long (0: none).
	Baseline time.Duration
	// Shell runs Inject and Reset (default: sh -c, or cmd /C on Windows).
	Shell func(ctx context.Context, cmd string) (string, error)
	Now   func() time.Time
}

type Step struct {
	Name    string  `json:"name"`
	OK      bool    `json:"ok"`
	Seconds float64 `json:"seconds"`
	Detail  string  `json:"detail,omitempty"`
}

type Result struct {
	Label        string    `json:"label"`
	Service      string    `json:"service"`
	Plugins      []string  `json:"plugins"`
	Run          int       `json:"run"`
	Started      time.Time `json:"started"`
	DeploymentID string    `json:"deployment_id"`
	Steps        []Step    `json:"steps"`
	FinalState   string    `json:"final_state"`
	// TimeToFail: observation start to the FAIL verdict. Rollback: rollback
	// start to the final state.
	TimeToFail      float64               `json:"time_to_fail_seconds"`
	RollbackSeconds float64               `json:"rollback_seconds"`
	PoolBefore      []executor.PoolMember `json:"pool_before,omitempty"`
	PoolAfter       []executor.PoolMember `json:"pool_after,omitempty"`
	Passed          bool                  `json:"passed"`
}

// defaultShell runs a command on this machine the way local executors do.
func defaultShell(ctx context.Context, cmd string) (string, error) {
	out, err := (&transport.Local{}).Run(ctx, cmd, nil)
	return strings.TrimSpace(out), err
}

// Plugins lists what a service exercises, e.g. "executor:openstack".
func Plugins(cfg *config.Config, svc *config.Service) []string {
	seen := map[string]bool{}
	if ex, ok := cfg.Executors[svc.Rollback.Executor]; ok {
		seen["executor:"+ex.Type] = true
	}
	for _, esc := range svc.Rollback.Escalation {
		if ex, ok := cfg.Executors[esc.Executor]; ok {
			seen["executor:"+ex.Type] = true
		}
	}
	if tr, ok := cfg.Traffic[svc.Rollback.Traffic]; ok {
		seen["traffic:"+tr.Type] = true
	}
	for _, p := range svc.Probes {
		seen["probe:"+p.Type] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Run executes the scenario once. It never panics on a failed step: the
// result says where it stopped.
func Run(ctx context.Context, e *orchestrator.Engine, opt Options, n int) (r Result) {
	if opt.Shell == nil {
		opt.Shell = defaultShell
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Phase == "" {
		opt.Phase = model.PhaseCanary
	}
	r = Result{Label: opt.Label, Service: opt.Service, Run: n, Started: opt.Now().UTC()}
	svc, ok := e.Cfg.Service(opt.Service)
	if !ok {
		r.Steps = append(r.Steps, Step{Name: "config", Detail: "unknown service " + opt.Service})
		return r
	}
	r.Plugins = Plugins(e.Cfg, svc)
	step := func(name string, fn func() (string, error)) bool {
		t := opt.Now()
		detail, err := fn()
		s := Step{Name: name, OK: err == nil, Seconds: opt.Now().Sub(t).Seconds(), Detail: detail}
		if err != nil {
			s.Detail = strings.TrimSpace(detail + " " + err.Error())
		}
		r.Steps = append(r.Steps, s)
		return err == nil
	}
	// Named result: the reset step is part of what Run returns.
	defer func() {
		if opt.Reset != "" {
			step("reset", func() (string, error) { return opt.Shell(context.WithoutCancel(ctx), opt.Reset) })
		}
	}()

	id := fmt.Sprintf("lab-%s-%s-%d", opt.Service, r.Started.Format("20060102T150405"), n)
	r.DeploymentID = id
	version := opt.Version
	if version == "" {
		version = "lab-bad"
	}
	var d *model.Deployment
	if !step("checkpoint", func() (string, error) {
		var err error
		if d, err = e.Create(id, opt.Service, version, opt.Previous); err != nil {
			return "", err
		}
		e.SetCreatedBy(d, "lab:"+opt.Label)
		if err := e.Prepare(ctx, d); err != nil {
			return "", err
		}
		cp, _ := e.Deployment(id)
		return fmt.Sprintf("%d target(s) checkpointed", len(cp.Checkpoints)), nil
	}) {
		return r
	}
	if opt.Baseline > 0 && !step("baseline", func() (string, error) {
		snap, err := e.CaptureBaseline(ctx, opt.Service, opt.Baseline)
		if err != nil {
			return "", err
		}
		e.SetBaseline(snap)
		return "", nil
	}) {
		return r
	}
	tc, err := e.Traffic(opt.Service)
	if err != nil {
		step("traffic", func() (string, error) { return "", err })
		return r
	}
	if tc != nil && !step("pool before", func() (string, error) {
		var err error
		r.PoolBefore, err = tc.Pool(ctx)
		return fmt.Sprintf("%d member(s), %d enabled", len(r.PoolBefore), enabled(r.PoolBefore)), err
	}) {
		return r
	}
	if !step("inject bad version", func() (string, error) { return opt.Shell(ctx, opt.Inject) }) {
		return r
	}
	step("observe and roll back", func() (string, error) {
		if err := e.Watch(ctx, d, opt.Phase); err != nil {
			return "", err
		}
		cp, _ := e.Deployment(id)
		if cp.State == model.StateAwaitApproval {
			// Approve mode: the lab approves at once to exercise the rollback.
			if err := e.DecideRollback(ctx, cp, "lab:"+opt.Label, true, "lab scenario"); err != nil {
				return "", err
			}
			cp, _ = e.Deployment(id)
		}
		r.FinalState = string(cp.State)
		r.TimeToFail, r.RollbackSeconds = timings(cp)
		if cp.State != model.StateRolledBack {
			return cp.Reason, fmt.Errorf("ended %s, want ROLLED_BACK", cp.State)
		}
		return cp.Reason, nil
	})
	if tc != nil {
		step("pool after", func() (string, error) {
			var err error
			if r.PoolAfter, err = tc.Pool(ctx); err != nil {
				return "", err
			}
			after := map[string]bool{}
			for _, m := range r.PoolAfter {
				after[m.ID] = m.Enabled
			}
			var off []string
			for _, m := range r.PoolBefore {
				if m.Enabled && !after[m.ID] {
					off = append(off, m.ID)
				}
			}
			if len(off) > 0 {
				return "", fmt.Errorf("not back in traffic: %s", strings.Join(off, ", "))
			}
			return fmt.Sprintf("%d of %d enabled again", enabled(r.PoolAfter), len(r.PoolAfter)), nil
		})
	}
	r.Passed = r.FinalState == string(model.StateRolledBack)
	for _, s := range r.Steps {
		r.Passed = r.Passed && s.OK
	}
	return r
}

func enabled(ms []executor.PoolMember) int {
	n := 0
	for _, m := range ms {
		if m.Enabled {
			n++
		}
	}
	return n
}

func timings(d *model.Deployment) (toFail, rollback float64) {
	var observing, failAt, rbStart time.Time
	for _, ev := range d.Events {
		switch {
		case ev.Kind == "state" && strings.HasPrefix(ev.Message, string(model.StateObserving)+":"):
			observing = ev.Time
		case ev.Kind == "verdict" && strings.HasPrefix(ev.Message, "FAIL:") && failAt.IsZero():
			failAt = ev.Time
		case ev.Kind == "state" && strings.HasPrefix(ev.Message, string(model.StateRollingBack)+":"):
			rbStart = ev.Time
		case ev.Kind == "state" && (strings.HasPrefix(ev.Message, string(model.StateRolledBack)+":") || strings.HasPrefix(ev.Message, string(model.StateRollbackFailed)+":")):
			if !rbStart.IsZero() {
				rollback = ev.Time.Sub(rbStart).Seconds()
			}
		}
	}
	if !observing.IsZero() && !failAt.IsZero() {
		toFail = failAt.Sub(observing).Seconds()
	}
	return toFail, rollback
}

// Summary aggregates results by label and plugin set (one matrix row each).
type Summary struct {
	Label          string   `json:"label"`
	Service        string   `json:"service"`
	Plugins        []string `json:"plugins"`
	Runs           int      `json:"runs"`
	Passed         int      `json:"passed"`
	MedianToFail   float64  `json:"median_time_to_fail_seconds"`
	MedianRollback float64  `json:"median_rollback_seconds"`
	Problems       []string `json:"problems,omitempty"`
}

func Summarize(rs []Result) []Summary {
	type key struct{ label, service, plugins string }
	groups := map[key][]Result{}
	var order []key
	for _, r := range rs {
		k := key{r.Label, r.Service, strings.Join(r.Plugins, ",")}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	var out []Summary
	for _, k := range order {
		g := groups[k]
		s := Summary{Label: k.label, Service: k.service, Plugins: g[0].Plugins, Runs: len(g)}
		var ttf, rb []float64
		seen := map[string]bool{}
		for _, r := range g {
			if r.Passed {
				s.Passed++
				ttf = append(ttf, r.TimeToFail)
				rb = append(rb, r.RollbackSeconds)
				continue
			}
			for _, st := range r.Steps {
				if !st.OK && !seen[st.Name+st.Detail] {
					seen[st.Name+st.Detail] = true
					s.Problems = append(s.Problems, st.Name+": "+st.Detail)
				}
			}
		}
		s.MedianToFail, s.MedianRollback = median(ttf), median(rb)
		out = append(out, s)
	}
	return out
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	return xs[len(xs)/2]
}

// Markdown renders summaries as rows for docs/09-compatibility.md.
func Markdown(ss []Summary) string {
	var b strings.Builder
	b.WriteString("| Equipment | Service | Plugins | Runs | Passed | Detect (median s) | Rollback (median s) | Problems |\n|---|---|---|---|---|---|---|---|\n")
	for _, s := range ss {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %.0f | %.0f | %s |\n", s.Label, s.Service, strings.Join(s.Plugins, ", "),
			s.Runs, s.Passed, s.MedianToFail, s.MedianRollback, strings.ReplaceAll(strings.Join(s.Problems, "; "), "|", "/"))
	}
	return b.String()
}

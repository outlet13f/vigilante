package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/executor"
	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/notify"
	"vigilante/internal/probe"
	"vigilante/internal/safety"
	"vigilante/internal/telemetry"
)

type RollbackOptions struct {
	Reason string
	// Manual rollbacks are operator decisions: they bypass the flapping guard
	// and the circuit breaker (but still take the per-service lock).
	Manual bool
	// Approved permits escalation steps marked require_approval (e.g. VM snapshot restore).
	Approved bool
	Executor string   // override the primary executor
	Targets  []string // override the target set
	// Actor is who asked (auth principal ID); empty for automatic rollbacks.
	Actor string
	// DetectedAt is when the failing evaluation happened (automatic rollbacks).
	DetectedAt time.Time
}

// ErrNeedsApproval marks a target whose remaining recovery path requires a human OK.
type ErrNeedsApproval struct{ Executor string }

func (e ErrNeedsApproval) Error() string {
	return "escalation to " + e.Executor + " requires approval (POST /v1/deployments/{id}/approve or `vigilante rollback --approve`)"
}

func isApp(action string) bool {
	return action == "app.rollback" || action == "app.verify" || action == "probe.verify"
}

func (e *Engine) rollbackTargets(d *model.Deployment, svc *config.Service, opt RollbackOptions) []string {
	if len(opt.Targets) > 0 {
		return opt.Targets
	}
	if svc.Rollback.Scope == "failed" {
		seen := map[string]bool{}
		var out []string
		for _, b := range d.Breaches {
			if !seen[b.Target] {
				seen[b.Target] = true
				out = append(out, b.Target)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return d.Targets
}

func (e *Engine) traffic(svc *config.Service) (executor.TrafficController, error) {
	if svc.Rollback.Traffic == "" {
		return nil, nil
	}
	return executor.NewTraffic(svc.Rollback.Traffic, e.Cfg.Traffic[svc.Rollback.Traffic], executor.TrafficEnv{
		Runners: e.runners, Creds: e.Cfg.Credentials, DryRun: e.DryRun, Log: e.Log, HTTP: e.trafficHTTP,
	})
}

// Traffic returns the traffic controller of a service's rollback plan (nil
// when the plan has none).
func (e *Engine) Traffic(service string) (executor.TrafficController, error) {
	svc, ok := e.Cfg.Service(service)
	if !ok {
		return nil, fmt.Errorf("unknown service %q", service)
	}
	return e.traffic(svc)
}

// Rollback executes the service's rollback plan for a deployment. The final
// deployment state tells the outcome: ROLLED_BACK, ROLLBACK_FAILED or
// AWAITING_APPROVAL.
func (e *Engine) Rollback(ctx context.Context, d *model.Deployment, opt RollbackOptions) error {
	if !e.Active() {
		return ErrInactive
	}
	svc, _ := e.Cfg.Service(d.Service)
	targets := e.rollbackTargets(d, svc, opt)
	if !opt.Manual {
		if f := e.ActiveFreeze(svc.Name, time.Now()); f != nil && !f.AllowRollback {
			return e.blocked(ctx, d, svc, fmt.Errorf("%w: change freeze %q forbids automatic rollback until %s",
				ErrFrozen, f.Name, f.Until.Format(time.RFC3339)))
		}
		if err := e.Guard.Check(svc.Name); err != nil {
			return e.blocked(ctx, d, svc, err)
		}
		if err := e.Breaker.Allow(); err != nil {
			return e.blocked(ctx, d, svc, err)
		}
	}
	if !opt.Manual {
		e.Audit(journal.Entry{Actor: "system", Source: "system", Action: "rollback.auto", Service: d.Service, DeployID: d.ID, Reason: opt.Reason})
	}
	if opt.Actor != "" {
		e.mu.Lock()
		if opt.Approved {
			d.ApprovedBy = opt.Actor
		} else {
			d.RollbackRequestedBy = opt.Actor
		}
		e.mu.Unlock()
		e.event(d, "actor", fmt.Sprintf("%s by %s", map[bool]string{true: "escalation approved", false: "rollback requested"}[opt.Approved], opt.Actor))
	}
	release, err := e.Guard.Acquire(svc.Name)
	if err != nil {
		e.setState(d, model.StateRollbackFailed, "rollback not started: "+err.Error())
		return err
	}
	defer release()

	now := time.Now()
	e.Guard.Record(svc.Name, now)
	e.record(journal.Entry{Kind: journal.KindRollbackStart, Service: svc.Name, DeployID: d.ID, Time: now})
	if !opt.DetectedAt.IsZero() {
		telemetry.RollbackTrigger.Since(opt.DetectedAt)
	}
	result := "failed"
	defer func() {
		telemetry.Rollbacks.Inc(svc.Name, result)
		telemetry.RollbackDuration.Since(now, result)
	}()
	reason := opt.Reason
	if reason == "" {
		reason = "manual rollback"
	}
	e.setState(d, model.StateRollingBack, fmt.Sprintf("rolling back %v to %s: %s", targets, d.PreviousVersion, reason))
	e.Notify.Send(ctx, notify.Message{Level: notify.Critical, Title: fmt.Sprintf("ROLLBACK %s %s -> %s", d.Service, d.Version, d.PreviousVersion), Text: reason, Deployment: d})

	run := &rollbackRun{e: e, d: d, svc: svc, opt: opt}
	failures := run.execute(ctx, targets)
	if !e.Active() {
		// Leadership moved mid-rollback. The new leader replays the recorded
		// steps and finishes; this node must not report a failure it did not have.
		e.Log.Warn("rollback handed over to the new leader", "deployment", d.ID)
		result = "handed_over"
		return ErrInactive
	}

	if len(failures) == 0 {
		result = "rolled_back"
		e.Breaker.Success()
		e.setState(d, model.StateRolledBack, fmt.Sprintf("%d target(s) restored to %s", len(targets), d.PreviousVersion))
		e.Notify.Send(ctx, notify.Message{Level: notify.Warning, Title: fmt.Sprintf("Rollback complete: %s back on %s", d.Service, d.PreviousVersion), Text: reason, Deployment: d})
		return nil
	}
	var msgs []string
	onlyApproval := true
	for t, err := range failures {
		msgs = append(msgs, t+": "+err.Error())
		var na ErrNeedsApproval
		if !errors.As(err, &na) {
			onlyApproval = false
		}
	}
	summary := strings.Join(msgs, "; ")
	if onlyApproval {
		result = "await_approval"
		e.setState(d, model.StateAwaitApproval, summary)
		e.Notify.Send(ctx, notify.Message{Level: notify.Critical, Title: fmt.Sprintf("%s rollback needs approval", d.Service), Text: summary, Deployment: d})
		return errors.New(summary)
	}
	e.Breaker.Failure(fmt.Sprintf("%s/%s: %s", d.Service, d.ID, summary))
	if svc.Rollback.Traffic != "" {
		summary += " — failed targets are left drained (isolated)"
	}
	e.setState(d, model.StateRollbackFailed, summary+"; circuit: "+string(e.Breaker.State().State))
	e.Notify.Send(ctx, notify.Message{Level: notify.Critical, Title: fmt.Sprintf("ROLLBACK FAILED %s — human intervention required", d.Service), Text: summary, Deployment: d})
	return errors.New(summary)
}

// blocked handles "we should roll back but automation is not allowed to"
// (circuit open, flapping): isolate the failing targets from traffic if that
// is safe, and hand over to a human.
func (e *Engine) blocked(ctx context.Context, d *model.Deployment, svc *config.Service, cause error) error {
	_, isolated := e.isolateBreaches(ctx, d, svc)
	telemetry.Rollbacks.Inc(svc.Name, "blocked")
	reason := fmt.Sprintf("automatic rollback blocked: %v; isolation: %s", cause, isolated)
	e.setState(d, model.StateRollbackFailed, reason)
	e.Notify.Send(ctx, notify.Message{Level: notify.Critical, Title: fmt.Sprintf("%s: rollback blocked by safety guard", d.Service), Text: reason, Deployment: d})
	return cause
}

// isolateBreaches drains the targets that breached rules, within the blast
// radius. It returns the drained targets and a summary for the reason text.
func (e *Engine) isolateBreaches(ctx context.Context, d *model.Deployment, svc *config.Service) ([]string, string) {
	tc, err := e.traffic(svc)
	if err != nil {
		return nil, "traffic controller error: " + err.Error()
	}
	if tc == nil {
		return nil, "no traffic controller configured"
	}
	seen := map[string]bool{}
	var members []executor.Member
	for _, b := range d.Breaches {
		if !seen[b.Target] {
			seen[b.Target] = true
			t, _ := e.Cfg.Target(b.Target)
			members = append(members, executor.Member{Target: b.Target, Data: e.templateData(d, *t)})
		}
	}
	return e.isolate(ctx, tc, members)
}

func (e *Engine) isolate(ctx context.Context, tc executor.TrafficController, members []executor.Member) ([]string, string) {
	if len(members) == 0 {
		return nil, "no failing targets identified"
	}
	pool, err := tc.Pool(ctx)
	if err != nil {
		return nil, "pool state unavailable, not draining: " + err.Error()
	}
	enabled := 0
	for _, p := range pool {
		if p.Enabled {
			enabled++
		}
	}
	n, why := safety.DrainBatch(len(pool), enabled, len(members), e.Cfg.Safety.BlastRadius)
	if n == 0 {
		return nil, why
	}
	if err := tc.Drain(ctx, members[:n]); err != nil {
		return nil, "drain failed: " + err.Error()
	}
	names := make([]string, n)
	for i := range n {
		names[i] = members[i].Target
	}
	return names, fmt.Sprintf("drained %v %s", names, why)
}

type rollbackRun struct {
	e   *Engine
	d   *model.Deployment
	svc *config.Service
	opt RollbackOptions
	tc  executor.TrafficController
}

func (r *rollbackRun) execute(ctx context.Context, targets []string) map[string]error {
	failures := map[string]error{}
	tc, err := r.e.traffic(r.svc)
	if err != nil {
		for _, t := range targets {
			failures[t] = fmt.Errorf("traffic controller: %w", err)
		}
		return failures
	}
	r.tc = tc
	batch := r.svc.Rollback.Parallelism
	drain := tc != nil
	if tc != nil {
		pool, err := tc.Pool(ctx)
		if err != nil {
			batch = 1
			r.e.event(r.d, "blast-radius", "pool state unavailable, rolling back one target at a time: "+err.Error())
		} else {
			enabled := 0
			for _, p := range pool {
				if p.Enabled {
					enabled++
				}
			}
			n, why := safety.DrainBatch(len(pool), enabled, len(targets), r.e.Cfg.Safety.BlastRadius)
			if n == 0 {
				drain, batch = false, 1
				r.e.event(r.d, "blast-radius", why+"; rolling back in place without draining")
			} else {
				batch = min(batch, n)
				if why != "" {
					r.e.event(r.d, "blast-radius", why)
				}
			}
		}
	}
	batch = max(batch, 1)
	var mu sync.Mutex
	for i := 0; i < len(targets); i += batch {
		chunk := targets[i:min(i+batch, len(targets))]
		var wg sync.WaitGroup
		for _, tn := range chunk {
			wg.Add(1)
			go func(tn string) {
				defer wg.Done()
				if err := r.target(ctx, tn, drain); err != nil {
					mu.Lock()
					failures[tn] = err
					mu.Unlock()
					r.e.event(r.d, "target-failed", fmt.Sprintf("%s: %v", tn, err))
				} else {
					r.e.event(r.d, "target-restored", tn+" restored")
				}
			}(tn)
		}
		wg.Wait()
	}
	return failures
}

func (r *rollbackRun) stepsDone(target string) int {
	r.e.mu.Lock()
	defer r.e.mu.Unlock()
	return r.e.stepsDone[r.d.ID][target]
}

func (r *rollbackRun) markDone(target string, step int) {
	r.e.mu.Lock()
	m := r.e.stepsDone[r.d.ID]
	if m == nil {
		m = map[string]int{}
		r.e.stepsDone[r.d.ID] = m
	}
	m[target] = step + 1
	r.e.mu.Unlock()
	r.e.record(journal.Entry{Kind: journal.KindStepDone, DeployID: r.d.ID, Target: target, Step: step})
}

func (r *rollbackRun) primary() (executor.Executor, string, error) {
	name := r.svc.Rollback.Executor
	if r.opt.Executor != "" {
		name = r.opt.Executor
	}
	spec, ok := r.e.Cfg.Executors[name]
	if !ok {
		return nil, name, fmt.Errorf("unknown executor %q", name)
	}
	ex, err := executor.New(name, spec)
	return ex, name, err
}

func (r *rollbackRun) target(ctx context.Context, tn string, drain bool) error {
	t, ok := r.e.Cfg.Target(tn)
	if !ok {
		return fmt.Errorf("unknown target %q", tn)
	}
	rc := r.e.runContext(r.d, *t)
	ex, exName, err := r.primary()
	if err != nil {
		return err
	}
	member := executor.Member{Target: tn, Data: rc.Data}
	plan := r.svc.Rollback.Plan
	done := r.stepsDone(tn)
	for i, st := range plan {
		if i < done {
			continue // completed before a crash; steps are idempotent but skipping is faster
		}
		// Only the drain is optional. traffic.enable always runs: it is
		// idempotent, and after a crash the target may still be drained from
		// the previous attempt even if draining is not allowed now.
		if st.Action == "traffic.drain" && !drain {
			r.markDone(tn, i)
			continue
		}
		if !r.e.Active() {
			return ErrInactive
		}
		r.e.event(r.d, "step", fmt.Sprintf("%s: %s (%s)", tn, st.Action, exName))
		err := r.step(ctx, st, ex, rc, member)
		if errors.Is(err, ErrInactive) {
			return err
		}
		if err == nil {
			r.markDone(tn, i)
			continue
		}
		switch {
		case st.Action == "traffic.drain":
			// Rolling back a target that is still in rotation is better than not rolling back.
			r.e.event(r.d, "step-failed", fmt.Sprintf("%s: drain failed, continuing in place: %v", tn, err))
			r.markDone(tn, i)
			continue
		case isApp(st.Action):
			r.e.event(r.d, "step-failed", fmt.Sprintf("%s: %s failed with %s: %v", tn, st.Action, exName, err))
			if err := r.escalate(ctx, plan, rc, member, err); err != nil {
				return err // traffic.enable is NOT run: the target stays isolated
			}
			for j := i + 1; j < len(plan); j++ {
				if isApp(plan[j].Action) {
					continue // belonged to the primary strategy
				}
				if plan[j].Action == "traffic.drain" {
					continue
				}
				if err := r.step(ctx, plan[j], ex, rc, member); err != nil {
					return fmt.Errorf("%s after escalation: %w", plan[j].Action, err)
				}
				r.markDone(tn, j)
			}
			return nil
		default:
			return fmt.Errorf("%s: %w", st.Action, err)
		}
	}
	return nil
}

// escalate walks the escalation ladder (e.g. container switch -> VM snapshot).
func (r *rollbackRun) escalate(ctx context.Context, plan []config.Step, rc *executor.RunContext, member executor.Member, cause error) error {
	if len(r.svc.Rollback.Escalation) == 0 {
		return cause
	}
	last := cause
	for _, esc := range r.svc.Rollback.Escalation {
		if esc.RequireApproval && !r.opt.Approved {
			return ErrNeedsApproval{Executor: esc.Executor}
		}
		ex, err := executor.New(esc.Executor, r.e.Cfg.Executors[esc.Executor])
		if err != nil {
			last = err
			continue
		}
		r.e.event(r.d, "escalation", fmt.Sprintf("%s: escalating to %s", rc.Target.Name, esc.Executor))
		err = r.step(ctx, config.Step{Action: "app.rollback"}, ex, rc, member)
		if err == nil {
			err = r.step(ctx, config.Step{Action: "app.verify"}, ex, rc, member)
		}
		for _, st := range plan {
			if err == nil && st.Action == "probe.verify" {
				err = r.step(ctx, st, ex, rc, member)
			}
		}
		if err == nil {
			return nil
		}
		r.e.event(r.d, "escalation-failed", fmt.Sprintf("%s: %s failed: %v", rc.Target.Name, esc.Executor, err))
		last = err
	}
	return fmt.Errorf("all strategies failed, last error: %w", last)
}

// step runs one plan step with per-attempt timeout and exponential backoff.
func (r *rollbackRun) step(ctx context.Context, st config.Step, ex executor.Executor, rc *executor.RunContext, m executor.Member) error {
	rb := r.svc.Rollback
	attempts := rb.Retry.Attempts
	if st.Action == "probe.verify" || st.Action == "wait" {
		attempts = 1 // these loop internally
	}
	timeout := rb.StepTimeout
	if st.Timeout > 0 {
		timeout = st.Timeout
	}
	backoff := rb.Retry.Backoff
	var err error
	for a := 1; a <= attempts; a++ {
		if !r.e.Active() {
			return ErrInactive
		}
		actx, cancel := context.WithTimeout(ctx, timeout)
		err = r.do(actx, st, ex, rc, m)
		cancel()
		if err == nil {
			return nil
		}
		if a < attempts {
			r.e.event(r.d, "retry", fmt.Sprintf("%s: %s attempt %d/%d failed: %v (retry in %s)", rc.Target.Name, st.Action, a, attempts, err, backoff))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, rb.Retry.MaxBackoff)
		}
	}
	return err
}

func (r *rollbackRun) do(ctx context.Context, st config.Step, ex executor.Executor, rc *executor.RunContext, m executor.Member) error {
	switch st.Action {
	case "traffic.drain":
		if err := r.tc.Drain(ctx, []executor.Member{m}); err != nil {
			return err
		}
		if w := r.e.Cfg.Traffic[r.svc.Rollback.Traffic].DrainWait; w > 0 && !r.e.DryRun {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(w):
			}
		}
		return nil
	case "traffic.enable":
		return r.tc.Enable(ctx, []executor.Member{m})
	case "app.rollback":
		return ex.Rollback(ctx, rc)
	case "app.verify":
		return ex.Verify(ctx, rc)
	case "wait":
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(st.Duration):
			return nil
		}
	case "probe.verify":
		return r.probeVerify(ctx, st, rc)
	}
	return fmt.Errorf("unknown step %q", st.Action)
}

// probeVerify requires N consecutive successful health checks.
func (r *rollbackRun) probeVerify(ctx context.Context, st config.Step, rc *executor.RunContext) error {
	if r.e.DryRun {
		return nil
	}
	spec, _ := r.svc.Probe(st.Probe)
	p, err := r.e.collector(r.d).Build(probe.Job{Target: rc.Target, Spec: *spec})
	if err != nil {
		return err
	}
	if c, ok := p.(io.Closer); ok {
		defer c.Close()
	}
	checker, ok := p.(probe.Checker)
	if !ok {
		return fmt.Errorf("probe %s (%s) cannot be used for verification", spec.ID, spec.Type)
	}
	streak := 0
	var last error
	for {
		cctx, cancel := context.WithTimeout(ctx, spec.Timeout)
		last = checker.Check(cctx)
		cancel()
		if last == nil {
			if streak++; streak >= st.Successes {
				return nil
			}
		} else {
			streak = 0
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("health not confirmed (%d/%d consecutive successes): %v", streak, st.Successes, last)
		case <-time.After(spec.Interval):
		}
	}
}

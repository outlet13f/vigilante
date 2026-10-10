package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/executor"
	"vigilante/internal/model"
	"vigilante/internal/notify"
	"vigilante/internal/telemetry"
)

// ErrNoPendingRollback means there is no rollback waiting for a decision.
var ErrNoPendingRollback = errors.New("no rollback is waiting for approval on this deployment")

// requestApproval prepares the rollback of a failed phase (approve mode)
// and waits for a person: the deployment goes to AWAITING_APPROVAL with the
// plan recorded, optionally with the failing targets drained meanwhile.
func (e *Engine) requestApproval(ctx context.Context, d *model.Deployment, svc *config.Service, reason string, detected time.Time) {
	now := time.Now().UTC()
	pr := &model.PendingRollback{Reason: reason, Targets: e.rollbackTargets(d, svc, RollbackOptions{}),
		RequestedAt: now, ExpiresAt: now.Add(svc.Rollback.Approval.Timeout), DetectedAt: detected}
	isolation := ""
	if svc.Rollback.Approval.DrainFirst {
		drained, summary := e.isolateBreaches(ctx, d, svc)
		pr.Drained, isolation = drained, "; isolation: "+summary
	}
	e.mu.Lock()
	d.PendingRollback = pr
	e.mu.Unlock()
	telemetry.Approvals.Inc(svc.Name, "requested")
	msg := fmt.Sprintf("rollback of %v to %s awaits approval until %s: %s%s",
		pr.Targets, d.PreviousVersion, pr.ExpiresAt.Format(time.RFC3339), reason, isolation)
	e.setState(d, model.StateAwaitApproval, msg)
	e.Notify.Send(ctx, notify.Message{Level: notify.Critical,
		Title:      fmt.Sprintf("%s %s failed — rollback to %s awaits approval", d.Service, d.Phase, d.PreviousVersion),
		Text:       msg + fmt.Sprintf("\nApprove or reject in the console, with POST /v2/deployments/%s/approvals, or `vigilante rollback --id %s --approve|--reject`", d.ID, d.ID),
		Deployment: d})
}

// PendingRollback returns the rollback a deployment waits on, if any.
func (e *Engine) PendingRollback(id string) (*model.PendingRollback, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.deployments[id]
	if d == nil || d.PendingRollback == nil || d.State != model.StateAwaitApproval {
		return nil, false
	}
	cp := *d.PendingRollback
	return &cp, true
}

// DecideRollback approves (runs the prepared rollback) or rejects (puts any
// drained targets back and holds the deployment for a human) a pending
// rollback. actor is the deciding principal.
func (e *Engine) DecideRollback(ctx context.Context, d *model.Deployment, actor string, approve bool, comment string) error {
	if !e.Active() {
		return ErrInactive
	}
	e.mu.Lock()
	pr := d.PendingRollback
	if pr == nil || d.State != model.StateAwaitApproval {
		e.mu.Unlock()
		return ErrNoPendingRollback
	}
	d.PendingRollback = nil
	if approve {
		d.ApprovedBy = actor
	}
	e.mu.Unlock()
	svc, _ := e.Cfg.Service(d.Service)
	note := ""
	if comment != "" {
		note = ": " + comment
	}
	if approve {
		telemetry.Approvals.Inc(svc.Name, "approved")
		e.event(d, "approval", "rollback approved by "+actor+note)
		return e.Rollback(ctx, d, RollbackOptions{Reason: pr.Reason + " (approved by " + actor + ")", Manual: true,
			Targets: pr.Targets, DetectedAt: pr.DetectedAt})
	}
	telemetry.Approvals.Inc(svc.Name, "rejected")
	restored := ""
	if len(pr.Drained) > 0 {
		restored = "; " + e.restoreTraffic(ctx, d, svc, pr.Drained)
	}
	e.setState(d, model.StateHeld, "rollback rejected by "+actor+note+restored)
	e.Notify.Send(ctx, notify.Message{Level: notify.Warning, Title: fmt.Sprintf("%s rollback rejected by %s", d.Service, actor),
		Text: "The deployment stays on the new version and is held for a human decision" + note + restored, Deployment: d})
	return nil
}

// restoreTraffic puts drained targets back into rotation.
func (e *Engine) restoreTraffic(ctx context.Context, d *model.Deployment, svc *config.Service, targets []string) string {
	tc, err := e.traffic(svc)
	if err != nil || tc == nil {
		return fmt.Sprintf("could not re-enable %v: no traffic controller (%v)", targets, err)
	}
	var ms []executor.Member
	for _, name := range targets {
		if t, ok := e.Cfg.Target(name); ok {
			ms = append(ms, executor.Member{Target: name, Data: e.templateData(d, *t)})
		}
	}
	if err := tc.Enable(ctx, ms); err != nil {
		return fmt.Sprintf("re-enabling %v failed: %v (still drained)", targets, err)
	}
	return fmt.Sprintf("re-enabled %v", targets)
}

// ExpireApprovals handles pending rollbacks past their timeout: with
// on_timeout rollback they roll back automatically (circuit breaker and
// flapping guard apply); with hold they keep waiting and operators are
// alerted once more. The active leader calls it periodically.
func (e *Engine) ExpireApprovals(ctx context.Context, now time.Time) {
	if !e.Active() {
		return
	}
	for _, snap := range e.Deployments() {
		if snap.State != model.StateAwaitApproval || snap.PendingRollback == nil || now.Before(snap.PendingRollback.ExpiresAt) {
			continue
		}
		d := e.Live(snap.ID)
		svc, ok := e.Cfg.Service(d.Service)
		if !ok {
			continue
		}
		if svc.Rollback.Approval.OnTimeout == "rollback" {
			e.mu.Lock()
			pr := d.PendingRollback
			d.PendingRollback = nil
			e.mu.Unlock()
			if pr == nil {
				continue
			}
			telemetry.Approvals.Inc(svc.Name, "expired")
			e.event(d, "approval", "no decision before "+pr.ExpiresAt.Format(time.RFC3339)+": rolling back automatically (on_timeout: rollback)")
			_ = e.Rollback(ctx, d, RollbackOptions{Reason: pr.Reason + " (approval timed out)", Targets: pr.Targets, DetectedAt: pr.DetectedAt})
			continue
		}
		e.mu.Lock()
		pr := d.PendingRollback
		if pr == nil || pr.Escalated {
			e.mu.Unlock()
			continue
		}
		pr.Escalated = true
		e.mu.Unlock()
		telemetry.Approvals.Inc(svc.Name, "expired")
		e.event(d, "approval", "no decision before "+pr.ExpiresAt.Format(time.RFC3339)+"; still waiting (on_timeout: hold)")
		e.persist(d)
		e.Notify.Send(ctx, notify.Message{Level: notify.Critical,
			Title: fmt.Sprintf("ESCALATION: %s rollback still awaits approval", d.Service),
			Text:  fmt.Sprintf("No decision since %s. The failing release is still running%s.", pr.RequestedAt.Format(time.RFC3339), drainedNote(pr)), Deployment: d})
	}
}

func drainedNote(pr *model.PendingRollback) string {
	if len(pr.Drained) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%v drained)", pr.Drained)
}

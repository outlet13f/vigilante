package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"vigilante/internal/events"
	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/telemetry"
)

// itsmWorker follows the event stream and, on the active leader, opens
// ServiceNow incidents (failed rollback, open circuit) and writes deployment
// outcomes to the change ticket. It never blocks or delays a rollback.
func (s *Server) itsmWorker() {
	if s.E.ITSM == nil {
		return
	}
	after := s.bus.Seq()
	for s.ctx.Err() == nil {
		backlog, sub := s.bus.Subscribe(after, events.Filter{})
		for _, ev := range backlog {
			after = ev.Sequence
			s.handleITSM(ev)
		}
	stream:
		for {
			select {
			case <-s.ctx.Done():
				s.bus.Unsubscribe(sub)
				return
			case ev, ok := <-sub.C():
				if !ok {
					break stream // fell behind: resubscribe from the last handled event
				}
				after = ev.Sequence
				s.handleITSM(ev)
			}
		}
	}
}

// workNoteEvents are written to the deployment's change ticket.
var workNoteEvents = map[string]string{
	model.EvObservationStarted: "observation started", model.EvObservationPassed: "phase passed",
	model.EvObservationFailed: "phase FAILED", model.EvObservationHeld: "phase held for a human decision",
	model.EvRollbackStarted: "rollback started", model.EvRollbackCompleted: "rollback completed",
	model.EvRollbackFailed: "ROLLBACK FAILED", model.EvApprovalRequested: "rollback awaits approval",
	model.EvApprovalDecided: "approval decided",
}

func (s *Server) handleITSM(ev *model.CloudEvent) {
	if !s.E.Active() {
		return
	}
	cfg := s.E.ITSM.Config()
	var data struct {
		DeploymentID string `json:"deployment_id"`
		Service      string `json:"service"`
		Version      string `json:"version"`
		Previous     string `json:"previous_version"`
		State        string `json:"state"`
		Reason       string `json:"reason"`
		Decision     string `json:"decision"`
		DecidedBy    string `json:"decided_by"`
	}
	_ = json.Unmarshal(ev.Data, &data)
	inc := cfg.Incidents

	switch {
	case ev.Type == model.EvRollbackFailed && inc.Enabled && slices.Contains(inc.On, "rollback_failed"):
		short := fmt.Sprintf("Vigilante: rollback of %s %s failed", data.Service, data.Version)
		desc := fmt.Sprintf("Deployment %s of %s (%s -> %s) could not be rolled back and needs a human.\n\nState: %s\nReason: %s\n\nFailed targets are left drained where a traffic controller exists. See the Vigilante console or GET /v2/deployments/%s.",
			data.DeploymentID, data.Service, data.Previous, data.Version, data.State, data.Reason, data.DeploymentID)
		s.openIncident("vigilante:"+data.DeploymentID+":rollback_failed", short, desc, data.DeploymentID, data.Service)
	case ev.Type == model.EvCircuitOpened && inc.Enabled && slices.Contains(inc.On, "circuit_opened"):
		var c struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Data, &c)
		s.openIncident("vigilante:circuit:"+ev.ID, "Vigilante: circuit breaker OPEN, automatic rollbacks frozen",
			"The safety circuit breaker opened: "+c.Reason+"\n\nNew deployments are refused and automatic rollbacks are frozen until an admin resets the circuit after investigation.", "", "")
	}

	label, ok := workNoteEvents[ev.Type]
	if !ok || data.DeploymentID == "" || (cfg.WorkNotes != nil && !*cfg.WorkNotes) {
		return
	}
	d, found := s.E.Deployment(data.DeploymentID)
	if !found || d.ChangeTicket == nil || d.ChangeTicket.SysID == "" {
		return
	}
	note := fmt.Sprintf("[vigilante] %s — %s %s (deployment %s, state %s)", label, data.Service, data.Version, data.DeploymentID, data.State)
	if data.Decision != "" {
		note += fmt.Sprintf(": %s by %s", data.Decision, data.DecidedBy)
	}
	if data.Reason != "" && ev.Type != model.EvApprovalDecided {
		note += "\n" + data.Reason
	}
	s.itsmCall("work_note", func(ctx context.Context) error { return s.E.ITSM.WorkNote(ctx, d.ChangeTicket.SysID, note) })
}

func (s *Server) openIncident(correlation, short, desc, deploymentID, service string) {
	var number string
	var created bool
	ok := s.itsmCall("incident", func(ctx context.Context) error {
		var err error
		number, created, err = s.E.ITSM.EnsureIncident(ctx, correlation, short, desc)
		return err
	})
	if !ok || !created {
		return
	}
	s.E.Audit(journal.Entry{Actor: "system", Source: "system", Action: "itsm.incident", Service: service, DeployID: deploymentID,
		Reason: number + " opened: " + short})
	if d := s.E.Live(deploymentID); d != nil {
		s.E.Annotate(d, "itsm", "incident "+number+" opened")
	}
}

// itsmCall retries a ServiceNow call a few times; failures are logged and
// counted, never raised.
func (s *Server) itsmCall(kind string, fn func(context.Context) error) bool {
	var err error
	for attempt, wait := range []time.Duration{0, 2 * time.Second, 10 * time.Second} {
		if attempt > 0 {
			select {
			case <-s.ctx.Done():
				return false
			case <-time.After(wait):
			}
		}
		ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
		err = fn(ctx)
		cancel()
		if err == nil {
			telemetry.ITSMCalls.Inc(kind, "ok")
			return true
		}
		if strings.Contains(err.Error(), ": 4") && !strings.Contains(err.Error(), ": 429") { // a 4xx will not get better
			break
		}
	}
	telemetry.ITSMCalls.Inc(kind, "error")
	s.E.Log.Error("servicenow call failed", "kind", kind, "err", err)
	return false
}

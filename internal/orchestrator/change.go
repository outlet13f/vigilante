package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"vigilante/internal/itsm"
	"vigilante/internal/model"
)

var (
	// ErrChangeTicket means the change ticket is missing or does not allow the deployment.
	ErrChangeTicket = errors.New("change ticket")
	// ErrITSMUnavailable means the ticket could not be verified and the gate fails closed.
	ErrITSMUnavailable = errors.New("ITSM unavailable")
)

// changeGateApplies reports whether new deployments of service need a ticket.
func (e *Engine) changeGateApplies(service string) bool {
	if e.ITSM == nil {
		return false
	}
	g := e.ITSM.Config().ChangeGate
	if !g.Enabled {
		return false
	}
	if len(g.Services) == 0 && len(g.Teams) == 0 {
		return true
	}
	sv, _ := e.Cfg.Service(service)
	return slices.Contains(g.Services, service) || (sv != nil && sv.Team != "" && slices.Contains(g.Teams, sv.Team))
}

// ChangeGate verifies the change ticket for a new deployment of service.
// It returns nil, nil when no gate applies.
func (e *Engine) ChangeGate(ctx context.Context, service, ticket string) (*model.ChangeTicket, error) {
	if !e.changeGateApplies(service) {
		return nil, nil
	}
	if ticket == "" {
		return nil, fmt.Errorf("%w: %w required for %s (API X-Change-Ticket header or change_ticket, CLI --ticket)", ErrBlocked, ErrChangeTicket, service)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c, err := e.ITSM.Check(ctx, ticket)
	var invalid *itsm.ErrInvalidChange
	switch {
	case err == nil:
		return &model.ChangeTicket{Number: c.Number, SysID: c.SysID, State: c.State, Approval: c.Approval, CheckedAt: time.Now().UTC()}, nil
	case errors.As(err, &invalid):
		return nil, fmt.Errorf("%w: %w: %s", ErrBlocked, ErrChangeTicket, invalid.Reason)
	case e.ITSM.Config().ChangeGate.OnError == "open":
		e.Log.Warn("change ticket not verified; proceeding (change_gate.on_error: open)", "ticket", ticket, "err", err)
		return &model.ChangeTicket{Number: ticket, CheckedAt: time.Now().UTC(), Unverified: true}, nil
	default:
		return nil, fmt.Errorf("%w: %w: could not verify %s: %v (change_gate.on_error: closed)", ErrBlocked, ErrITSMUnavailable, ticket, err)
	}
}

// SetChangeTicket records the verified ticket on the deployment.
func (e *Engine) SetChangeTicket(d *model.Deployment, ct *model.ChangeTicket) {
	if ct == nil {
		return
	}
	e.mu.Lock()
	d.ChangeTicket = ct
	e.mu.Unlock()
	note := "change " + ct.Number + " verified"
	if ct.Unverified {
		note = "change " + ct.Number + " NOT verified (ITSM unreachable, on_error: open)"
	}
	e.event(d, "change", note)
	e.persist(d)
}

// recheckChange re-verifies the ticket when a phase starts: the planned
// window may have closed since the deployment was registered.
func (e *Engine) recheckChange(d *model.Deployment) error {
	if !e.changeGateApplies(d.Service) {
		return nil
	}
	ticket := ""
	if d.ChangeTicket != nil {
		ticket = d.ChangeTicket.Number
	}
	ct, err := e.ChangeGate(context.Background(), d.Service, ticket)
	if err != nil {
		return err
	}
	e.mu.Lock()
	d.ChangeTicket = ct
	e.mu.Unlock()
	return nil
}

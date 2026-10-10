package orchestrator

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/safety"
)

// ErrFrozen is returned while a change freeze refuses new deployments.
var ErrFrozen = errors.New("change freeze")

// ActiveFreeze describes the freeze that applies to a service right now.
type ActiveFreeze struct {
	ID            string    `json:"id"` // config:<name> or the API freeze ID
	Name          string    `json:"name"`
	Reason        string    `json:"reason,omitempty"`
	Until         time.Time `json:"until"`
	AllowRollback bool      `json:"allow_rollback"`
	Source        string    `json:"source"` // config | api
}

// FreezeWindow is any freeze, active or not (config windows and API freezes).
type FreezeWindow struct {
	ActiveFreeze
	StartsAt  *time.Time `json:"starts_at,omitempty"` // absolute windows
	EndsAt    *time.Time `json:"ends_at,omitempty"`
	Weekly    string     `json:"weekly,omitempty"` // "fri 18:00 - mon 09:00 Asia/Seoul"
	Services  []string   `json:"services"`
	Teams     []string   `json:"teams"`
	Active    bool       `json:"active"`
	CreatedBy string     `json:"created_by,omitempty"`
}

// ActiveFreeze returns the freeze covering service at t (the one ending
// last when several overlap), or nil.
func (e *Engine) ActiveFreeze(service string, t time.Time) *ActiveFreeze {
	team := ""
	if sv, ok := e.Cfg.Service(service); ok {
		team = sv.Team
	}
	var best *ActiveFreeze
	consider := func(f *ActiveFreeze) {
		if best == nil || f.Until.After(best.Until) {
			best = f
		}
	}
	for i := range e.Cfg.ChangeFreeze {
		f := &e.Cfg.ChangeFreeze[i]
		if until, ok := f.ActiveAt(t); ok && f.Covers(service, team) {
			consider(&ActiveFreeze{ID: "config:" + f.Name, Name: f.Name, Reason: f.Reason, Until: until, AllowRollback: f.RollbackAllowed(), Source: "config"})
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range e.freezes {
		if f.EndedAt != nil || t.Before(f.StartsAt) || !t.Before(f.EndsAt) {
			continue
		}
		if (len(f.Services) == 0 && len(f.Teams) == 0) || slices.Contains(f.Services, service) || (team != "" && slices.Contains(f.Teams, team)) {
			consider(&ActiveFreeze{ID: f.ID, Name: f.Name, Reason: f.Reason, Until: f.EndsAt, AllowRollback: f.AllowRollback, Source: "api"})
		}
	}
	return best
}

// Gate reports why a phase of d may not start now: the circuit breaker is
// open, or a change freeze applies and the deployment has no override.
func (e *Engine) Gate(d *model.Deployment) error {
	if st := e.Breaker.State(); st.State == safety.Open {
		return fmt.Errorf("%w: %w (%s); reset with `vigilante circuit reset` after investigation", ErrBlocked, safety.ErrCircuitOpen, st.Reason)
	}
	if err := e.FreezeGate(d.Service, d.FreezeOverride); err != nil {
		return err
	}
	return e.recheckChange(d)
}

// FreezeGate refuses new work on a frozen service unless override is set.
func (e *Engine) FreezeGate(service, override string) error {
	if override != "" {
		return nil
	}
	if f := e.ActiveFreeze(service, time.Now()); f != nil {
		reason := ""
		if f.Reason != "" {
			reason = " (" + f.Reason + ")"
		}
		return fmt.Errorf("%w: %w %q%s until %s; to proceed anyway give a reason: API freeze_override (admins) or CLI --freeze-override", ErrBlocked, ErrFrozen, f.Name, reason, f.Until.Format(time.RFC3339))
	}
	return nil
}

// SetFreezeOverride lets d proceed during a freeze; who and why are kept.
func (e *Engine) SetFreezeOverride(d *model.Deployment, actor, reason string) {
	e.mu.Lock()
	d.FreezeOverride = reason + " (by " + actor + ")"
	e.mu.Unlock()
	e.event(d, "freeze", "change freeze overridden by "+actor+": "+reason)
	e.persist(d)
}

// CreateFreeze declares a freeze at runtime.
func (e *Engine) CreateFreeze(f *model.Freeze) {
	f.ID = newID("frz_")
	f.CreatedAt = time.Now().UTC()
	e.mu.Lock()
	e.freezes[f.ID] = f
	cp := *f
	e.mu.Unlock()
	e.record(journal.Entry{Kind: journal.KindFreeze, Freeze: &cp})
}

// EndFreeze ends an API freeze early. Config windows end by editing the config.
func (e *Engine) EndFreeze(id, actor string) (*model.Freeze, error) {
	e.mu.Lock()
	f, ok := e.freezes[id]
	if !ok {
		e.mu.Unlock()
		return nil, fmt.Errorf("freeze %q not found (config windows end by changing the config)", id)
	}
	if f.EndedAt == nil {
		now := time.Now().UTC()
		f.EndedAt, f.EndedBy = &now, actor
	}
	cp := *f
	e.mu.Unlock()
	e.record(journal.Entry{Kind: journal.KindFreeze, Freeze: &cp})
	return &cp, nil
}

// Freezes lists config windows and API freezes that have not ended.
func (e *Engine) Freezes(t time.Time) []FreezeWindow {
	var out []FreezeWindow
	for i := range e.Cfg.ChangeFreeze {
		f := &e.Cfg.ChangeFreeze[i]
		until, active := f.ActiveAt(t)
		w := FreezeWindow{ActiveFreeze: ActiveFreeze{ID: "config:" + f.Name, Name: f.Name, Reason: f.Reason, Until: until,
			AllowRollback: f.RollbackAllowed(), Source: "config"}, Services: nz(f.Services), Teams: nz(f.Teams), Active: active}
		if f.Weekly != nil {
			tz := f.Weekly.Timezone
			if tz == "" {
				tz = "Local"
			}
			w.Weekly = f.Weekly.From + " - " + f.Weekly.To + " " + tz
		} else if s, err := time.Parse(time.RFC3339, f.Start); err == nil {
			en, _ := time.Parse(time.RFC3339, f.End)
			w.StartsAt, w.EndsAt = &s, &en
			if !active && t.After(en) {
				continue // past
			}
		}
		out = append(out, w)
	}
	e.mu.Lock()
	for _, f := range e.freezes {
		if f.EndedAt != nil || !t.Before(f.EndsAt) {
			continue
		}
		s, en := f.StartsAt, f.EndsAt
		active := !t.Before(s)
		w := FreezeWindow{ActiveFreeze: ActiveFreeze{ID: f.ID, Name: f.Name, Reason: f.Reason, AllowRollback: f.AllowRollback, Source: "api"},
			StartsAt: &s, EndsAt: &en, Services: nz(f.Services), Teams: nz(f.Teams), Active: active, CreatedBy: f.CreatedBy}
		if active {
			w.Until = en
		}
		out = append(out, w)
	}
	e.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func nz(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

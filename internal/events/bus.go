// Package events turns what the engine records into CloudEvents, keeps the
// recent ones for SSE resume, and delivers them to webhook subscriptions.
//
// Events are derived from stored journal entries (deployment state changes,
// the circuit breaker, approvals), so nothing in the engine has to remember
// to publish. Each event gets the next cluster-wide sequence number and is
// itself stored, so a new HA leader continues the sequence and webhook
// cursors where the old one stopped. Delivery is at least once and in order
// per subscription; receivers deduplicate by the webhook-id header.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/safety"
)

// Deps connects the bus to the engine and the outside world.
type Deps struct {
	Record func(journal.Entry) // durable store write (fenced on a demoted leader)
	Active func() bool         // only the active leader publishes and delivers
	Log    *slog.Logger
	// Alert tells operators (notification channels) about a disabled webhook.
	Alert func(title, text string)
	// Audit records bus actions in the audit trail.
	Audit func(action, reason string)
	// SigningKey returns the master key webhook secrets derive from.
	SigningKey func(ctx context.Context) ([]byte, error)
	// TeamOf maps a service to its owning team (for filters and SSE scopes).
	TeamOf func(service string) string
	Source string       // CloudEvents source, e.g. "/vigilante"
	HTTP   *http.Client // webhook delivery client (no redirects)
}

// Bus is the event hub of one server process.
type Bus struct {
	d Deps
	// Backoff is the wait before each retry of a failed delivery; after the
	// last one the event goes to the dead-letter list.
	Backoff []time.Duration
	// DisableAfter consecutive dead letters disable a subscription.
	DisableAfter int

	mu          sync.Mutex
	seq         int64
	events      []*model.CloudEvent // recent, oldest first
	prevState   map[string]model.State
	prevCircuit safety.CircuitStateName
	subs        map[*Sub]bool
	changed     chan struct{} // closed and replaced on every change

	hooks     map[string]*model.Webhook
	workers   map[string]context.CancelFunc
	queue     map[string][]int64 // manual redeliveries per webhook
	history   map[string][]Delivery
	ctx       context.Context
	cancelAll context.CancelFunc
}

// Delivery is one webhook attempt, kept in memory for the deliveries API.
type Delivery struct {
	Sequence   int64     `json:"sequence"`
	Type       string    `json:"type"`
	Attempt    int       `json:"attempt"`
	At         time.Time `json:"at"`
	StatusCode int       `json:"status_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	DurationMs int64     `json:"duration_ms"`
	Outcome    string    `json:"outcome"` // delivered | retrying | dead
}

const maxHistory = 100

// New builds a bus; call Reload with the replayed state, then Start.
func New(d Deps) *Bus {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Source == "" {
		d.Source = "/vigilante"
	}
	if d.HTTP == nil {
		d.HTTP = &http.Client{Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Bus{d: d,
		Backoff:      []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute},
		DisableAfter: 5,
		prevState:    map[string]model.State{}, subs: map[*Sub]bool{}, changed: make(chan struct{}),
		hooks: map[string]*model.Webhook{}, workers: map[string]context.CancelFunc{},
		queue: map[string][]int64{}, history: map[string][]Delivery{},
	}
}

// Reload adopts the replayed state: the event sequence, recent events,
// webhook subscriptions with their cursors, and the last known states that
// later changes are compared with.
func (b *Bus) Reload(st *journal.State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append([]*model.CloudEvent(nil), st.Events...)
	if len(b.events) > journal.MaxEvents {
		b.events = b.events[len(b.events)-journal.MaxEvents:]
	}
	b.seq = 0
	if n := len(b.events); n > 0 {
		b.seq = b.events[n-1].Sequence
	}
	b.prevState = map[string]model.State{}
	for id, d := range st.Deployments {
		b.prevState[id] = d.State
	}
	b.prevCircuit = safety.Closed
	if st.Circuit != nil {
		b.prevCircuit = st.Circuit.State
	}
	b.hooks = map[string]*model.Webhook{}
	for id, w := range st.Webhooks {
		cp := *w
		b.hooks[id] = &cp
	}
	b.signal()
	if b.ctx != nil {
		b.syncWorkersLocked()
	}
}

// Start runs webhook delivery until ctx ends.
func (b *Bus) Start(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ctx, b.cancelAll = context.WithCancel(ctx)
	b.syncWorkersLocked()
}

// signal wakes everything waiting for a change (caller holds b.mu).
func (b *Bus) signal() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// ---------------------------------------------------------------- deriving events

type deploymentData struct {
	DeploymentID    string         `json:"deployment_id"`
	Service         string         `json:"service"`
	Version         string         `json:"version"`
	PreviousVersion string         `json:"previous_version"`
	Phase           model.Phase    `json:"phase,omitempty"`
	State           model.State    `json:"state"`
	Verdict         model.Verdict  `json:"verdict,omitempty"`
	Reason          string         `json:"reason,omitempty"`
	ExitCode        int            `json:"exit_code"`
	Targets         []string       `json:"targets,omitempty"`
	Breaches        []model.Breach `json:"breaches,omitempty"`
}

// Observe derives events from a stored journal entry. It is the engine's
// record hook.
func (b *Bus) Observe(en journal.Entry) {
	switch en.Kind {
	case journal.KindDeployment, journal.KindCircuit, journal.KindAudit:
	default:
		return // includes the bus's own entries: never re-enter
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch en.Kind {
	case journal.KindDeployment:
		if en.Deployment != nil {
			b.deploymentChanged(en.Deployment)
		}
	case journal.KindCircuit:
		if c := en.Circuit; c != nil && c.State != b.prevCircuit {
			b.prevCircuit = c.State
			typ := map[safety.CircuitStateName]string{safety.Open: model.EvCircuitOpened, safety.HalfOpen: model.EvCircuitHalfOpened, safety.Closed: model.EvCircuitClosed}[c.State]
			b.publishLocked(typ, "circuit", "", map[string]any{"state": c.State, "reason": c.Reason})
		}
	case journal.KindAudit:
		if en.Action == "escalation.approve" {
			b.publishLocked(model.EvApprovalDecided, en.DeployID, en.Service,
				map[string]any{"deployment_id": en.DeployID, "service": en.Service, "approved_by": en.Actor, "comment": en.Reason})
		}
	}
}

func (b *Bus) deploymentChanged(d *model.Deployment) {
	prev, known := b.prevState[d.ID]
	b.prevState[d.ID] = d.State
	if known && prev == d.State {
		return
	}
	data := deploymentData{DeploymentID: d.ID, Service: d.Service, Version: d.Version, PreviousVersion: d.PreviousVersion,
		Phase: d.Phase, State: d.State, Verdict: d.Verdict, Reason: d.Reason, ExitCode: model.ExitCode(d), Targets: d.Targets, Breaches: d.Breaches}
	emit := func(typ string) { b.publishLocked(typ, d.ID, d.Service, data) }
	if !known && d.State == model.StatePending {
		emit(model.EvDeploymentCreated)
		return
	}
	fromObserving := prev == model.StateObserving
	if fromObserving && d.State != model.StateObserving {
		switch {
		case d.State == model.StateAborted:
			emit(model.EvObservationAborted)
		case d.Verdict == model.VerdictPass:
			emit(model.EvObservationPassed)
		case d.Verdict == model.VerdictFail:
			emit(model.EvObservationFailed)
		default:
			emit(model.EvObservationHeld)
		}
	}
	switch d.State {
	case model.StateObserving:
		emit(model.EvObservationStarted)
	case model.StateSucceeded:
		if !fromObserving {
			emit(model.EvDeploymentMarkedGood)
		}
	case model.StateHeld:
		if !fromObserving {
			emit(model.EvObservationHeld)
		}
	case model.StateAborted:
		if !fromObserving {
			emit(model.EvObservationAborted)
		}
	case model.StateRollingBack:
		emit(model.EvRollbackStarted)
	case model.StateRolledBack:
		emit(model.EvRollbackCompleted)
	case model.StateRollbackFailed:
		emit(model.EvRollbackFailed)
	case model.StateAwaitApproval:
		emit(model.EvApprovalRequested)
	}
}

// Emit publishes an event that does not come from a journal entry (agent
// lost, ping). It does nothing on a node that is not the active leader.
func (b *Bus) Emit(typ, subject, service string, data any) *model.CloudEvent {
	if b.d.Active != nil && !b.d.Active() {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.publishLocked(typ, subject, service, data)
}

func (b *Bus) publishLocked(typ, subject, service string, data any) *model.CloudEvent {
	raw, _ := json.Marshal(data)
	b.seq++
	ev := &model.CloudEvent{SpecVersion: "1.0", ID: strconv.FormatInt(b.seq, 10), Source: b.d.Source, Type: typ, Subject: subject,
		Time: time.Now().UTC(), DataContentType: "application/json", Sequence: b.seq, Service: service, Data: raw}
	if service != "" && b.d.TeamOf != nil {
		ev.Team = b.d.TeamOf(service)
	}
	b.d.Record(journal.Entry{Kind: journal.KindEvent, Time: ev.Time, Service: service, Event: ev})
	b.events = append(b.events, ev)
	if len(b.events) > journal.MaxEvents+journal.MaxEvents/10 {
		b.events = append([]*model.CloudEvent(nil), b.events[len(b.events)-journal.MaxEvents:]...)
	}
	for s := range b.subs {
		if s.match(ev) {
			select {
			case s.ch <- ev:
			default: // too slow: drop the stream; the client resumes with Last-Event-ID
				delete(b.subs, s)
				close(s.ch)
			}
		}
	}
	b.signal()
	return ev
}

// ---------------------------------------------------------------- reading and streaming

// Filter selects events.
type Filter struct {
	Types []string
	// Visible decides whether the caller may see an event (scope check).
	Visible func(ev *model.CloudEvent) bool
}

func (f Filter) match(ev *model.CloudEvent) bool {
	if len(f.Types) > 0 && !slices.Contains(f.Types, ev.Type) {
		return false
	}
	return f.Visible == nil || f.Visible(ev)
}

// Since returns stored events after sequence after that pass f, and whether
// events between after and the oldest kept one were already discarded.
func (b *Bus) Since(after int64, f Filter, limit int) (out []*model.CloudEvent, gap bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.events) > 0 && after < b.events[0].Sequence-1 && after > 0 {
		gap = true
	}
	for _, ev := range b.events {
		if ev.Sequence > after && f.match(ev) {
			out = append(out, ev)
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out, gap
}

// Sub is a live stream subscription.
type Sub struct {
	f  Filter
	ch chan *model.CloudEvent
}

func (s *Sub) match(ev *model.CloudEvent) bool { return s.f.match(ev) }

// C delivers events; it is closed if the subscriber falls too far behind.
func (s *Sub) C() <-chan *model.CloudEvent { return s.ch }

// Subscribe atomically returns the stored events after `after` and a
// subscription for everything later, so nothing falls in between.
func (b *Bus) Subscribe(after int64, f Filter) ([]*model.CloudEvent, *Sub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var backlog []*model.CloudEvent
	for _, ev := range b.events {
		if ev.Sequence > after && f.match(ev) {
			backlog = append(backlog, ev)
		}
	}
	s := &Sub{f: f, ch: make(chan *model.CloudEvent, 256)}
	b.subs[s] = true
	return backlog, s
}

// Unsubscribe ends a stream.
func (b *Bus) Unsubscribe(s *Sub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[s] {
		delete(b.subs, s)
		close(s.ch)
	}
}

// Seq is the latest sequence number.
func (b *Bus) Seq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

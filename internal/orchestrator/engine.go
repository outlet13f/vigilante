// Package orchestrator drives a deployment through its phases:
//
//	prepare (checkpoints) -> baseline -> canary -> rolling -> full
//
// For each phase it starts the probe collectors, runs the decision engine and,
// on FAIL, executes the service's rollback plan under the safety guards.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/decision"
	"vigilante/internal/executor"
	"vigilante/internal/itsm"
	"vigilante/internal/journal"
	"vigilante/internal/metrics"
	"vigilante/internal/model"
	"vigilante/internal/notify"
	"vigilante/internal/observer"
	"vigilante/internal/probe"
	"vigilante/internal/rules"
	"vigilante/internal/safety"
	"vigilante/internal/store"
	"vigilante/internal/telemetry"
	"vigilante/internal/tmpl"
	"vigilante/internal/transport"
)

type Options struct {
	DryRun  bool
	Log     *slog.Logger
	Runners func(target string) (transport.Runner, error) // override (tests)
	// TrafficHTTP overrides the HTTP client of API-driven traffic controllers (tests).
	TrafficHTTP *http.Client
	// Store overrides the state store from server.state (tests, HA wiring).
	// The engine closes only a store it opened itself.
	Store store.Store
	// Owner names this process in lease records (default host/pid).
	Owner string
}

type Engine struct {
	Cfg     *config.Config
	Store   *metrics.Store
	Journal store.Store
	Breaker *safety.Breaker
	Guard   *safety.Guard
	// Observer judges whether this process's own measurements can be trusted.
	Observer *observer.Guard
	Notify   *notify.Notifier
	ITSM     *itsm.ServiceNow // nil without itsm.servicenow
	Log      *slog.Logger
	DryRun   bool

	// OnRecord sees every entry after it is durably stored (SIEM export).
	OnRecord func(journal.Entry)
	// hooks also see every stored entry (event bus); reloadHooks see the
	// state after every Reload (HA election).
	// pending holds writes the store refused while unreachable, in order.
	pendMu   sync.Mutex
	pending  []journal.Entry
	flushing bool
	closing  bool

	hookMu      sync.RWMutex
	hooks       []func(journal.Entry)
	reloadHooks []func(*journal.State)
	loaded      *journal.State

	transport   *transport.Manager
	runners     func(string) (transport.Runner, error)
	trafficHTTP *http.Client
	ownsStore   bool
	owner       string
	// active is false on an HA follower or a demoted leader: no new phases
	// or rollback steps start, and fenced writes have already been refused.
	active atomic.Bool

	mu          sync.Mutex
	deployments map[string]*model.Deployment
	stepsDone   map[string]map[string]int
	baselines   map[string]*rules.Snapshot
	cancels     map[string]context.CancelFunc
	lastEval    map[string]decision.Evaluation
	inflight    []*model.Deployment
	operations  map[string]*model.Operation
	liveOps     map[string]bool // operations this node is running
	idem        map[string]*model.IdemRecord
	freezes     map[string]*model.Freeze
	clients     map[string]*model.APIClient
	clientMu    sync.Mutex // orders client snapshots
	tokens      map[string]*model.AccessToken
}

// ErrInactive is returned when this node may not act (HA follower or demoted leader).
var ErrInactive = errors.New("this node is not the active leader")

// New opens the state store, replays it and wires every component.
func New(cfg *config.Config, opt Options) (*Engine, error) {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	st := opt.Store
	owns := false
	if st == nil {
		var err error
		if st, err = store.Open(context.Background(), cfg); err != nil {
			return nil, err
		}
		owns = true
	}
	e := &Engine{
		Cfg:       cfg,
		Store:     metrics.NewStore(30 * time.Minute),
		Journal:   st,
		Notify:    newNotifier(cfg, log),
		Log:       log,
		DryRun:    opt.DryRun || cfg.Server.DryRun,
		transport: transport.NewManager(cfg),
		ownsStore: owns,
		owner:     opt.Owner,
		baselines: map[string]*rules.Snapshot{},
		cancels:   map[string]context.CancelFunc{},
		lastEval:  map[string]decision.Evaluation{},
	}
	e.Observer = observer.New(cfg, log)
	if sn := cfg.ITSM.ServiceNow; sn != nil {
		e.ITSM = itsm.NewServiceNow(*sn, cfg.Credentials)
	}
	if e.owner == "" {
		host, _ := os.Hostname()
		e.owner = fmt.Sprintf("%s/%d", host, os.Getpid())
	}
	e.runners = e.transport.ForTarget
	e.trafficHTTP = opt.TrafficHTTP
	if opt.Runners != nil {
		e.runners = opt.Runners
	}
	e.active.Store(true)
	if err := e.Reload(context.Background()); err != nil {
		if owns {
			st.Close()
		}
		return nil, err
	}
	return e, nil
}

// Reload rebuilds in-memory state from the store: deployments, rollback
// progress, circuit breaker and flapping history. A node calls it when it
// becomes HA leader, because the previous leader may have written since.
func (e *Engine) Reload(ctx context.Context) error {
	st, err := e.Journal.Load(ctx)
	if err != nil {
		return fmt.Errorf("load state from %s: %w", e.Journal.Describe(), err)
	}
	if st.Corrupt > 0 {
		e.Log.Warn("state contained unreadable entries (skipped)", "count", st.Corrupt)
	}
	breaker := safety.NewBreaker(e.Cfg.Safety.CircuitBreaker, st.Circuit)
	breaker.Persist = func(s safety.CircuitState) {
		e.record(journal.Entry{Kind: journal.KindCircuit, Circuit: &s})
	}
	guard := safety.NewGuard(e.Cfg.Safety.Flapping, st.Rollbacks)
	guard.Leases, guard.Owner = e.Journal, e.owner
	guard.LeaseWait = e.Cfg.Safety.RollbackLease.Wait
	guard.ProceedUnleased = e.Cfg.Safety.RollbackLease.OnUnavailable != "fail"
	guard.Unleased, guard.Conflict = e.rollbackUnleased, e.rollbackLeaseConflict
	e.mu.Lock()
	e.deployments, e.stepsDone, e.inflight = st.Deployments, st.StepsDone, st.InFlight()
	e.operations, e.idem = st.Operations, st.Idempotency
	e.clients, e.tokens = st.Clients, st.Tokens
	e.freezes = st.Freezes
	e.Breaker, e.Guard = breaker, guard
	e.mu.Unlock()
	e.hookMu.Lock()
	e.loaded = st
	reload := e.reloadHooks
	e.hookMu.Unlock()
	for _, h := range reload {
		h(st)
	}
	return nil
}

// AddRecordHook registers fn to see every entry after it is stored.
func (e *Engine) AddRecordHook(fn func(journal.Entry)) {
	e.hookMu.Lock()
	e.hooks = append(e.hooks, fn)
	e.hookMu.Unlock()
}

// AddReloadHook registers fn to receive the replayed state after every
// Reload; it is also called at once with the current state.
func (e *Engine) AddReloadHook(fn func(*journal.State)) {
	e.hookMu.Lock()
	e.reloadHooks = append(e.reloadHooks, fn)
	st := e.loaded
	e.hookMu.Unlock()
	if st != nil {
		fn(st)
	}
}

// Record stores an entry (for components outside the engine, such as the
// event bus). Fenced writes stand the node down like any other.
func (e *Engine) Record(en journal.Entry) { e.record(en) }

// SetActive switches this node between acting (leader / single node) and
// standing by (HA follower or demoted). Deactivating cancels observations.
func (e *Engine) SetActive(on bool) {
	e.active.Store(on)
	if !on {
		e.mu.Lock()
		for _, cancel := range e.cancels {
			cancel()
		}
		e.mu.Unlock()
	}
}

func (e *Engine) Active() bool { return e.active.Load() }

// Owner is this process's identity in lease records.
func (e *Engine) Owner() string { return e.owner }

// Audit records who did what. Callers fill Actor, Source, Action and the
// subject (Service / DeployID); the store chains it like every other entry.
func (e *Engine) Audit(en journal.Entry) {
	en.Kind = journal.KindAudit
	if en.Time.IsZero() {
		en.Time = time.Now()
	}
	e.record(en)
}

// record appends to the store. A fenced write means another node is leader
// now: this node stops acting at once.
// Writes that fail while the state store is unreachable are kept in order
// and written when it is back (write-behind), so an outage loses no
// decision, rollback step or audit record and the hash chain stays in
// order. Decisions and rollbacks go on meanwhile: the store is not on the
// rollback path. What is queued lives in memory only; a crash during the
// outage loses it (the deployment snapshot written after recovery carries
// the state again).
const (
	appendTimeout   = 5 * time.Second
	maxPendingWrite = 100_000
)

func (e *Engine) record(en journal.Entry) {
	if en.Time.IsZero() {
		en.Time = time.Now()
	}
	if en.Actor == "" {
		en.Actor, en.Source = "system", "system"
	}
	e.pendMu.Lock()
	if len(e.pending) > 0 { // keep order: queue behind what is waiting
		e.queue(en)
		e.pendMu.Unlock()
		return
	}
	err := e.appendOne(en)
	if err != nil && !errors.Is(err, store.ErrFenced) {
		telemetry.StoreErrors.Inc("error")
		e.Log.Error("state write failed; queued until the store is back", "err", err, "store", e.Journal.Describe())
		e.queue(en)
	}
	e.pendMu.Unlock()
	// Hooks run unlocked: they may record entries themselves (event bus).
	switch {
	case err == nil:
		e.stored(en)
	case errors.Is(err, store.ErrFenced):
		e.fenced()
	}
}

func (e *Engine) appendOne(en journal.Entry) error {
	backend := e.Cfg.Server.State.Backend
	if backend == "" {
		backend = "file"
	}
	ctx, cancel := context.WithTimeout(context.Background(), appendTimeout)
	defer cancel()
	started := time.Now()
	err := e.Journal.Append(ctx, en)
	telemetry.StoreAppend.Since(started, backend)
	return err
}

// stored runs the hooks of an entry that reached the store.
func (e *Engine) stored(en journal.Entry) {
	if e.OnRecord != nil {
		e.OnRecord(en)
	}
	e.hookMu.RLock()
	hooks := e.hooks
	e.hookMu.RUnlock()
	for _, h := range hooks {
		h(en)
	}
}

func (e *Engine) fenced() {
	telemetry.StoreErrors.Inc("fenced")
	if e.active.Load() {
		e.Log.Error("leadership lost: state writes are fenced, standing down")
	}
	e.SetActive(false)
}

// queue holds an entry for the flusher (caller holds pendMu).
func (e *Engine) queue(en journal.Entry) {
	if len(e.pending) >= maxPendingWrite {
		telemetry.StoreErrors.Inc("dropped")
		return
	}
	e.pending = append(e.pending, en)
	telemetry.StorePending.Set(float64(len(e.pending)))
	if !e.flushing {
		e.flushing = true
		go e.flush()
	}
}

// flush writes queued entries in order, retrying until the store answers.
func (e *Engine) flush() {
	wait := 200 * time.Millisecond
	for {
		e.pendMu.Lock()
		if len(e.pending) == 0 || e.closing {
			e.flushing = false
			e.pendMu.Unlock()
			return
		}
		en := e.pending[0]
		err := e.appendOne(en)
		switch {
		case err == nil:
			e.pending = e.pending[1:]
			telemetry.StorePending.Set(float64(len(e.pending)))
			if len(e.pending) == 0 {
				e.Log.Info("state store is back: queued writes stored")
			}
			wait = 200 * time.Millisecond
			e.pendMu.Unlock()
			e.stored(en)
			continue
		case errors.Is(err, store.ErrFenced):
			// Another node leads now: what we queued is no longer ours to write.
			telemetry.StoreErrors.Add(float64(len(e.pending)), "fenced")
			e.pending = nil
			telemetry.StorePending.Set(0)
			e.flushing = false
			e.pendMu.Unlock()
			e.fenced()
			return
		}
		e.pendMu.Unlock()
		time.Sleep(wait)
		wait = min(wait*2, 5*time.Second)
	}
}

// PendingWrites reports entries waiting for the state store.
func (e *Engine) PendingWrites() int {
	e.pendMu.Lock()
	defer e.pendMu.Unlock()
	return len(e.pending)
}

// Flush waits up to d for queued writes to reach the store.
func (e *Engine) Flush(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for e.PendingWrites() > 0 {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

func (e *Engine) Close() {
	if n := e.PendingWrites(); n > 0 && !e.Flush(10*time.Second) {
		e.Log.Error("state writes still queued at shutdown are lost", "count", e.PendingWrites())
	}
	e.pendMu.Lock()
	e.closing = true
	e.pendMu.Unlock()
	e.transport.Close()
	if e.ownsStore {
		e.Journal.Close()
	}
}

// persist journals a snapshot of the deployment (caller must not hold e.mu).
func (e *Engine) persist(d *model.Deployment) {
	e.mu.Lock()
	cp := *d
	cp.Events = append([]model.Event(nil), d.Events...)
	cp.Breaches = append([]model.Breach(nil), d.Breaches...)
	e.deployments[d.ID] = d
	e.mu.Unlock()
	e.record(journal.Entry{Kind: journal.KindDeployment, Deployment: &cp})
}

func (e *Engine) event(d *model.Deployment, kind, msg string, args ...any) {
	e.mu.Lock()
	d.AddEvent(kind, msg)
	e.mu.Unlock()
	e.Log.Info(msg, append([]any{"deployment", d.ID, "event", kind}, args...)...)
}

// rollbackUnleased reports a rollback that goes ahead without the
// cross-process service lease because the state store is unreachable
// (safety.rollback_lease.on_unavailable: proceed).
func (e *Engine) rollbackUnleased(service string, err error) {
	msg := fmt.Sprintf("state store unreachable (%v): rolling back under this process's lock only; another process could act on %s at the same time", err, service)
	e.leaseNotice(service, "lease.unavailable", "Rollback without the service lease", msg)
}

// rollbackLeaseConflict reports that the store came back during an unleased
// rollback and another process holds the service lease.
func (e *Engine) rollbackLeaseConflict(service, holder string) {
	msg := fmt.Sprintf("state store is back and %s holds the rollback lease for %s: two processes may be acting on it; check its targets", holder, service)
	e.leaseNotice(service, "lease.conflict", "Concurrent rollback suspected", msg)
}

func (e *Engine) leaseNotice(service, action, title, msg string) {
	var d *model.Deployment
	e.mu.Lock()
	for _, x := range e.deployments {
		if x.Service == service && !x.State.Terminal() && (d == nil || x.CreatedAt.After(d.CreatedAt)) {
			d = x
		}
	}
	e.mu.Unlock()
	en := journal.Entry{Actor: "system", Source: "system", Action: action, Service: service, Reason: msg}
	if d != nil {
		en.DeployID = d.ID
		e.event(d, "safety", msg)
	} else {
		e.Log.Warn(msg, "service", service)
	}
	e.Audit(en)
	e.Notify.Send(context.Background(), notify.Message{Level: notify.Warning, Title: fmt.Sprintf("%s: %s", service, title), Text: msg, Deployment: d})
}

func (e *Engine) setState(d *model.Deployment, s model.State, reason string) {
	e.mu.Lock()
	d.State, d.Reason = s, reason
	d.AddEvent("state", string(s)+": "+reason)
	e.mu.Unlock()
	e.Log.Info("deployment state", "deployment", d.ID, "state", s, "reason", reason)
	e.persist(d)
}

// Deployment returns a copy of a deployment.
func (e *Engine) Deployment(id string) (*model.Deployment, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, ok := e.deployments[id]
	if !ok {
		return nil, false
	}
	cp := *d
	cp.Events = append([]model.Event(nil), d.Events...)
	cp.Breaches = append([]model.Breach(nil), d.Breaches...)
	return &cp, true
}

// Live returns the engine-owned deployment (for Watch / Rollback); nil if unknown.
func (e *Engine) Live(id string) *model.Deployment {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deployments[id]
}

// Annotate records a note on the deployment timeline (journalled), e.g. where
// an auto-filled input came from.
func (e *Engine) Annotate(d *model.Deployment, kind, msg string) {
	e.event(d, kind, msg)
	e.persist(d)
}

// SetCreatedBy records who created the deployment (first caller wins).
func (e *Engine) SetCreatedBy(d *model.Deployment, actor string) {
	e.mu.Lock()
	set := d.CreatedBy == "" && actor != ""
	if set {
		d.CreatedBy = actor
	}
	e.mu.Unlock()
	if set {
		e.persist(d)
	}
}

// ErrFeedback: the outcome contradicts what the deployment's verdicts were.
var ErrFeedback = errors.New("invalid feedback")

// SetFeedback records a person's assessment of a deployment's verdict (it
// replaces an earlier one) and audits it.
func (e *Engine) SetFeedback(id string, fb model.Feedback, source string) (*model.Deployment, error) {
	e.mu.Lock()
	d, ok := e.deployments[id]
	if !ok {
		e.mu.Unlock()
		return nil, fmt.Errorf("deployment %s not found", id)
	}
	failed := d.Failed()
	e.mu.Unlock()
	switch fb.Outcome {
	case model.FeedbackCorrect, model.FeedbackUnclear:
	case model.FeedbackFalsePositive:
		if !failed {
			return nil, fmt.Errorf("%w: false_positive means a FAIL verdict on a healthy deployment, and %s had no FAIL verdict", ErrFeedback, id)
		}
	case model.FeedbackFalseNegative:
		if failed {
			return nil, fmt.Errorf("%w: false_negative means a harmful deployment was not failed, and %s was failed", ErrFeedback, id)
		}
	default:
		return nil, fmt.Errorf("%w: outcome must be correct, false_positive, false_negative or unclear", ErrFeedback)
	}
	if fb.At.IsZero() {
		fb.At = time.Now()
	}
	e.mu.Lock()
	d.Feedback = &fb
	e.mu.Unlock()
	e.event(d, "feedback", fmt.Sprintf("verdict assessed as %s by %s", fb.Outcome, fb.By))
	e.persist(d)
	e.Audit(journal.Entry{Actor: fb.By, Source: source, Action: "deployment.feedback", Service: d.Service, DeployID: d.ID,
		Reason: strings.TrimSpace(fb.Outcome + " " + fb.Note), Ticket: fb.Incident})
	cp, _ := e.Deployment(id)
	return cp, nil
}

// MarkBlocked records that a phase could not start (e.g. circuit open).
func (e *Engine) MarkBlocked(d *model.Deployment, err error) {
	e.setState(d, model.StateHeld, err.Error())
}

func (e *Engine) Deployments() []*model.Deployment {
	e.mu.Lock()
	ids := make([]string, 0, len(e.deployments))
	for id := range e.deployments {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	sort.Strings(ids)
	out := make([]*model.Deployment, 0, len(ids))
	for _, id := range ids {
		if d, ok := e.Deployment(id); ok {
			out = append(out, d)
		}
	}
	return out
}

func (e *Engine) LastEvaluation(id string) (decision.Evaluation, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ev, ok := e.lastEval[id]
	return ev, ok
}

// SetBaseline stores a baseline snapshot for subsequent phases of a service.
func (e *Engine) SetBaseline(s *rules.Snapshot) {
	e.mu.Lock()
	e.baselines[s.Service] = s
	e.mu.Unlock()
}

// Create registers (or re-opens) a deployment.
func (e *Engine) Create(id, service, version, previous string) (*model.Deployment, error) {
	if _, ok := e.Cfg.Service(service); !ok {
		return nil, fmt.Errorf("unknown service %q", service)
	}
	if id == "" {
		id = fmt.Sprintf("%s-%s-%d", service, version, time.Now().Unix())
	}
	e.mu.Lock()
	if d, ok := e.deployments[id]; ok {
		e.mu.Unlock()
		if d.Service != service {
			return nil, fmt.Errorf("deployment %s exists for service %s", id, d.Service)
		}
		if version != "" && d.Version != version {
			return nil, fmt.Errorf("deployment %s exists with version %s", id, d.Version)
		}
		return d, nil
	}
	now := time.Now()
	d := &model.Deployment{ID: id, Service: service, Version: version, PreviousVersion: previous,
		State: model.StatePending, Verdict: model.VerdictPending, CreatedAt: now, UpdatedAt: now,
		Checkpoints: map[string]map[string]string{}, DryRun: e.DryRun}
	e.deployments[id] = d
	e.mu.Unlock()
	e.event(d, "created", fmt.Sprintf("deployment created: %s %s -> %s", service, previous, version))
	e.persist(d)
	return d, nil
}

// LastGoodVersion returns the version of the service's most recent SUCCEEDED
// deployment (a full phase that passed, or one registered with MarkGood) and
// that deployment's ID. ok is false when the journal has no such record.
func (e *Engine) LastGoodVersion(service string) (version, fromID string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var best *model.Deployment
	for _, d := range e.deployments {
		if d.Service != service || d.State != model.StateSucceeded || d.Version == "" {
			continue
		}
		if best == nil || d.UpdatedAt.After(best.UpdatedAt) {
			best = d
		}
	}
	if best == nil {
		return "", "", false
	}
	return best.Version, best.ID, true
}

// MarkGood records version as the service's known-good version without a
// deployment, so automatic --previous works from the first pipeline run.
func (e *Engine) MarkGood(service, version, reason string) (*model.Deployment, error) {
	if version == "" {
		return nil, errors.New("version is required")
	}
	if _, ok := e.Cfg.Service(service); !ok {
		return nil, fmt.Errorf("unknown service %q", service)
	}
	if reason == "" {
		reason = "marked as known-good manually"
	}
	// One record, already SUCCEEDED: it is a reference point, not a deployment
	// that was created and then observed.
	now := time.Now()
	d := &model.Deployment{ID: fmt.Sprintf("mark-good-%s-%s-%d", service, version, now.UnixNano()), Service: service, Version: version,
		State: model.StateSucceeded, Verdict: model.VerdictPending, Reason: reason, CreatedAt: now, UpdatedAt: now,
		Checkpoints: map[string]map[string]string{}}
	d.AddEvent("state", string(model.StateSucceeded)+": "+reason)
	e.persist(d)
	e.Log.Info("deployment state", "deployment", d.ID, "state", d.State, "reason", reason)
	return d, nil
}

func (e *Engine) templateData(d *model.Deployment, t config.Target) tmpl.Data {
	data := tmpl.ForTarget(t)
	data.Service, data.Version, data.PreviousVersion, data.DeploymentID, data.Phase = d.Service, d.Version, d.PreviousVersion, d.ID, string(d.Phase)
	e.mu.Lock()
	for k, v := range d.Checkpoints[t.Name] {
		data.Checkpoint[k] = v
	}
	e.mu.Unlock()
	return data
}

func (e *Engine) runContext(d *model.Deployment, t config.Target) *executor.RunContext {
	r, err := e.runners(t.Name)
	if err != nil {
		r = nil
	}
	data := e.templateData(d, t)
	return &executor.RunContext{
		Target: t, Data: data, Runner: r, Runners: e.runners, Creds: e.Cfg.Credentials,
		Checkpoint: data.Checkpoint, DryRun: e.DryRun,
		Log: e.Log.With("deployment", d.ID, "target", t.Name),
	}
}

// Prepare captures pre-deployment checkpoints (current symlink, current image,
// VM snapshots) on every service target, for the primary executor and all
// escalation executors that support it.
func (e *Engine) Prepare(ctx context.Context, d *model.Deployment) error {
	svc, _ := e.Cfg.Service(d.Service)
	names := []string{svc.Rollback.Executor}
	for _, esc := range svc.Rollback.Escalation {
		names = append(names, esc.Executor)
	}
	var errs []error
	for _, tn := range svc.Targets {
		t, _ := e.Cfg.Target(tn)
		for _, name := range names {
			ex, err := executor.New(name, e.Cfg.Executors[name])
			if err != nil {
				return err
			}
			p, ok := ex.(executor.Preparer)
			if !ok {
				continue
			}
			cp, err := p.Prepare(ctx, e.runContext(d, *t))
			if err != nil {
				errs = append(errs, fmt.Errorf("prepare %s on %s: %w", name, tn, err))
				continue
			}
			e.mu.Lock()
			if d.Checkpoints[tn] == nil {
				d.Checkpoints[tn] = map[string]string{}
			}
			for k, v := range cp {
				d.Checkpoints[tn][k] = v
			}
			e.mu.Unlock()
		}
	}
	e.event(d, "prepared", fmt.Sprintf("checkpoints captured on %d targets", len(svc.Targets)))
	e.persist(d)
	return errors.Join(errs...)
}

func (e *Engine) jobs(svc *config.Service, targets []string) []probe.Job {
	var jobs []probe.Job
	for _, tn := range targets {
		t, ok := e.Cfg.Target(tn)
		if !ok {
			continue
		}
		for _, p := range svc.Probes {
			jobs = append(jobs, probe.Job{Target: *t, Spec: p})
		}
	}
	return jobs
}

func (e *Engine) collector(d *model.Deployment) *probe.Collector {
	return &probe.Collector{
		Runners: e.runners,
		Sink: func(s model.Sample) {
			e.Store.Add(s)
			e.Observer.Observe(s)
		},
		Source: model.SourceCentral,
		Log:    e.Log,
		Data: func(t config.Target) tmpl.Data {
			if d == nil {
				return tmpl.ForTarget(t)
			}
			return e.templateData(d, t)
		},
	}
}

// CaptureBaseline observes the service's current (pre-deploy) behaviour.
func (e *Engine) CaptureBaseline(ctx context.Context, service string, window time.Duration) (*rules.Snapshot, error) {
	svc, ok := e.Cfg.Service(service)
	if !ok {
		return nil, fmt.Errorf("unknown service %q", service)
	}
	if window == 0 {
		window = svc.Baseline.Window
	}
	cctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	e.Log.Info("capturing baseline", "service", service, "window", window, "targets", len(svc.Targets))
	e.collector(nil).Run(cctx, e.jobs(svc, svc.Targets))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	snap := rules.Capture(e.Store, service, svc.Rules, svc.Targets, window, time.Now())
	if svc.Baseline.MinSamples > 0 && snap.Samples < svc.Baseline.MinSamples {
		return snap, fmt.Errorf("baseline has %d samples, need %d", snap.Samples, svc.Baseline.MinSamples)
	}
	e.SetBaseline(snap)
	return snap, nil
}

// ErrBlocked is returned when the safety circuit forbids starting a phase.
var ErrBlocked = errors.New("deployment gate closed")

// Watch observes one phase and rolls back on failure. It returns when the
// phase reaches a verdict and any resulting rollback has finished.
func (e *Engine) Watch(ctx context.Context, d *model.Deployment, phase model.Phase) error {
	svc, _ := e.Cfg.Service(d.Service)
	pc, ok := svc.Phases[string(phase)]
	if !ok {
		return fmt.Errorf("service %s has no phase %q configured", svc.Name, phase)
	}
	if !e.Active() {
		return ErrInactive
	}
	if len(svc.PhaseTargets(string(phase))) == 0 {
		return fmt.Errorf("service %s phase %s resolves to no targets", svc.Name, phase)
	}
	if err := e.Gate(d); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.cancels[d.ID] = cancel
	d.Phase = phase
	d.Targets = svc.PhaseTargets(string(phase))
	d.Verdict = model.VerdictPending
	d.Breaches = nil
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		delete(e.cancels, d.ID)
		e.mu.Unlock()
	}()
	controls := svc.Controls(string(phase))
	e.setState(d, model.StateObserving, fmt.Sprintf("phase %s: observing %v for %s (warmup %s, controls %v)", phase, d.Targets, pc.ObservationWindow, pc.Warmup, controls))

	releaseObserver := e.Observer.Start()
	defer releaseObserver()
	collectCtx, stopCollect := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.collector(d).Run(collectCtx, e.jobs(svc, append(append([]string(nil), d.Targets...), controls...)))
	}()

	e.mu.Lock()
	snap := e.baselines[svc.Name]
	e.mu.Unlock()
	baseline := rules.Chain{&rules.Control{Store: e.Store, Targets: controls}, snap}
	eng := decision.New(decision.Phase{
		Name: phase, Cfg: pc, Rules: svc.Rules, Deployed: d.Targets, Controls: controls,
		Quorum: e.Cfg.Safety.ObserverQuorum,
	}, e.Store, baseline)
	if e.Observer.Enabled() {
		eng.Observer = e.Observer
	}
	eng.OnEval = func(ev decision.Evaluation) {
		e.mu.Lock()
		e.lastEval[d.ID] = ev
		e.mu.Unlock()
		e.Log.Debug("evaluation", "deployment", d.ID, "fail", len(ev.Fail), "hold", len(ev.Hold), "known", ev.Known, "unknown", ev.Unknown)
	}
	out := eng.Run(ctx)
	stopCollect()
	wg.Wait()

	for _, w := range out.Warnings {
		e.Notify.Send(ctx, notify.Message{Level: notify.Warning, Title: fmt.Sprintf("%s %s: warning rule %s", d.Service, phase, w.Rule), Text: w.Target + ": " + w.Detail, Deployment: d})
	}
	e.mu.Lock()
	d.Verdict, d.Breaches = out.Verdict, out.Breaches
	e.mu.Unlock()
	telemetry.Verdicts.Inc(svc.Name, string(phase), string(out.Verdict))

	if ctx.Err() != nil && out.Verdict == model.VerdictHold {
		e.setState(d, model.StateAborted, out.Reason)
		return nil
	}
	switch out.Verdict {
	case model.VerdictPass:
		if phase == model.PhaseFull {
			e.setState(d, model.StateSucceeded, out.Reason)
		} else {
			e.setState(d, model.StatePromoted, out.Reason)
		}
	case model.VerdictHold, model.VerdictInconclusive:
		e.setState(d, model.StateHeld, out.Reason)
		e.Notify.Send(ctx, notify.Message{Level: notify.Warning, Title: fmt.Sprintf("%s %s HELD — human decision needed", d.Service, phase), Text: out.Reason, Deployment: d})
	case model.VerdictFail:
		e.event(d, "verdict", "FAIL: "+out.Reason)
		if svc.Rollback.RollbackMode() == "approve" {
			e.requestApproval(context.WithoutCancel(ctx), d, svc, out.Reason, out.EndedAt)
			return nil
		}
		e.Rollback(context.WithoutCancel(ctx), d, RollbackOptions{Reason: out.Reason, DetectedAt: out.EndedAt})
	}
	return nil
}

// Resume continues rollbacks that were in flight when the previous process
// died. Completed steps (journalled) are skipped; the rest are idempotent.
func (e *Engine) Resume(ctx context.Context) int {
	e.mu.Lock()
	pending := e.inflight
	e.inflight = nil
	e.mu.Unlock()
	for _, d := range pending {
		e.event(d, "resume", "resuming interrupted rollback after restart")
		// Manual: the guard history already contains this rollback's start.
		_ = e.Rollback(ctx, d, RollbackOptions{Reason: "resumed after orchestrator restart: " + d.Reason, Manual: true})
	}
	return len(pending)
}

// Abort cancels an in-progress observation.
func (e *Engine) Abort(id string) bool {
	e.mu.Lock()
	cancel, ok := e.cancels[id]
	e.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// LoadBaselineFile is a convenience for the CLI.
func (e *Engine) LoadBaselineFile(path string) error {
	if path == "" {
		return nil
	}
	s, err := rules.LoadSnapshot(path)
	if err != nil {
		return err
	}
	if _, ok := e.Cfg.Service(s.Service); !ok {
		return fmt.Errorf("baseline file is for unknown service %q", s.Service)
	}
	e.SetBaseline(s)
	return nil
}

// Hostname is used as the agent's default name.
func Hostname() string {
	h, _ := os.Hostname()
	return h
}

func newNotifier(cfg *config.Config, log *slog.Logger) *notify.Notifier {
	n := notify.New(cfg.Notify, log)
	n.TeamOf = func(service string) string {
		if sv, ok := cfg.Service(service); ok {
			return sv.Team
		}
		return ""
	}
	return n
}

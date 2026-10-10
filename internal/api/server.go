// Package api exposes the orchestrator over REST for CI/CD systems, deploy
// consoles and push agents.
//
//	POST /v1/deployments                       create {id?, service, version, previous_version, prepare?}
//	GET  /v1/deployments                       list
//	GET  /v1/deployments/{id}                  status (+ last evaluation)
//	POST /v1/deployments/{id}/phases/{phase}   start observing canary|rolling|full (async; ?wait=true blocks)
//	POST /v1/deployments/{id}/rollback         manual rollback {executor?, reason?, targets?}
//	POST /v1/deployments/{id}/approve          approve gated escalation (e.g. VM snapshot restore)
//	POST /v1/deployments/{id}/abort            stop observing
//	POST /v1/baselines/{service}?window=5m     capture a pre-deploy baseline
//	GET  /v1/circuit   POST /v1/circuit/reset   POST /v1/circuit/trip
//	POST /v1/samples                           agent push (JSON array of samples)
//	GET  /v1/agents/{target}/heartbeat         agent heartbeat; returns the active deployment
//	POST /v1/webhooks/{provider}               github | gitlab | jenkins | generic
//	GET  /v1/targets/{target}/metrics          latest values per metric (debug)
//	GET  /v1/whoami                            the caller's identity and grants
//	GET  /v1/audit?since&until&actor&service&action&kind&limit&format=csv   audit records
//	GET  /healthz
//
// Every route but /healthz and signed webhooks requires a bearer token (see
// package auth); each checks the caller's role on the service it touches.
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"vigilante/internal/audit"
	"vigilante/internal/auth"
	"vigilante/internal/console"
	"vigilante/internal/events"
	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/orchestrator"
	"vigilante/internal/telemetry"
)

type Server struct {
	E    *orchestrator.Engine
	Auth *auth.Authenticator
	// WebhookSecret verifies GitHub signatures / GitLab tokens.
	WebhookSecret string

	// HA is set when several nodes share a postgres store; followers forward
	// every API call to the leader. nil = single node.
	HA Leadership
	// HATLS verifies the leader when forwarding to an https advertise URL
	// (server.ha.tls); nil = system defaults.
	HATLS *tls.Config

	ctx      context.Context
	mu       sync.Mutex
	agents   map[string]time.Time
	proxies  map[string]*httputil.ReverseProxy
	inflight map[string]bool // idempotency keys being processed
	limits   *limiter
	bus      *events.Bus
	console  *console.Console

	cancel context.CancelFunc
	bg     sync.WaitGroup // observations, rollbacks and operations started by requests
}

// background runs work that outlives its request; Close waits for it.
func (s *Server) background(f func()) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		f()
	}()
}

// Close cancels background work (observations, rollbacks in progress,
// webhook delivery) and waits up to 30 seconds for it to record its final
// state. The engine and its store stay open; close them afterwards.
func (s *Server) Close() {
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.bg.Wait()
		s.bus.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		s.E.Log.Warn("background work still running at shutdown")
	}
}

// Leadership is what the API needs from the HA elector.
type Leadership interface {
	IsLeader() bool
	Leader() (node, addr string)
}

// forwardedHeader marks a request a follower already forwarded, so a request
// is never bounced between two nodes that disagree during a leader change.
const forwardedHeader = "X-Vigilante-Forwarded-By"

// New builds the API server. With auth.oidc configured it contacts the
// identity provider for its discovery document.
func New(ctx context.Context, e *orchestrator.Engine) (*Server, error) {
	s := &Server{E: e, agents: map[string]time.Time{}, proxies: map[string]*httputil.ReverseProxy{}}
	s.ctx, s.cancel = context.WithCancel(ctx)
	var legacy string
	if env := e.Cfg.Server.AuthTokenEnv; env != "" {
		legacy = os.Getenv(env)
	}
	if env := e.Cfg.Server.WebhookSecret; env != "" {
		s.WebhookSecret = os.Getenv(env)
	}
	a, err := auth.New(ctx, e.Cfg.Auth, legacy)
	if err != nil {
		return nil, err
	}
	s.Auth = a
	if !e.Cfg.Console.Disabled {
		if s.console, err = console.New(ctx, e.Cfg, s.consoleVerify, e.Log); err != nil {
			return nil, err
		}
	}
	s.bus = s.newBus()
	s.background(s.sweepApprovals)
	s.background(s.itsmWorker)
	return s, nil
}

// sweepApprovals applies approval timeouts while this node is the leader.
func (s *Server) sweepApprovals() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-t.C:
			s.E.ExpireApprovals(s.ctx, now)
		}
	}
}

func (s *Server) Handler() http.Handler {
	if s.limits == nil {
		s.limits = newLimiter()
	}
	if s.Auth != nil && s.Auth.Clients == nil {
		s.Auth.Clients = s.clientPrincipal
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		role, leader := "single", ""
		if s.HA != nil {
			role = "follower"
			if s.HA.IsLeader() {
				role = "leader"
			}
			_, leader = s.HA.Leader()
		}
		writeJSON(w, 200, map[string]any{"ok": true, "role": role, "leader": leader, "active": s.E.Active(),
			"circuit": s.E.Breaker.State().State, "dry_run": s.E.DryRun, "store": s.E.Journal.Describe()})
	})
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /metrics", s.metricsHandler())
	mux.HandleFunc("GET /v1/whoami", s.authn(s.whoami))
	mux.HandleFunc("GET /v1/audit", s.authn(s.auditQuery))
	mux.HandleFunc("POST /v1/deployments", s.authn(s.createDeployment))
	mux.HandleFunc("GET /v1/deployments", s.authn(s.listDeployments))
	mux.HandleFunc("GET /v1/deployments/{id}", s.authn(s.getDeployment))
	mux.HandleFunc("POST /v1/deployments/{id}/phases/{phase}", s.authn(s.startPhase))
	mux.HandleFunc("POST /v1/deployments/{id}/rollback", s.authn(s.manualRollback))
	mux.HandleFunc("POST /v1/deployments/{id}/approve", s.authn(s.approve))
	mux.HandleFunc("POST /v1/deployments/{id}/abort", s.authn(s.abort))
	mux.HandleFunc("POST /v1/baselines/{service}", s.authn(s.captureBaseline))
	mux.HandleFunc("GET /v1/circuit", s.authn(s.circuitStatus))
	mux.HandleFunc("POST /v1/circuit/reset", s.authn(s.circuitReset))
	mux.HandleFunc("POST /v1/circuit/trip", s.authn(s.circuitTrip))
	mux.HandleFunc("POST /v1/samples", s.authn(s.ingest))
	mux.HandleFunc("GET /v1/agents/{target}/heartbeat", s.authn(s.heartbeat))
	mux.HandleFunc("POST /v1/webhooks/{provider}", s.webhook) // authenticated by signature/token
	mux.HandleFunc("GET /v1/targets/{target}/metrics", s.authn(s.targetMetrics))
	s.routesV2(mux)
	if s.console != nil {
		s.console.Register(mux)
	}
	return s.instrument(s.forwardToLeader(mux))
}

// consoleVerify accepts a console sign-in only for a user the API itself
// accepts and who holds at least one role.
func (s *Server) consoleVerify(ctx context.Context, raw string) error {
	p, err := s.Auth.AuthenticateToken(ctx, raw)
	if err != nil {
		return err
	}
	if len(p.Bindings) == 0 {
		s.E.Audit(journal.Entry{Actor: p.ID, Source: "ui", Action: "denied", Reason: "console sign-in: no role binding"})
		return fmt.Errorf("signed in as %s, but no auth.role_bindings entry grants a role to this user or their groups", p.ID)
	}
	s.E.Audit(journal.Entry{Actor: p.ID, Source: "ui", Action: "console.sign_in"})
	return nil
}

// forwardToLeader sends every API call except /healthz, /readyz and /metrics to the HA leader when
// this node is a follower, so clients may talk to any node.
func (s *Server) forwardToLeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.HA == nil || s.HA.IsLeader() || local[r.URL.Path] || isConsole(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		node, addr := s.HA.Leader()
		if addr == "" || r.Header.Get(forwardedHeader) != "" {
			w.Header().Set("Retry-After", "2")
			s.fail(w, r, http.StatusServiceUnavailable, "not_leader", errors.New("no leader available yet; retry shortly"))
			return
		}
		s.mu.Lock()
		p, ok := s.proxies[addr]
		if !ok {
			target, err := url.Parse(addr)
			if err != nil {
				s.mu.Unlock()
				s.fail(w, r, http.StatusBadGateway, "not_leader", fmt.Errorf("leader %s advertises an invalid URL %q", node, addr))
				return
			}
			p = httputil.NewSingleHostReverseProxy(target)
			if s.HATLS != nil {
				tr := http.DefaultTransport.(*http.Transport).Clone()
				tr.TLSClientConfig = s.HATLS
				p.Transport = tr
			}
			p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
				w.Header().Set("Retry-After", "2")
				s.fail(w, r, http.StatusBadGateway, "not_leader", fmt.Errorf("leader %s unreachable: %w", node, err))
			}
			s.proxies[addr] = p
		}
		s.mu.Unlock()
		r.Header.Set(forwardedHeader, s.E.Owner())
		p.ServeHTTP(w, r)
	})
}

// authn identifies the caller and stores the principal in the request context.
// Without an Authorization header a console session cookie is accepted;
// changing requests made with it must carry the CSRF token.
func (s *Server) authn(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.console != nil && r.Header.Get("Authorization") == "" {
			if tok, ok, csrfOK := s.console.SessionToken(r); ok {
				if !csrfOK {
					s.fail(w, r, http.StatusForbidden, "forbidden", errors.New("console session: missing or wrong "+console.CSRFHeader+" header"))
					return
				}
				r = r.Clone(context.WithValue(r.Context(), uiKey{}, true))
				r.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		p, err := s.Auth.Authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vigilante"`)
			s.fail(w, r, http.StatusUnauthorized, "unauthenticated", err)
			return
		}
		h(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}
}

type uiKey struct{}

// source is the audit source of a request: "ui" for the console session,
// "api" otherwise.
func source(r *http.Request) string { return sourceOf(r.Context()) }

func sourceOf(ctx context.Context) string {
	if ui, _ := ctx.Value(uiKey{}).(bool); ui {
		return "ui"
	}
	return "api"
}

func isConsole(path string) bool { return path == "/console" || strings.HasPrefix(path, "/console/") }

// svc describes a service for authorization (name + owning team).
func (s *Server) svc(name string) auth.Service {
	if sv, ok := s.E.Cfg.Service(name); ok {
		return auth.Service{Name: sv.Name, Team: sv.Team}
	}
	return auth.Service{Name: name}
}

// targetServices lists the services a target belongs to.
func (s *Server) targetServices(target string) []auth.Service {
	var out []auth.Service
	for _, sv := range s.E.Cfg.Services {
		for _, t := range sv.Targets {
			if t == target {
				out = append(out, auth.Service{Name: sv.Name, Team: sv.Team})
			}
		}
	}
	return out
}

// allow checks the caller's role for action a on svc and writes 403 if not.
func (s *Server) allow(w http.ResponseWriter, r *http.Request, a auth.Action, svc auth.Service) (*auth.Principal, bool) {
	p := auth.FromContext(r.Context())
	if p != nil && p.Can(a, svc) {
		return p, true
	}
	err := fmt.Errorf("forbidden: %s needs role %s on %s", principalID(p), auth.Required(a), describe(svc))
	s.denied(r, svc.Name, err)
	s.fail(w, r, http.StatusForbidden, "forbidden", err)
	return p, false
}

// allowAny passes if the caller may do a on at least one of svcs.
func (s *Server) allowAny(w http.ResponseWriter, r *http.Request, a auth.Action, svcs []auth.Service, what string) bool {
	p := auth.FromContext(r.Context())
	for _, sv := range svcs {
		if p != nil && p.Can(a, sv) {
			return true
		}
	}
	err := fmt.Errorf("forbidden: %s needs role %s on a service using %s", principalID(p), auth.Required(a), what)
	s.denied(r, "", err)
	s.fail(w, r, http.StatusForbidden, "forbidden", err)
	return false
}

// audit records an API action by the caller. X-Change-Ticket, when sent,
// links the record to a change or incident ticket.
func (s *Server) audit(r *http.Request, action, service, deployID, reason string) {
	s.E.Audit(journal.Entry{Actor: principalID(auth.FromContext(r.Context())), Source: source(r), Action: action,
		Service: service, DeployID: deployID, Reason: reason, Ticket: r.Header.Get("X-Change-Ticket")})
}

// denied records a refused request; repeated denials are how probing shows up.
func (s *Server) denied(r *http.Request, service string, err error) {
	s.E.Audit(journal.Entry{Actor: principalID(auth.FromContext(r.Context())), Source: source(r), Action: "denied",
		Service: service, DeployID: r.PathValue("id"), Reason: r.Method + " " + r.URL.Path + ": " + err.Error(),
		Ticket: r.Header.Get("X-Change-Ticket")})
}

// auditQuery returns audit records. Audit spans every service, so it needs a
// viewer grant with scope *.
func (s *Server) auditQuery(w http.ResponseWriter, r *http.Request) {
	if !s.auditAllowed(w, r) {
		return
	}
	q := r.URL.Query()
	f := audit.Filter{Actor: q.Get("actor"), Service: q.Get("service"), Action: q.Get("action"), Limit: 1000}
	for _, k := range []struct {
		name string
		dst  *time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if v := q.Get(k.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeErr(w, 400, fmt.Errorf("%s: use RFC 3339, e.g. 2026-10-01T00:00:00Z", k.name))
				return
			}
			*k.dst = t
		}
	}
	if v := q.Get("kind"); v != "" {
		f.Kinds = map[string]bool{}
		for _, k := range strings.Split(v, ",") {
			f.Kinds[k] = true
		}
	}
	if v := q.Get("limit"); v != "" {
		if _, err := fmt.Sscan(v, &f.Limit); err != nil || f.Limit < 1 || f.Limit > 100000 {
			writeErr(w, 400, errors.New("limit must be 1..100000"))
			return
		}
	}
	entries, err := audit.Query(r.Context(), s.E.Journal, f)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if q.Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="vigilante-audit.csv"`)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"time", "kind", "actor", "source", "action", "service", "deployment_id", "state", "reason", "ticket", "hash"})
		for _, e := range entries {
			svc, dep, state := e.Service, e.DeployID, ""
			if e.Deployment != nil {
				svc, dep, state = e.Deployment.Service, e.Deployment.ID, string(e.Deployment.State)
			}
			if e.Circuit != nil {
				state = string(e.Circuit.State)
			}
			_ = cw.Write([]string{e.Time.UTC().Format(time.RFC3339Nano), e.Kind, e.Actor, e.Source, e.Action, svc, dep, state, e.Reason, e.Ticket, e.Hash})
		}
		cw.Flush()
		return
	}
	writeJSON(w, 200, entries)
}

func principalID(p *auth.Principal) string {
	if p == nil {
		return "caller"
	}
	return p.ID
}

func describe(svc auth.Service) string {
	switch {
	case svc.Name == "":
		return "all services (scope *)"
	case svc.Team != "":
		return "service=" + svc.Name + " (team=" + svc.Team + ")"
	}
	return "service=" + svc.Name
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	grants := make([]string, len(p.Bindings))
	for i, b := range p.Bindings {
		grants[i] = b.String()
	}
	writeJSON(w, 200, map[string]any{"id": p.ID, "kind": p.Kind, "groups": p.Groups, "grants": grants})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

type createReq struct {
	ID              string `json:"id"`
	Service         string `json:"service"`
	Version         string `json:"version"`
	PreviousVersion string `json:"previous_version"`
	Prepare         bool   `json:"prepare"`
	Phase           string `json:"phase"` // optional: start observing immediately
	// FreezeOverride (admins only, checked by the caller) lets a new
	// deployment proceed during a change freeze; it is the reason.
	FreezeOverride string `json:"-"`
	// ChangeTicket is checked against ITSM when a change gate applies.
	ChangeTicket string `json:"-"`
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	p, ok := s.allow(w, r, auth.ActDeploy, s.svc(req.Service))
	if !ok {
		return
	}
	req.ChangeTicket = r.Header.Get("X-Change-Ticket")
	d, err := s.create(r.Context(), p, req)
	if err != nil {
		writeErr(w, gateStatus(err, 400), err)
		return
	}
	s.audit(r, "deployment.create", d.Service, d.ID, fmt.Sprintf("%s %s -> %s", d.Service, d.PreviousVersion, d.Version))
	writeJSON(w, 201, d)
}

func (s *Server) create(ctx context.Context, p *auth.Principal, req createReq) (*model.Deployment, error) {
	if req.Service == "" || req.Version == "" {
		return nil, errors.New("service and version are required")
	}
	existing := req.ID != "" && s.E.Live(req.ID) != nil
	var ticket *model.ChangeTicket
	if !existing {
		if err := s.E.FreezeGate(req.Service, req.FreezeOverride); err != nil {
			return nil, err
		}
		var err error
		if ticket, err = s.E.ChangeGate(ctx, req.Service, req.ChangeTicket); err != nil {
			return nil, err
		}
	}
	var note string
	if req.PreviousVersion == "" && !existing {
		if v, from, ok := s.E.LastGoodVersion(req.Service); ok {
			req.PreviousVersion = v
			note = fmt.Sprintf("auto-filled previous=%s (last good deployment %s)", v, from)
		}
	}
	d, err := s.E.Create(req.ID, req.Service, req.Version, req.PreviousVersion)
	if err != nil {
		return nil, err
	}
	s.E.SetCreatedBy(d, p.ID)
	if note != "" {
		s.E.Annotate(d, "input", note)
	}
	s.E.SetChangeTicket(d, ticket)
	if req.FreezeOverride != "" && !existing {
		if f := s.E.ActiveFreeze(d.Service, time.Now()); f != nil {
			s.E.SetFreezeOverride(d, p.ID, req.FreezeOverride)
			s.E.Audit(journal.Entry{Actor: p.ID, Source: sourceOf(ctx), Action: "freeze.override", Service: d.Service, DeployID: d.ID,
				Reason: f.Name + ": " + req.FreezeOverride})
		}
	}
	if req.Prepare {
		if err := s.E.Prepare(ctx, d); err != nil {
			return d, fmt.Errorf("prepare: %w", err)
		}
	}
	if req.Phase != "" {
		if err := s.launch(d, model.Phase(req.Phase)); err != nil {
			return d, err
		}
	}
	cp, _ := s.E.Deployment(d.ID)
	return cp, nil
}

// lookup finds the deployment in the path and checks the caller may do a on its service.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request, a auth.Action) (*model.Deployment, *auth.Principal, bool) {
	id := r.PathValue("id")
	d := s.E.Live(id)
	if d == nil {
		writeErr(w, 404, fmt.Errorf("deployment %q not found", id))
		return nil, nil, false
	}
	p, ok := s.allow(w, r, a, s.svc(d.Service))
	return d, p, ok
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	out := []*model.Deployment{}
	for _, d := range s.E.Deployments() {
		if p.Can(auth.ActRead, s.svc(d.Service)) {
			out = append(out, d)
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	d, _, ok := s.lookup(w, r, auth.ActDeploy)
	if !ok {
		return
	}
	s.audit(r, "deployment.abort", d.Service, d.ID, "")
	writeJSON(w, 200, map[string]bool{"aborted": s.E.Abort(d.ID)})
}

func (s *Server) circuitStatus(w http.ResponseWriter, r *http.Request) {
	if p := auth.FromContext(r.Context()); !p.CanSomewhere(auth.ActRead) {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: %s has no viewer role", p.ID))
		return
	}
	writeJSON(w, 200, s.E.Breaker.State())
}

func (s *Server) circuitReset(w http.ResponseWriter, r *http.Request) {
	p, ok := s.allow(w, r, auth.ActCircuit, auth.Service{})
	if !ok {
		return
	}
	s.E.Breaker.Reset()
	s.E.Log.Warn("circuit reset", "by", p.ID)
	s.audit(r, "circuit.reset", "", "", r.URL.Query().Get("reason"))
	writeJSON(w, 200, s.E.Breaker.State())
}

func (s *Server) circuitTrip(w http.ResponseWriter, r *http.Request) {
	p, ok := s.allow(w, r, auth.ActCircuit, auth.Service{})
	if !ok {
		return
	}
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "kill switch via API"
	}
	s.E.Breaker.Trip(reason + " (by " + p.ID + ")")
	s.audit(r, "circuit.trip", "", "", reason)
	writeJSON(w, 200, s.E.Breaker.State())
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	live, _, ok := s.lookup(w, r, auth.ActRead)
	if !ok {
		return
	}
	id := live.ID
	d, _ := s.E.Deployment(id)
	resp := map[string]any{"deployment": d, "exit_code": model.ExitCode(d)}
	if ev, ok := s.E.LastEvaluation(id); ok {
		resp["last_evaluation"] = ev
	}
	writeJSON(w, 200, resp)
}

// checkLaunch reports why a phase may not start now.
func checkLaunch(d *model.Deployment) error {
	if d.State == model.StateObserving || d.State == model.StateRollingBack {
		return fmt.Errorf("deployment %s is %s", d.ID, d.State)
	}
	if d.State.Terminal() && d.State != model.StateSucceeded {
		return fmt.Errorf("deployment %s is already %s", d.ID, d.State)
	}
	return nil
}

func (s *Server) launch(d *model.Deployment, phase model.Phase) error {
	switch phase {
	case model.PhaseCanary, model.PhaseRolling, model.PhaseFull:
	default:
		return fmt.Errorf("unknown phase %q", phase)
	}
	if err := checkLaunch(d); err != nil {
		return err
	}
	if err := s.E.Gate(d); err != nil {
		return err
	}
	s.background(func() {
		if err := s.E.Watch(s.ctx, d, phase); err != nil {
			s.E.Log.Error("watch failed", "deployment", d.ID, "err", err)
			s.E.MarkBlocked(d, err)
		}
	})
	return nil
}

func (s *Server) startPhase(w http.ResponseWriter, r *http.Request) {
	d, _, ok := s.lookup(w, r, auth.ActDeploy)
	if !ok {
		return
	}
	if err := s.launch(d, model.Phase(r.PathValue("phase"))); err != nil {
		writeErr(w, 409, err)
		return
	}
	s.audit(r, "phase.start", d.Service, d.ID, r.PathValue("phase"))
	if r.URL.Query().Get("wait") == "true" {
		// Long-poll for CI jobs that prefer one blocking call.
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Second):
			}
			cp, _ := s.E.Deployment(d.ID)
			if cp.State != model.StateObserving && cp.State != model.StateRollingBack && cp.State != model.StatePending {
				writeJSON(w, 200, map[string]any{"deployment": cp, "exit_code": model.ExitCode(cp)})
				return
			}
		}
	}
	cp, _ := s.E.Deployment(d.ID)
	writeJSON(w, 202, cp)
}

type rollbackReq struct {
	Executor string   `json:"executor"`
	Reason   string   `json:"reason"`
	Targets  []string `json:"targets"`
}

func (s *Server) manualRollback(w http.ResponseWriter, r *http.Request) {
	d, p, ok := s.lookup(w, r, auth.ActRollback)
	if !ok {
		return
	}
	var req rollbackReq
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	s.E.Abort(d.ID)
	reason := req.Reason
	if reason == "" {
		reason = "manual rollback via API"
	}
	s.audit(r, "rollback.manual", d.Service, d.ID, reason)
	s.background(func() {
		_ = s.E.Rollback(s.ctx, d, orchestrator.RollbackOptions{Reason: reason, Manual: true, Executor: req.Executor, Targets: req.Targets, Actor: p.ID})
	})
	writeJSON(w, 202, map[string]string{"status": "rollback started"})
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	d, p, ok := s.lookup(w, r, auth.ActApprove)
	if !ok {
		return
	}
	if d.State != model.StateAwaitApproval {
		writeErr(w, 409, fmt.Errorf("deployment is %s, not awaiting approval", d.State))
		return
	}
	if s.E.Cfg.Auth.FourEyes && (p.ID == d.CreatedBy || p.ID == d.RollbackRequestedBy) {
		writeErr(w, http.StatusForbidden, fmt.Errorf("four-eyes: %s created this deployment or requested its rollback, so another operator must approve", p.ID))
		return
	}
	if _, pending := s.E.PendingRollback(d.ID); pending {
		s.audit(r, "rollback.approve", d.Service, d.ID, "")
		s.background(func() { _ = s.E.DecideRollback(s.ctx, d, p.ID, true, "") })
		writeJSON(w, 202, map[string]string{"status": "approved; rollback running"})
		return
	}
	s.audit(r, "escalation.approve", d.Service, d.ID, "")
	s.background(func() {
		_ = s.E.Rollback(s.ctx, d, orchestrator.RollbackOptions{Reason: "approved escalation", Manual: true, Approved: true, Actor: p.ID})
	})
	writeJSON(w, 202, map[string]string{"status": "approved; escalation running"})
}

func (s *Server) captureBaseline(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.allow(w, r, auth.ActDeploy, s.svc(r.PathValue("service"))); !ok {
		return
	}
	var window time.Duration
	if v := r.URL.Query().Get("window"); v != "" {
		var err error
		if window, err = time.ParseDuration(v); err != nil {
			writeErr(w, 400, err)
			return
		}
	}
	s.audit(r, "baseline.capture", r.PathValue("service"), "", window.String())
	snap, err := s.E.CaptureBaseline(r.Context(), r.PathValue("service"), window)
	if err != nil {
		writeErr(w, 422, err)
		return
	}
	writeJSON(w, 200, snap)
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	var samples []model.Sample
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&samples); err != nil {
		writeErr(w, 400, err)
		return
	}
	checked := map[string]bool{}
	for _, sm := range samples {
		if !checked[sm.Target] {
			if !s.allowAny(w, r, auth.ActAgent, s.targetServices(sm.Target), "target "+sm.Target) {
				return
			}
			checked[sm.Target] = true
		}
	}
	for _, sm := range samples {
		if !strings.HasPrefix(sm.Source, "agent:") {
			sm.Source = "agent:" + sm.Target
		}
		s.E.Store.Add(sm)
	}
	telemetry.AgentSamples.Add(float64(len(samples)))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("target")
	if !s.allowAny(w, r, auth.ActAgent, s.targetServices(target), "target "+target) {
		return
	}
	s.mu.Lock()
	s.agents[target] = time.Now()
	s.mu.Unlock()
	var active *model.Deployment
	for _, d := range s.E.Deployments() {
		if d.State != model.StateObserving {
			continue
		}
		for _, t := range d.Targets {
			if t == target {
				active = d
			}
		}
	}
	writeJSON(w, 200, map[string]any{"time": time.Now(), "active": active, "circuit": s.E.Breaker.State().State})
}

func (s *Server) targetMetrics(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("target")
	if !s.allowAny(w, r, auth.ActRead, s.targetServices(target), "target "+target) {
		return
	}
	out := map[string]float64{}
	now := time.Now()
	for _, m := range s.E.Store.Metrics(target) {
		pts := s.E.Store.Window(target, m, time.Minute, now, "")
		if len(pts) > 0 {
			out[m] = pts[len(pts)-1].V
		}
	}
	writeJSON(w, 200, out)
}

// webhook maps CI/CD events onto deployments. The payload must identify the
// service and version; providers differ only in where those live and how the
// request is authenticated.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	provider := r.PathValue("provider")
	p, err := s.verifyWebhook(provider, r, body)
	if err != nil {
		writeErr(w, 401, err)
		return
	}
	req, err := ParseWebhook(provider, body, r.URL.Query().Get("service"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if req == nil {
		writeJSON(w, 202, map[string]string{"status": "ignored"})
		return
	}
	if !p.Can(auth.ActDeploy, s.svc(req.Service)) {
		writeErr(w, http.StatusForbidden, fmt.Errorf("forbidden: %s needs role deployer on %s", p.ID, describe(s.svc(req.Service))))
		return
	}
	req.ChangeTicket = r.Header.Get("X-Change-Ticket")
	d, err := s.create(r.Context(), p, *req)
	if err != nil {
		writeErr(w, gateStatus(err, 400), err)
		return
	}
	s.E.Audit(journal.Entry{Actor: p.ID, Source: "webhook", Action: "deployment.create", Service: d.Service, DeployID: d.ID,
		Reason: fmt.Sprintf("%s %s -> %s", d.Service, d.PreviousVersion, d.Version)})
	writeJSON(w, 201, d)
}

// verifyWebhook authenticates a webhook. GitHub and GitLab sign with the
// shared secret and act as a deployer on every service; other providers use
// a normal bearer token and that caller's own grants.
func (s *Server) verifyWebhook(provider string, r *http.Request, body []byte) (*auth.Principal, error) {
	signed := &auth.Principal{ID: "webhook:" + provider, Kind: "webhook",
		Bindings: []auth.Binding{{Role: auth.Deployer, Scope: auth.Scope{Kind: "all"}}}}
	switch provider {
	case "github":
		if s.WebhookSecret == "" {
			return nil, errors.New("webhook secret not configured")
		}
		mac := hmac.New(sha256.New, []byte(s.WebhookSecret))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(want), []byte(r.Header.Get("X-Hub-Signature-256"))) {
			return nil, errors.New("bad signature")
		}
		return signed, nil
	case "gitlab":
		if s.WebhookSecret == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Gitlab-Token")), []byte(s.WebhookSecret)) != 1 {
			return nil, errors.New("bad gitlab token")
		}
		return signed, nil
	default: // jenkins / generic: an ordinary API caller
		return s.Auth.Authenticate(r)
	}
}

// ParseWebhook extracts a deployment request. A nil request means "not an
// event we act on" (e.g. a GitHub deployment_status that is not "success").
func ParseWebhook(provider string, body []byte, serviceOverride string) (*createReq, error) {
	var req createReq
	switch provider {
	case "github":
		// deployment_status event; vigilante fields go in the deployment payload.
		var ev struct {
			DeploymentStatus struct{ State string } `json:"deployment_status"`
			Deployment       struct {
				ID          int64          `json:"id"`
				Sha         string         `json:"sha"`
				Ref         string         `json:"ref"`
				Environment string         `json:"environment"`
				Payload     map[string]any `json:"payload"`
			} `json:"deployment"`
		}
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, err
		}
		if ev.DeploymentStatus.State != "success" {
			return nil, nil
		}
		p := ev.Deployment.Payload
		req = createReq{
			ID: fmt.Sprintf("gh-%d", ev.Deployment.ID), Service: str(p["service"]),
			Version: firstNonEmpty(str(p["version"]), ev.Deployment.Ref), PreviousVersion: str(p["previous_version"]),
			Phase: firstNonEmpty(str(p["phase"]), "canary"),
		}
	case "gitlab":
		// Deployment events: status, environment, ref, short_sha, deployable_id
		var ev struct {
			Status       string            `json:"status"`
			Environment  string            `json:"environment"`
			Ref          string            `json:"ref"`
			ShortSha     string            `json:"short_sha"`
			DeploymentID int64             `json:"deployment_id"`
			Variables    map[string]string `json:"variables"`
		}
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, err
		}
		if ev.Status != "success" {
			return nil, nil
		}
		req = createReq{
			ID: fmt.Sprintf("gl-%d", ev.DeploymentID), Service: ev.Variables["VIGILANTE_SERVICE"],
			Version:         firstNonEmpty(ev.Variables["VIGILANTE_VERSION"], ev.ShortSha, ev.Ref),
			PreviousVersion: ev.Variables["VIGILANTE_PREVIOUS_VERSION"], Phase: firstNonEmpty(ev.Variables["VIGILANTE_PHASE"], "canary"),
		}
	default: // jenkins / generic: the createReq JSON itself
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
	}
	if serviceOverride != "" {
		req.Service = serviceOverride
	}
	if req.Service == "" {
		return nil, errors.New("webhook payload does not name a service (payload.service or ?service=)")
	}
	return &req, nil
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// gateStatus maps a closed deployment gate (circuit open, change freeze) to
// 409 Conflict and anything else to def.
func gateStatus(err error, def int) int {
	if errors.Is(err, orchestrator.ErrITSMUnavailable) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, orchestrator.ErrBlocked) {
		return http.StatusConflict
	}
	return def
}

// gateCode is the problem code for a closed gate.
func gateCode(err error) string {
	switch {
	case errors.Is(err, orchestrator.ErrFrozen):
		return "change_frozen"
	case errors.Is(err, orchestrator.ErrITSMUnavailable):
		return "itsm_unavailable"
	case errors.Is(err, orchestrator.ErrChangeTicket):
		return "change_ticket_invalid"
	}
	return "circuit_open"
}

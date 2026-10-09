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
//	GET  /healthz
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"vigilante/internal/model"
	"vigilante/internal/orchestrator"
)

type Server struct {
	E     *orchestrator.Engine
	Token string // bearer token; empty disables auth (dev only)
	// WebhookSecret verifies GitHub signatures / GitLab tokens.
	WebhookSecret string

	ctx    context.Context
	mu     sync.Mutex
	agents map[string]time.Time
}

func New(ctx context.Context, e *orchestrator.Engine) *Server {
	s := &Server{E: e, ctx: ctx, agents: map[string]time.Time{}}
	if env := e.Cfg.Server.AuthTokenEnv; env != "" {
		s.Token = os.Getenv(env)
	}
	if env := e.Cfg.Server.WebhookSecret; env != "" {
		s.WebhookSecret = os.Getenv(env)
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "circuit": s.E.Breaker.State().State, "dry_run": s.E.DryRun})
	})
	mux.HandleFunc("POST /v1/deployments", s.auth(s.createDeployment))
	mux.HandleFunc("GET /v1/deployments", s.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.E.Deployments())
	}))
	mux.HandleFunc("GET /v1/deployments/{id}", s.auth(s.getDeployment))
	mux.HandleFunc("POST /v1/deployments/{id}/phases/{phase}", s.auth(s.startPhase))
	mux.HandleFunc("POST /v1/deployments/{id}/rollback", s.auth(s.manualRollback))
	mux.HandleFunc("POST /v1/deployments/{id}/approve", s.auth(s.approve))
	mux.HandleFunc("POST /v1/deployments/{id}/abort", s.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"aborted": s.E.Abort(r.PathValue("id"))})
	}))
	mux.HandleFunc("POST /v1/baselines/{service}", s.auth(s.captureBaseline))
	mux.HandleFunc("GET /v1/circuit", s.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.E.Breaker.State())
	}))
	mux.HandleFunc("POST /v1/circuit/reset", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.E.Breaker.Reset()
		writeJSON(w, 200, s.E.Breaker.State())
	}))
	mux.HandleFunc("POST /v1/circuit/trip", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reason := r.URL.Query().Get("reason")
		if reason == "" {
			reason = "kill switch via API"
		}
		s.E.Breaker.Trip(reason)
		writeJSON(w, 200, s.E.Breaker.State())
	}))
	mux.HandleFunc("POST /v1/samples", s.auth(s.ingest))
	mux.HandleFunc("GET /v1/agents/{target}/heartbeat", s.auth(s.heartbeat))
	mux.HandleFunc("POST /v1/webhooks/{provider}", s.webhook) // authenticated by signature/token
	mux.HandleFunc("GET /v1/targets/{target}/metrics", s.auth(s.targetMetrics))
	return mux
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}
		h(w, r)
	}
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
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	d, err := s.create(r.Context(), req)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, d)
}

func (s *Server) create(ctx context.Context, req createReq) (*model.Deployment, error) {
	if req.Service == "" || req.Version == "" {
		return nil, errors.New("service and version are required")
	}
	var note string
	if req.PreviousVersion == "" && (req.ID == "" || s.E.Live(req.ID) == nil) {
		if v, from, ok := s.E.LastGoodVersion(req.Service); ok {
			req.PreviousVersion = v
			note = fmt.Sprintf("auto-filled previous=%s (last good deployment %s)", v, from)
		}
	}
	d, err := s.E.Create(req.ID, req.Service, req.Version, req.PreviousVersion)
	if err != nil {
		return nil, err
	}
	if note != "" {
		s.E.Annotate(d, "input", note)
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

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*model.Deployment, bool) {
	id := r.PathValue("id")
	if _, ok := s.E.Deployment(id); !ok {
		writeErr(w, 404, fmt.Errorf("deployment %q not found", id))
		return nil, false
	}
	return s.E.Live(id), true
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, ok := s.E.Deployment(id)
	if !ok {
		writeErr(w, 404, fmt.Errorf("deployment %q not found", id))
		return
	}
	resp := map[string]any{"deployment": d, "exit_code": model.ExitCode(d)}
	if ev, ok := s.E.LastEvaluation(id); ok {
		resp["last_evaluation"] = ev
	}
	writeJSON(w, 200, resp)
}

func (s *Server) launch(d *model.Deployment, phase model.Phase) error {
	switch phase {
	case model.PhaseCanary, model.PhaseRolling, model.PhaseFull:
	default:
		return fmt.Errorf("unknown phase %q", phase)
	}
	if d.State == model.StateObserving || d.State == model.StateRollingBack {
		return fmt.Errorf("deployment %s is %s", d.ID, d.State)
	}
	if d.State.Terminal() && d.State != model.StateSucceeded {
		return fmt.Errorf("deployment %s is already %s", d.ID, d.State)
	}
	go func() {
		if err := s.E.Watch(s.ctx, d, phase); err != nil {
			s.E.Log.Error("watch failed", "deployment", d.ID, "err", err)
			s.E.MarkBlocked(d, err)
		}
	}()
	return nil
}

func (s *Server) startPhase(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if err := s.launch(d, model.Phase(r.PathValue("phase"))); err != nil {
		writeErr(w, 409, err)
		return
	}
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
	d, ok := s.lookup(w, r)
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
	go s.E.Rollback(s.ctx, d, orchestrator.RollbackOptions{Reason: reason, Manual: true, Executor: req.Executor, Targets: req.Targets})
	writeJSON(w, 202, map[string]string{"status": "rollback started"})
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if d.State != model.StateAwaitApproval {
		writeErr(w, 409, fmt.Errorf("deployment is %s, not awaiting approval", d.State))
		return
	}
	go s.E.Rollback(s.ctx, d, orchestrator.RollbackOptions{Reason: "approved escalation", Manual: true, Approved: true})
	writeJSON(w, 202, map[string]string{"status": "approved; escalation running"})
}

func (s *Server) captureBaseline(w http.ResponseWriter, r *http.Request) {
	var window time.Duration
	if v := r.URL.Query().Get("window"); v != "" {
		var err error
		if window, err = time.ParseDuration(v); err != nil {
			writeErr(w, 400, err)
			return
		}
	}
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
	for _, sm := range samples {
		if !strings.HasPrefix(sm.Source, "agent:") {
			sm.Source = "agent:" + sm.Target
		}
		s.E.Store.Add(sm)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("target")
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
	if err := s.verifyWebhook(provider, r, body); err != nil {
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
	d, err := s.create(r.Context(), *req)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, d)
}

func (s *Server) verifyWebhook(provider string, r *http.Request, body []byte) error {
	switch provider {
	case "github":
		if s.WebhookSecret == "" {
			return errors.New("webhook secret not configured")
		}
		mac := hmac.New(sha256.New, []byte(s.WebhookSecret))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(want), []byte(r.Header.Get("X-Hub-Signature-256"))) {
			return errors.New("bad signature")
		}
		return nil
	case "gitlab":
		if s.WebhookSecret == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Gitlab-Token")), []byte(s.WebhookSecret)) != 1 {
			return errors.New("bad gitlab token")
		}
		return nil
	default: // jenkins / generic use the API bearer token
		if s.Token == "" {
			return nil
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			return errors.New("unauthorized")
		}
		return nil
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

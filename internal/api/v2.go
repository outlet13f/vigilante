package api

// API v2: the public contract described by api/openapi.yaml. Conventions:
// RFC 9457 problem errors, cursor pages, Idempotency-Key on writes,
// ETag / If-Match on deployments, and 202 + Operation for long work.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"vigilante/internal/audit"
	"vigilante/internal/auth"
	"vigilante/internal/config"
	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/orchestrator"
)

type route struct {
	pattern string
	handler http.HandlerFunc
	write   bool // honours Idempotency-Key
}

// v2Routes is the v2 route table; TestV2RoutesMatchSpec holds it to the spec.
func (s *Server) v2Routes() []route {
	return []route{
		{"GET /v2/me", s.v2Me, false},
		{"GET /v2/deployments", s.v2ListDeployments, false},
		{"POST /v2/deployments", s.v2CreateDeployment, true},
		{"GET /v2/deployments/{id}", s.v2GetDeployment, false},
		{"POST /v2/deployments/{id}/observations", s.v2StartObservation, true},
		{"POST /v2/deployments/{id}/rollbacks", s.v2StartRollback, true},
		{"POST /v2/deployments/{id}/approvals", s.v2Approve, true},
		{"POST /v2/deployments/{id}/abort", s.v2Abort, true},
		{"GET /v2/operations", s.v2ListOperations, false},
		{"GET /v2/operations/{id}", s.v2GetOperation, false},
		{"GET /v2/services", s.v2ListServices, false},
		{"GET /v2/services/{name}", s.v2GetService, false},
		{"POST /v2/services/{name}/baselines", s.v2CaptureBaseline, true},
		{"PUT /v2/services/{name}/last-good", s.v2SetLastGood, true},
		{"GET /v2/services/{name}/last-good", s.v2GetLastGood, false},
		{"GET /v2/targets", s.v2ListTargets, false},
		{"GET /v2/targets/{name}", s.v2GetTarget, false},
		{"GET /v2/presets", s.v2ListPresets, false},
		{"GET /v2/circuit", s.v2GetCircuit, false},
		{"POST /v2/circuit/reset", s.v2ResetCircuit, true},
		{"POST /v2/circuit/trip", s.v2TripCircuit, true},
		{"GET /v2/audit-events", s.v2ListAuditEvents, false},
	}
}

func (s *Server) routesV2(mux *http.ServeMux) {
	for _, rt := range s.v2Routes() {
		h := rt.handler
		if rt.write {
			h = s.idempotent(h)
		}
		mux.HandleFunc(rt.pattern, s.authn(h))
	}
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		s.problem(w, r, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
	})
}

// ---------------------------------------------------------------- problems

// problemBase prefixes the problem type; each code has a section in docs/06-api.md.
const problemBase = "https://github.com/outlet13f/vigilante/blob/master/docs/06-api.md#"

type fieldError struct {
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

type problemDoc struct {
	Type      string       `json:"type"`
	Title     string       `json:"title"`
	Status    int          `json:"status"`
	Detail    string       `json:"detail,omitempty"`
	Code      string       `json:"code"`
	RequestID string       `json:"request_id,omitempty"`
	Errors    []fieldError `json:"errors,omitempty"`
}

// wantsProblem reports whether errors on this path use problem+json.
func wantsProblem(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/v2/") || r.URL.Path == "/metrics"
}

func (s *Server) problem(w http.ResponseWriter, r *http.Request, status int, code, detail string, errs ...fieldError) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(problemDoc{Type: problemBase + code, Title: http.StatusText(status), Status: status,
		Detail: detail, Code: code, RequestID: r.Header.Get(requestIDHeader), Errors: errs})
}

// fail writes an error in the style of the API version being called.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, code string, err error) {
	if wantsProblem(r) {
		s.problem(w, r, status, code, err.Error())
		return
	}
	writeErr(w, status, err)
}

// ---------------------------------------------------------------- paging

type pageReq struct {
	limit  int
	cursor string // decoded key; "" = first page
}

func (s *Server) pageParams(w http.ResponseWriter, r *http.Request) (pageReq, bool) {
	p := pageReq{limit: 50}
	q := r.URL.Query()
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			s.problem(w, r, 400, "bad_request", "limit must be an integer from 1 to 500",
				fieldError{Field: "limit", Message: "between 1 and 500"})
			return p, false
		}
		p.limit = n
	}
	if v := q.Get("cursor"); v != "" {
		b, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil || len(b) == 0 {
			s.problem(w, r, 400, "bad_request", "cursor is not one this server issued", fieldError{Field: "cursor", Message: "invalid"})
			return p, false
		}
		p.cursor = string(b)
	}
	return p, true
}

// paginate returns the page after the cursor. items must already be sorted
// by key ascending (desc=false) or descending (desc=true); keys are compared
// rather than looked up, so a deleted item never breaks a cursor.
func paginate[T any](items []T, key func(T) string, desc bool, p pageReq) ([]T, *string) {
	start := 0
	if p.cursor != "" {
		start = len(items)
		for i, it := range items {
			k := key(it)
			if (!desc && k > p.cursor) || (desc && k < p.cursor) {
				start = i
				break
			}
		}
	}
	end := min(start+p.limit, len(items))
	out := items[start:end]
	if end < len(items) && len(out) > 0 {
		c := base64.RawURLEncoding.EncodeToString([]byte(key(out[len(out)-1])))
		return out, &c
	}
	return out, nil
}

type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// timeKey sorts newest first with a fixed-width timestamp.
func timeKey(t time.Time, id string) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z") + "|" + id
}

// ---------------------------------------------------------------- idempotency

// idempotent replays the stored response when a write is retried with the
// same Idempotency-Key and body. The response is stored durably before it is
// sent, so neither a lost reply nor a leader change can run an action twice.
func (s *Server) idempotent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next(w, r)
			return
		}
		if len(key) > 255 {
			s.problem(w, r, 400, "bad_request", "Idempotency-Key is longer than 255 characters")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			s.problem(w, r, 400, "bad_request", err.Error())
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		who := principalID(auth.FromContext(r.Context()))
		ks := sha256.Sum256([]byte(who + "\x00" + key))
		k := hex.EncodeToString(ks[:])
		fs := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
		fp := hex.EncodeToString(fs[:])

		if rec, ok := s.E.Idempotent(k); ok {
			if rec.Fingerprint != fp {
				s.problem(w, r, http.StatusUnprocessableEntity, "idempotency_key_reused",
					"this Idempotency-Key was used for a different request; use a new key")
				return
			}
			if rec.Location != "" {
				w.Header().Set("Location", rec.Location)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(rec.Status)
			_, _ = w.Write(rec.Body)
			return
		}
		s.mu.Lock()
		if s.inflight == nil {
			s.inflight = map[string]bool{}
		}
		busy := s.inflight[k]
		s.inflight[k] = true
		s.mu.Unlock()
		if busy {
			w.Header().Set("Retry-After", "1")
			s.problem(w, r, http.StatusConflict, "idempotency_key_in_flight", "a request with this Idempotency-Key is still being processed")
			return
		}
		defer func() {
			s.mu.Lock()
			delete(s.inflight, k)
			s.mu.Unlock()
		}()

		rec := &captureWriter{header: http.Header{}}
		next(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		if rec.status < 300 { // errors may be retried after the cause is fixed
			s.E.RememberIdempotent(&model.IdemRecord{Key: k, Fingerprint: fp, Status: rec.status,
				Location: rec.header.Get("Location"), Body: json.RawMessage(bytes.TrimSpace(rec.body.Bytes())), CreatedAt: time.Now().UTC()})
		}
		for name, vs := range rec.header {
			w.Header()[name] = vs
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.body.Bytes())
	}
}

type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header { return c.header }
func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(b)
}

// ---------------------------------------------------------------- representations

type evalV2 struct {
	Time    time.Time `json:"time"`
	Failing int       `json:"failing"`
	Holding int       `json:"holding"`
	Warning int       `json:"warning"`
	Known   int       `json:"known"`
	Unknown int       `json:"unknown"`
}

type deploymentV2 struct {
	*model.Deployment
	ExitCode       int     `json:"exit_code"`
	LastEvaluation *evalV2 `json:"last_evaluation,omitempty"`
}

func (s *Server) deploymentV2(id string) (*deploymentV2, bool) {
	d, ok := s.E.Deployment(id)
	if !ok {
		return nil, false
	}
	out := &deploymentV2{Deployment: d, ExitCode: model.ExitCode(d)}
	if ev, ok := s.E.LastEvaluation(id); ok {
		out.LastEvaluation = &evalV2{Time: ev.Time.UTC(), Failing: len(ev.Fail), Holding: len(ev.Hold), Warning: len(ev.Warn), Known: ev.Known, Unknown: ev.Unknown}
	}
	return out, true
}

func etagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return `W/"` + hex.EncodeToString(sum[:8]) + `"`
}

// writeTagged writes v with an ETag and honours If-None-Match on reads.
func writeTagged(w http.ResponseWriter, r *http.Request, status int, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	tag := etagOf(b)
	w.Header().Set("ETag", tag)
	if r.Method == http.MethodGet && r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// precondition enforces If-Match against the deployment's current ETag.
func (s *Server) precondition(w http.ResponseWriter, r *http.Request, id string) bool {
	want := r.Header.Get("If-Match")
	if want == "" || want == "*" {
		return true
	}
	cur, _ := s.deploymentV2(id)
	b, _ := json.MarshalIndent(cur, "", "  ")
	if etagOf(b) != want {
		s.problem(w, r, http.StatusPreconditionFailed, "precondition_failed", "the deployment changed since the ETag in If-Match; fetch it again")
		return false
	}
	return true
}

// decodeStrict reads a JSON body into v, rejecting unknown fields. An empty
// body is allowed when optional.
func (s *Server) decodeStrict(w http.ResponseWriter, r *http.Request, v any, optional bool) bool {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.problem(w, r, 400, "bad_request", err.Error())
		return false
	}
	if len(bytes.TrimSpace(b)) == 0 {
		if optional {
			return true
		}
		s.problem(w, r, 400, "bad_request", "request body is required")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		s.problem(w, r, 400, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) accepted(w http.ResponseWriter, op *model.Operation) {
	w.Header().Set("Location", "/v2/operations/"+op.ID)
	writeJSON(w, http.StatusAccepted, op)
}

// v2Lookup finds the deployment in the path and checks the caller may do a.
func (s *Server) v2Lookup(w http.ResponseWriter, r *http.Request, a auth.Action) (*model.Deployment, *auth.Principal, bool) {
	id := r.PathValue("id")
	d := s.E.Live(id)
	if d == nil {
		s.problem(w, r, 404, "not_found", fmt.Sprintf("deployment %q not found", id))
		return nil, nil, false
	}
	p, ok := s.allow(w, r, a, s.svc(d.Service))
	return d, p, ok
}

// ---------------------------------------------------------------- identity

func (s *Server) v2Me(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	grants := make([]string, len(p.Bindings))
	for i, b := range p.Bindings {
		grants[i] = b.String()
	}
	writeJSON(w, 200, map[string]any{"id": p.ID, "kind": p.Kind, "groups": p.Groups, "grants": grants})
}

// ---------------------------------------------------------------- deployments

func (s *Server) v2ListDeployments(w http.ResponseWriter, r *http.Request) {
	pr, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	p := auth.FromContext(r.Context())
	q := r.URL.Query()
	var items []*deploymentV2
	for _, d := range s.E.Deployments() {
		if !p.Can(auth.ActRead, s.svc(d.Service)) {
			continue
		}
		if v := q.Get("service"); v != "" && d.Service != v {
			continue
		}
		if v := q.Get("state"); v != "" && string(d.State) != v {
			continue
		}
		if dv, ok := s.deploymentV2(d.ID); ok {
			items = append(items, dv)
		}
	}
	key := func(d *deploymentV2) string { return timeKey(d.CreatedAt, d.ID) }
	sort.Slice(items, func(i, j int) bool { return key(items[i]) > key(items[j]) })
	out, next := paginate(items, key, true, pr)
	if out == nil {
		out = []*deploymentV2{}
	}
	writeJSON(w, 200, page[*deploymentV2]{Items: out, NextCursor: next})
}

type createV2 struct {
	ID              string `json:"id"`
	Service         string `json:"service"`
	Version         string `json:"version"`
	PreviousVersion string `json:"previous_version"`
	Prepare         bool   `json:"prepare"`
}

func (s *Server) v2CreateDeployment(w http.ResponseWriter, r *http.Request) {
	var req createV2
	if !s.decodeStrict(w, r, &req, false) {
		return
	}
	var errs []fieldError
	if req.Service == "" {
		errs = append(errs, fieldError{"service", "required"})
	}
	if req.Version == "" {
		errs = append(errs, fieldError{"version", "required"})
	}
	if len(req.ID) > 128 {
		errs = append(errs, fieldError{"id", "at most 128 characters"})
	}
	if len(errs) > 0 {
		s.problem(w, r, 422, "validation_failed", "invalid deployment", errs...)
		return
	}
	if _, ok := s.E.Cfg.Service(req.Service); !ok {
		s.problem(w, r, 422, "validation_failed", fmt.Sprintf("unknown service %q", req.Service), fieldError{"service", "unknown"})
		return
	}
	p, ok := s.allow(w, r, auth.ActDeploy, s.svc(req.Service))
	if !ok {
		return
	}
	existing := req.ID != "" && s.E.Live(req.ID) != nil
	if !existing && req.PreviousVersion == "" {
		if _, _, ok := s.E.LastGoodVersion(req.Service); !ok {
			s.problem(w, r, 422, "validation_failed",
				"previous_version is required: service "+req.Service+" has no last good version yet (register one with PUT /v2/services/{name}/last-good)",
				fieldError{"previous_version", "required"})
			return
		}
	}
	d, err := s.create(r.Context(), p, createReq{ID: req.ID, Service: req.Service, Version: req.Version, PreviousVersion: req.PreviousVersion, Prepare: req.Prepare})
	if err != nil {
		if d == nil {
			s.problem(w, r, http.StatusConflict, "conflict", err.Error())
		} else {
			s.problem(w, r, 422, "validation_failed", err.Error())
		}
		return
	}
	status := http.StatusCreated
	if existing {
		status = http.StatusOK
	} else {
		s.audit(r, "deployment.create", d.Service, d.ID, fmt.Sprintf("%s %s -> %s", d.Service, d.PreviousVersion, d.Version))
	}
	dv, _ := s.deploymentV2(d.ID)
	w.Header().Set("Location", "/v2/deployments/"+d.ID)
	writeTagged(w, r, status, dv)
}

func (s *Server) v2GetDeployment(w http.ResponseWriter, r *http.Request) {
	d, _, ok := s.v2Lookup(w, r, auth.ActRead)
	if !ok {
		return
	}
	dv, _ := s.deploymentV2(d.ID)
	writeTagged(w, r, 200, dv)
}

func (s *Server) v2StartObservation(w http.ResponseWriter, r *http.Request) {
	d, p, ok := s.v2Lookup(w, r, auth.ActDeploy)
	if !ok || !s.precondition(w, r, d.ID) {
		return
	}
	var req struct {
		Phase string `json:"phase"`
	}
	if !s.decodeStrict(w, r, &req, false) {
		return
	}
	phase := model.Phase(req.Phase)
	switch phase {
	case model.PhaseCanary, model.PhaseRolling, model.PhaseFull:
	default:
		s.problem(w, r, 422, "validation_failed", fmt.Sprintf("unknown phase %q", req.Phase), fieldError{"phase", "canary, rolling or full"})
		return
	}
	svc, _ := s.E.Cfg.Service(d.Service)
	if _, ok := svc.Phases[string(phase)]; !ok {
		s.problem(w, r, 422, "validation_failed", fmt.Sprintf("service %s has no phase %q configured", svc.Name, phase), fieldError{"phase", "not configured"})
		return
	}
	if err := checkLaunch(d); err != nil {
		s.problem(w, r, http.StatusConflict, "conflict", err.Error())
		return
	}
	op := s.E.StartOperation(model.OpObservation, d.Service, d, phase, p.ID)
	s.audit(r, "phase.start", d.Service, d.ID, string(phase))
	go func() {
		err := s.E.Watch(s.ctx, d, phase)
		if err != nil {
			s.E.Log.Error("watch failed", "deployment", d.ID, "err", err)
			if !errors.Is(err, orchestrator.ErrInactive) {
				s.E.MarkBlocked(d, err)
			}
		}
		cp, _ := s.E.Deployment(d.ID)
		if err != nil {
			s.E.FinishOperation(op.ID, model.OpFailed, model.ResultOf(cp), err.Error())
			return
		}
		s.E.FinishOperation(op.ID, model.OpCompleted, model.ResultOf(cp), "")
	}()
	s.accepted(w, op)
}

// rollbackDone reports whether the rollback ran to an outcome (as opposed to
// not starting at all).
func rollbackOutcome(d *model.Deployment) bool {
	switch d.State {
	case model.StateRolledBack, model.StateRollbackFailed, model.StateAwaitApproval:
		return true
	}
	return false
}

func (s *Server) runRollbackOp(op *model.Operation, d *model.Deployment, opt orchestrator.RollbackOptions) {
	err := s.E.Rollback(s.ctx, d, opt)
	cp, _ := s.E.Deployment(d.ID)
	if err != nil && !rollbackOutcome(cp) {
		s.E.FinishOperation(op.ID, model.OpFailed, model.ResultOf(cp), err.Error())
		return
	}
	s.E.FinishOperation(op.ID, model.OpCompleted, model.ResultOf(cp), "")
}

func (s *Server) v2StartRollback(w http.ResponseWriter, r *http.Request) {
	d, p, ok := s.v2Lookup(w, r, auth.ActRollback)
	if !ok || !s.precondition(w, r, d.ID) {
		return
	}
	var req rollbackReq
	if !s.decodeStrict(w, r, &req, true) {
		return
	}
	if req.Executor != "" {
		if _, ok := s.E.Cfg.Executors[req.Executor]; !ok {
			s.problem(w, r, 422, "validation_failed", fmt.Sprintf("unknown executor %q", req.Executor), fieldError{"executor", "unknown"})
			return
		}
	}
	for _, t := range req.Targets {
		if _, ok := s.E.Cfg.Target(t); !ok {
			s.problem(w, r, 422, "validation_failed", fmt.Sprintf("unknown target %q", t), fieldError{"targets", "unknown target " + t})
			return
		}
	}
	if d.State == model.StateRollingBack {
		s.problem(w, r, http.StatusConflict, "conflict", "a rollback of this deployment is already running")
		return
	}
	reason := req.Reason
	if reason == "" {
		reason = "manual rollback via API"
	}
	s.E.Abort(d.ID)
	op := s.E.StartOperation(model.OpRollback, d.Service, d, "", p.ID)
	s.audit(r, "rollback.manual", d.Service, d.ID, reason)
	go s.runRollbackOp(op, d, orchestrator.RollbackOptions{Reason: reason, Manual: true, Executor: req.Executor, Targets: req.Targets, Actor: p.ID})
	s.accepted(w, op)
}

func (s *Server) v2Approve(w http.ResponseWriter, r *http.Request) {
	d, p, ok := s.v2Lookup(w, r, auth.ActApprove)
	if !ok || !s.precondition(w, r, d.ID) {
		return
	}
	var req struct {
		Comment string `json:"comment"`
	}
	if !s.decodeStrict(w, r, &req, true) {
		return
	}
	if d.State != model.StateAwaitApproval {
		s.problem(w, r, http.StatusConflict, "conflict", fmt.Sprintf("deployment is %s, not awaiting approval", d.State))
		return
	}
	if s.E.Cfg.Auth.FourEyes && (p.ID == d.CreatedBy || p.ID == d.RollbackRequestedBy) {
		err := fmt.Errorf("four-eyes: %s created this deployment or requested its rollback, so another operator must approve", p.ID)
		s.denied(r, d.Service, err)
		s.problem(w, r, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	op := s.E.StartOperation(model.OpApproval, d.Service, d, "", p.ID)
	s.audit(r, "escalation.approve", d.Service, d.ID, req.Comment)
	go s.runRollbackOp(op, d, orchestrator.RollbackOptions{Reason: "approved escalation", Manual: true, Approved: true, Actor: p.ID})
	s.accepted(w, op)
}

func (s *Server) v2Abort(w http.ResponseWriter, r *http.Request) {
	d, _, ok := s.v2Lookup(w, r, auth.ActDeploy)
	if !ok {
		return
	}
	if s.E.Abort(d.ID) {
		s.audit(r, "deployment.abort", d.Service, d.ID, "")
	}
	dv, _ := s.deploymentV2(d.ID)
	writeTagged(w, r, 200, dv)
}

// ---------------------------------------------------------------- operations

func (s *Server) v2ListOperations(w http.ResponseWriter, r *http.Request) {
	pr, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	p := auth.FromContext(r.Context())
	q := r.URL.Query()
	var items []*model.Operation
	for _, op := range s.E.Operations() {
		if !p.Can(auth.ActRead, s.svc(op.Service)) {
			continue
		}
		if v := q.Get("deployment_id"); v != "" && op.DeploymentID != v {
			continue
		}
		if v := q.Get("status"); v != "" && op.Status != v {
			continue
		}
		items = append(items, op)
	}
	key := func(op *model.Operation) string { return timeKey(op.CreatedAt, op.ID) }
	out, next := paginate(items, key, true, pr)
	if out == nil {
		out = []*model.Operation{}
	}
	writeJSON(w, 200, page[*model.Operation]{Items: out, NextCursor: next})
}

func (s *Server) v2GetOperation(w http.ResponseWriter, r *http.Request) {
	op, ok := s.E.Operation(r.PathValue("id"))
	if !ok {
		s.problem(w, r, 404, "not_found", fmt.Sprintf("operation %q not found", r.PathValue("id")))
		return
	}
	if _, ok := s.allow(w, r, auth.ActRead, s.svc(op.Service)); !ok {
		return
	}
	writeTagged(w, r, 200, op)
}

// ---------------------------------------------------------------- services

type serviceRule struct {
	Name   string `json:"name"`
	Action string `json:"action"`
}

type serviceV2 struct {
	Name            string        `json:"name"`
	Team            string        `json:"team,omitempty"`
	Preset          string        `json:"preset,omitempty"`
	Targets         []string      `json:"targets"`
	ControlTargets  []string      `json:"control_targets,omitempty"`
	Phases          []string      `json:"phases"`
	Rules           []serviceRule `json:"rules"`
	Executor        string        `json:"executor"`
	Traffic         string        `json:"traffic,omitempty"`
	LastGoodVersion string        `json:"last_good_version,omitempty"`
}

func (s *Server) serviceV2(sv *config.Service) serviceV2 {
	out := serviceV2{Name: sv.Name, Team: sv.Team, Preset: sv.Preset, Targets: append([]string{}, sv.Targets...),
		ControlTargets: sv.ControlTargets, Executor: sv.Rollback.Executor, Traffic: sv.Rollback.Traffic, Phases: []string{}, Rules: []serviceRule{}}
	for _, ph := range model.PhaseOrder {
		if _, ok := sv.Phases[string(ph)]; ok {
			out.Phases = append(out.Phases, string(ph))
		}
	}
	for _, rl := range sv.Rules {
		a := rl.Action
		if a == "" {
			a = "rollback"
		}
		out.Rules = append(out.Rules, serviceRule{Name: rl.Name, Action: a})
	}
	if v, _, ok := s.E.LastGoodVersion(sv.Name); ok {
		out.LastGoodVersion = v
	}
	return out
}

func (s *Server) v2ListServices(w http.ResponseWriter, r *http.Request) {
	pr, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	p := auth.FromContext(r.Context())
	var items []serviceV2
	for i := range s.E.Cfg.Services {
		sv := &s.E.Cfg.Services[i]
		if !p.Can(auth.ActRead, auth.Service{Name: sv.Name, Team: sv.Team}) {
			continue
		}
		if t := r.URL.Query().Get("team"); t != "" && sv.Team != t {
			continue
		}
		items = append(items, s.serviceV2(sv))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	out, next := paginate(items, func(v serviceV2) string { return v.Name }, false, pr)
	if out == nil {
		out = []serviceV2{}
	}
	writeJSON(w, 200, page[serviceV2]{Items: out, NextCursor: next})
}

// v2Service finds the service in the path and checks the caller may do a.
func (s *Server) v2Service(w http.ResponseWriter, r *http.Request, a auth.Action) (*config.Service, *auth.Principal, bool) {
	sv, ok := s.E.Cfg.Service(r.PathValue("name"))
	if !ok {
		s.problem(w, r, 404, "not_found", fmt.Sprintf("service %q not found", r.PathValue("name")))
		return nil, nil, false
	}
	p, ok := s.allow(w, r, a, auth.Service{Name: sv.Name, Team: sv.Team})
	return sv, p, ok
}

func (s *Server) v2GetService(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.v2Service(w, r, auth.ActRead)
	if !ok {
		return
	}
	writeJSON(w, 200, s.serviceV2(sv))
}

var durationRe = regexp.MustCompile(`^[0-9]+(ms|s|m|h)([0-9]+(ms|s|m))*$`)

func (s *Server) v2CaptureBaseline(w http.ResponseWriter, r *http.Request) {
	sv, p, ok := s.v2Service(w, r, auth.ActDeploy)
	if !ok {
		return
	}
	var req struct {
		Window string `json:"window"`
	}
	if !s.decodeStrict(w, r, &req, true) {
		return
	}
	var window time.Duration
	if req.Window != "" {
		var err error
		if window, err = time.ParseDuration(req.Window); err != nil || !durationRe.MatchString(req.Window) || window <= 0 {
			s.problem(w, r, 422, "validation_failed", "window must be a positive duration such as 5m", fieldError{"window", "invalid duration"})
			return
		}
	}
	op := s.E.StartOperation(model.OpBaseline, sv.Name, nil, "", p.ID)
	s.audit(r, "baseline.capture", sv.Name, "", window.String())
	go func() {
		snap, err := s.E.CaptureBaseline(s.ctx, sv.Name, window)
		res := &model.OperationResult{}
		if snap != nil {
			res.BaselineSamples = snap.Samples
		}
		if err != nil {
			s.E.FinishOperation(op.ID, model.OpFailed, res, err.Error())
			return
		}
		s.E.FinishOperation(op.ID, model.OpCompleted, res, "")
	}()
	s.accepted(w, op)
}

type lastGoodV2 struct {
	Service      string `json:"service"`
	Version      string `json:"version"`
	DeploymentID string `json:"deployment_id,omitempty"`
}

func (s *Server) v2SetLastGood(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.v2Service(w, r, auth.ActDeploy)
	if !ok {
		return
	}
	var req struct {
		Version string `json:"version"`
		Reason  string `json:"reason"`
	}
	if !s.decodeStrict(w, r, &req, false) {
		return
	}
	if req.Version == "" {
		s.problem(w, r, 422, "validation_failed", "version is required", fieldError{"version", "required"})
		return
	}
	d, err := s.E.MarkGood(sv.Name, req.Version, req.Reason)
	if err != nil {
		s.problem(w, r, 422, "validation_failed", err.Error())
		return
	}
	s.E.SetCreatedBy(d, principalID(auth.FromContext(r.Context())))
	s.audit(r, "mark-good", sv.Name, d.ID, req.Reason)
	writeJSON(w, 200, lastGoodV2{Service: sv.Name, Version: req.Version, DeploymentID: d.ID})
}

func (s *Server) v2GetLastGood(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.v2Service(w, r, auth.ActRead)
	if !ok {
		return
	}
	v, from, ok := s.E.LastGoodVersion(sv.Name)
	if !ok {
		s.problem(w, r, 404, "not_found", "service "+sv.Name+" has no last good version yet")
		return
	}
	writeJSON(w, 200, lastGoodV2{Service: sv.Name, Version: v, DeploymentID: from})
}

// ---------------------------------------------------------------- targets, presets

type targetV2 struct {
	Name       string            `json:"name"`
	Kind       string            `json:"kind"`
	Address    string            `json:"address,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Connection string            `json:"connection"`
	Services   []string          `json:"services"`
}

// targetVisible: a target is visible to whoever may read one of its
// services; targets in no service only to viewers of every service.
func (s *Server) targetV2(t *config.Target, p *auth.Principal) (targetV2, bool) {
	out := targetV2{Name: t.Name, Kind: t.Kind, Address: t.Address, Labels: t.Labels, Connection: t.Connection.Type, Services: []string{}}
	if out.Connection == "" {
		out.Connection = "none"
	}
	visible := p.Can(auth.ActRead, auth.Service{})
	for _, sv := range s.targetServices(t.Name) {
		out.Services = append(out.Services, sv.Name)
		visible = visible || p.Can(auth.ActRead, sv)
	}
	return out, visible
}

func (s *Server) v2ListTargets(w http.ResponseWriter, r *http.Request) {
	pr, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	p := auth.FromContext(r.Context())
	var items []targetV2
	for i := range s.E.Cfg.Targets {
		if tv, ok := s.targetV2(&s.E.Cfg.Targets[i], p); ok {
			items = append(items, tv)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	out, next := paginate(items, func(v targetV2) string { return v.Name }, false, pr)
	if out == nil {
		out = []targetV2{}
	}
	writeJSON(w, 200, page[targetV2]{Items: out, NextCursor: next})
}

func (s *Server) v2GetTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := s.E.Cfg.Target(r.PathValue("name"))
	if !ok {
		s.problem(w, r, 404, "not_found", fmt.Sprintf("target %q not found", r.PathValue("name")))
		return
	}
	p := auth.FromContext(r.Context())
	tv, visible := s.targetV2(t, p)
	if !visible {
		err := fmt.Errorf("forbidden: %s needs role viewer on a service using target %s", principalID(p), t.Name)
		s.denied(r, "", err)
		s.problem(w, r, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	writeJSON(w, 200, tv)
}

type presetV2 struct {
	Name        string        `json:"name"`
	Version     int           `json:"version"`
	Ref         string        `json:"ref"`
	Description string        `json:"description,omitempty"`
	Source      string        `json:"source"`
	Params      []presetParam `json:"params"`
}

type presetParam struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	Default     any    `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

func (s *Server) v2ListPresets(w http.ResponseWriter, r *http.Request) {
	reg, err := s.E.Cfg.PresetRegistry(s.E.Cfg.BaseDir)
	if err != nil {
		s.problem(w, r, 500, "internal", err.Error())
		return
	}
	items := []presetV2{}
	for _, p := range reg.List() {
		pv := presetV2{Name: p.Name, Version: p.Version, Ref: p.Ref(), Description: p.Description, Source: p.Source, Params: []presetParam{}}
		for _, pa := range p.Params {
			pv.Params = append(pv.Params, presetParam{Name: pa.Name, Required: pa.Required, Default: pa.Default, Description: pa.Description})
		}
		items = append(items, pv)
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// ---------------------------------------------------------------- circuit

type circuitV2 struct {
	State          string     `json:"state"`
	Reason         string     `json:"reason,omitempty"`
	OpenedAt       *time.Time `json:"opened_at,omitempty"`
	RecentFailures int        `json:"recent_failures"`
}

func (s *Server) circuitV2() circuitV2 {
	st := s.E.Breaker.State()
	out := circuitV2{State: string(st.State), Reason: st.Reason, RecentFailures: len(st.Failures)}
	if !st.OpenedAt.IsZero() {
		t := st.OpenedAt.UTC()
		out.OpenedAt = &t
	}
	return out
}

func (s *Server) v2GetCircuit(w http.ResponseWriter, r *http.Request) {
	if p := auth.FromContext(r.Context()); !p.CanSomewhere(auth.ActRead) {
		s.problem(w, r, http.StatusForbidden, "forbidden", fmt.Sprintf("forbidden: %s has no viewer role", p.ID))
		return
	}
	writeJSON(w, 200, s.circuitV2())
}

func (s *Server) circuitReason(w http.ResponseWriter, r *http.Request) (*auth.Principal, string, bool) {
	p, ok := s.allow(w, r, auth.ActCircuit, auth.Service{})
	if !ok {
		return nil, "", false
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if !s.decodeStrict(w, r, &req, false) {
		return nil, "", false
	}
	if strings.TrimSpace(req.Reason) == "" {
		s.problem(w, r, 422, "validation_failed", "reason is required", fieldError{"reason", "required"})
		return nil, "", false
	}
	return p, req.Reason, true
}

func (s *Server) v2ResetCircuit(w http.ResponseWriter, r *http.Request) {
	p, reason, ok := s.circuitReason(w, r)
	if !ok {
		return
	}
	s.E.Breaker.Reset()
	s.E.Log.Warn("circuit reset", "by", p.ID, "reason", reason)
	s.audit(r, "circuit.reset", "", "", reason)
	writeJSON(w, 200, s.circuitV2())
}

func (s *Server) v2TripCircuit(w http.ResponseWriter, r *http.Request) {
	p, reason, ok := s.circuitReason(w, r)
	if !ok {
		return
	}
	s.E.Breaker.Trip(reason + " (by " + p.ID + ")")
	s.audit(r, "circuit.trip", "", "", reason)
	writeJSON(w, 200, s.circuitV2())
}

// ---------------------------------------------------------------- audit

type auditEventV2 struct {
	Seq          int64     `json:"seq"`
	Time         time.Time `json:"time"`
	Kind         string    `json:"kind"`
	Actor        string    `json:"actor,omitempty"`
	Source       string    `json:"source,omitempty"`
	Action       string    `json:"action,omitempty"`
	Service      string    `json:"service,omitempty"`
	DeploymentID string    `json:"deployment_id,omitempty"`
	State        string    `json:"state,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	Ticket       string    `json:"ticket,omitempty"`
	Hash         string    `json:"hash,omitempty"`
	Prev         string    `json:"prev,omitempty"`
}

func (s *Server) v2ListAuditEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.allow(w, r, auth.ActRead, auth.Service{}); !ok {
		return
	}
	pr, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := audit.Filter{Actor: q.Get("actor"), Service: q.Get("service"), Action: q.Get("action")}
	for _, k := range []struct {
		name string
		dst  *time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if v := q.Get(k.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				s.problem(w, r, 400, "bad_request", k.name+" must be RFC 3339", fieldError{k.name, "RFC 3339 timestamp"})
				return
			}
			*k.dst = t
		}
	}
	var after int64 = -1
	if pr.cursor != "" {
		n, err := strconv.ParseInt(pr.cursor, 10, 64)
		if err != nil {
			s.problem(w, r, 400, "bad_request", "cursor is not one this server issued")
			return
		}
		after = n
	}
	items := []auditEventV2{}
	more := false
	errStop := errors.New("page full")
	err := s.E.Journal.Scan(r.Context(), func(pos int64, e journal.Entry) error {
		if pos <= after || !f.Match(e) {
			return nil
		}
		if len(items) == pr.limit {
			more = true
			return errStop
		}
		ev := auditEventV2{Seq: pos, Time: e.Time.UTC(), Kind: e.Kind, Actor: e.Actor, Source: e.Source, Action: e.Action,
			Service: e.Service, DeploymentID: e.DeployID, Reason: e.Reason, Ticket: e.Ticket, Hash: e.Hash, Prev: e.Prev}
		if e.Deployment != nil {
			ev.Service, ev.DeploymentID, ev.State = e.Deployment.Service, e.Deployment.ID, string(e.Deployment.State)
		}
		if e.Circuit != nil {
			ev.State = string(e.Circuit.State)
		}
		items = append(items, ev)
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		s.problem(w, r, 500, "internal", err.Error())
		return
	}
	var next *string
	if more {
		c := base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(items[len(items)-1].Seq, 10)))
		next = &c
	}
	writeJSON(w, 200, page[auditEventV2]{Items: items, NextCursor: next})
}

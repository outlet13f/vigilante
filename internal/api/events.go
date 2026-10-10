package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"vigilante/internal/auth"
	"vigilante/internal/events"
	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/notify"
	"vigilante/internal/secrets"
)

// newBus wires the event bus to the engine: it observes every stored entry,
// reloads with the engine (HA election), and delivers webhooks while this
// node is the active leader.
func (s *Server) newBus() *events.Bus {
	e := s.E
	b := events.New(events.Deps{
		Record: e.Record,
		Active: e.Active,
		Log:    e.Log,
		Alert: func(title, text string) {
			e.Notify.Send(context.Background(), notify.Message{Level: notify.Critical, Title: title, Text: text})
		},
		Audit: func(action, reason string) {
			e.Audit(journal.Entry{Actor: "system", Source: "system", Action: action, Reason: reason})
		},
		SigningKey: func(ctx context.Context) ([]byte, error) {
			ref := e.Cfg.API.WebhookSigningKeyRef
			if ref == "" {
				return nil, events.ErrNoSigningKey
			}
			v, err := secrets.Resolve(ctx, ref)
			return []byte(v), err
		},
		TeamOf: func(service string) string {
			if sv, ok := e.Cfg.Service(service); ok {
				return sv.Team
			}
			return ""
		},
		Source: "/vigilante",
	})
	e.AddReloadHook(b.Reload)
	e.AddRecordHook(b.Observe)
	b.Start(s.ctx)
	go s.watchAgents()
	return b
}

// watchAgents publishes agent.lost once when an agent's heartbeat stops.
func (s *Server) watchAgents() {
	after := s.E.Cfg.Agent.FailsafeAfter
	if after <= 0 {
		after = 30 * time.Second
	}
	lost := map[string]bool{}
	t := time.NewTicker(min(after/3, 5*time.Second))
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		var gone []string
		for target, seen := range s.agents {
			switch {
			case time.Since(seen) > after && !lost[target]:
				lost[target] = true
				gone = append(gone, target+"|"+seen.UTC().Format(time.RFC3339))
			case time.Since(seen) <= after:
				delete(lost, target)
			}
		}
		s.mu.Unlock()
		for _, g := range gone {
			target, seen, _ := strings.Cut(g, "|")
			var svcs []string
			for _, sv := range s.targetServices(target) {
				svcs = append(svcs, sv.Name)
			}
			s.bus.Emit(model.EvAgentLost, target, "", map[string]any{"target": target, "last_seen": seen, "services": svcs})
		}
	}
}

// eventVisible: service events need viewer on that service; events of no
// service (circuit, agents) need viewer somewhere.
func (s *Server) eventVisible(p *auth.Principal) func(*model.CloudEvent) bool {
	return func(ev *model.CloudEvent) bool {
		if ev.Service == "" {
			return p.CanSomewhere(auth.ActRead)
		}
		return p.Can(auth.ActRead, auth.Service{Name: ev.Service, Team: ev.Team})
	}
}

func knownType(t string) bool {
	for _, et := range model.EventTypes {
		if et.Type == t {
			return true
		}
	}
	return false
}

func (s *Server) typesParam(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	v := r.URL.Query().Get("types")
	if v == "" {
		return nil, true
	}
	types := strings.Split(v, ",")
	for _, t := range types {
		if !knownType(t) {
			s.problem(w, r, 400, "bad_request", fmt.Sprintf("unknown event type %q (see GET /v2/event-types)", t), fieldError{"types", "unknown type " + t})
			return nil, false
		}
	}
	return types, true
}

// v2Events streams events as Server-Sent Events. A reconnecting client
// sends Last-Event-ID (or ?after=) and receives every event it missed that
// is still kept, then the live stream.
func (s *Server) v2Events(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.CanSomewhere(auth.ActRead) {
		s.problem(w, r, http.StatusForbidden, "forbidden", fmt.Sprintf("forbidden: %s has no viewer role", p.ID))
		return
	}
	types, ok := s.typesParam(w, r)
	if !ok {
		return
	}
	var after int64
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = r.URL.Query().Get("after")
	}
	if last != "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			s.problem(w, r, 400, "bad_request", "Last-Event-ID / after must be an event sequence number")
			return
		}
		after = n
	} else {
		after = s.bus.Seq() // new clients start with the live stream
	}
	f := events.Filter{Types: types, Visible: s.eventVisible(p)}
	if svc := r.URL.Query().Get("service"); svc != "" {
		vis := f.Visible
		f.Visible = func(ev *model.CloudEvent) bool { return ev.Service == svc && vis(ev) }
	}
	_, gap := s.bus.Since(after, f, 1)
	backlog, sub := s.bus.Subscribe(after, f)
	defer s.bus.Unsubscribe(sub)

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(ev *model.CloudEvent) error {
		b, _ := json.Marshal(ev)
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Sequence, ev.Type, b); err != nil {
			return err
		}
		return rc.Flush()
	}
	fmt.Fprint(w, "retry: 3000\n\n")
	if gap {
		fmt.Fprintf(w, ": events after %d were already discarded; resuming from the oldest kept\n\n", after)
	}
	_ = rc.Flush()
	for _, ev := range backlog {
		if write(ev) != nil {
			return
		}
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case ev, ok := <-sub.C():
			if !ok {
				return // fell behind; the client reconnects with Last-Event-ID
			}
			if write(ev) != nil {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

func (s *Server) v2EventTypes(w http.ResponseWriter, r *http.Request) {
	items := make([]map[string]string, 0, len(model.EventTypes))
	for _, et := range model.EventTypes {
		items = append(items, map[string]string{"type": et.Type, "description": et.Description})
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// ---------------------------------------------------------------- webhooks

type webhookV2 struct {
	ID             string             `json:"id"`
	URL            string             `json:"url"`
	Description    string             `json:"description,omitempty"`
	Types          []string           `json:"types"`
	Services       []string           `json:"services"`
	Teams          []string           `json:"teams"`
	Active         bool               `json:"active"`
	DisabledReason string             `json:"disabled_reason,omitempty"`
	Cursor         int64              `json:"cursor"`
	Failures       int                `json:"consecutive_failures"`
	DeadLetters    []model.DeadLetter `json:"dead_letters"`
	CreatedBy      string             `json:"created_by,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
}

func toWebhookV2(h *model.Webhook) webhookV2 {
	nz := func(v []string) []string {
		if v == nil {
			return []string{}
		}
		return v
	}
	dl := h.DeadLetters
	if dl == nil {
		dl = []model.DeadLetter{}
	}
	return webhookV2{ID: h.ID, URL: h.URL, Description: h.Description, Types: nz(h.Types), Services: nz(h.Services), Teams: nz(h.Teams),
		Active: h.Active, DisabledReason: h.DisabledReason, Cursor: h.Cursor, Failures: h.Failures, DeadLetters: dl,
		CreatedBy: h.CreatedBy, CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt}
}

type webhookFields struct {
	URL         *string   `json:"url"`
	Description *string   `json:"description"`
	Types       *[]string `json:"types"`
	Services    *[]string `json:"services"`
	Teams       *[]string `json:"teams"`
	Active      *bool     `json:"active"`
}

func (s *Server) validateWebhook(f webhookFields, create bool) []fieldError {
	var errs []fieldError
	if create && f.URL == nil {
		errs = append(errs, fieldError{"url", "required"})
	}
	if f.URL != nil {
		u, err := url.Parse(*f.URL)
		switch {
		case err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil:
			errs = append(errs, fieldError{"url", "an http(s) URL without credentials"})
		case len(s.E.Cfg.API.WebhookAllowedHosts) > 0 && !hostAllowed(u.Hostname(), s.E.Cfg.API.WebhookAllowedHosts):
			errs = append(errs, fieldError{"url", "host is not in api.webhook_allowed_hosts"})
		}
	}
	if f.Types != nil {
		for _, t := range *f.Types {
			if !knownType(t) {
				errs = append(errs, fieldError{"types", "unknown event type " + t})
			}
		}
	}
	if f.Services != nil {
		for _, sv := range *f.Services {
			if _, ok := s.E.Cfg.Service(sv); !ok {
				errs = append(errs, fieldError{"services", "unknown service " + sv})
			}
		}
	}
	return errs
}

func hostAllowed(host string, allowed []string) bool {
	for _, a := range allowed {
		if host == a || (strings.HasPrefix(a, ".") && strings.HasSuffix(host, a)) {
			return true
		}
	}
	return false
}

// webhookScope: a subscriber must be able to read everything the
// subscription can carry (viewer on its services and teams, or on * when it
// is unfiltered); API clients also need config:write.
func (s *Server) webhookScope(w http.ResponseWriter, r *http.Request, services, teams []string) (*auth.Principal, bool) {
	p := auth.FromContext(r.Context())
	ok := p.HasScope(auth.ScopeConfigWrite)
	if ok {
		rp := *p
		rp.Scopes = nil
		if len(services) == 0 && len(teams) == 0 {
			ok = rp.Can(auth.ActRead, auth.Service{})
		}
		for _, sv := range services {
			ok = ok && rp.Can(auth.ActRead, s.svc(sv))
		}
		for _, t := range teams {
			ok = ok && slices.ContainsFunc(rp.Bindings, func(b auth.Binding) bool {
				return b.Role >= auth.Viewer && (b.Scope.Kind == "all" || (b.Scope.Kind == "team" && b.Scope.Value == t))
			})
		}
	}
	if !ok {
		err := fmt.Errorf("forbidden: %s needs viewer on every service and team the subscription covers (on * when unfiltered), and scope %s for API clients", principalID(p), auth.ScopeConfigWrite)
		s.denied(r, "", err)
		s.problem(w, r, http.StatusForbidden, "forbidden", err.Error())
		return nil, false
	}
	return p, true
}

// ownWebhook finds a subscription the caller may manage: its creator, or an admin.
func (s *Server) ownWebhook(w http.ResponseWriter, r *http.Request) (*model.Webhook, *auth.Principal, bool) {
	h, ok := s.bus.Webhook(r.PathValue("id"))
	if !ok {
		s.problem(w, r, 404, "not_found", fmt.Sprintf("webhook %q not found", r.PathValue("id")))
		return nil, nil, false
	}
	p := auth.FromContext(r.Context())
	if h.CreatedBy != p.ID && !p.Can(auth.ActManage, auth.Service{}) {
		err := fmt.Errorf("forbidden: webhook %s belongs to %s", h.ID, h.CreatedBy)
		s.denied(r, "", err)
		s.problem(w, r, http.StatusForbidden, "forbidden", err.Error())
		return nil, nil, false
	}
	return h, p, true
}

func (s *Server) v2CreateWebhook(w http.ResponseWriter, r *http.Request) {
	var f webhookFields
	if !s.decodeStrict(w, r, &f, false) {
		return
	}
	if errs := s.validateWebhook(f, true); len(errs) > 0 {
		s.problem(w, r, 422, "validation_failed", "invalid webhook", errs...)
		return
	}
	h := &model.Webhook{URL: *f.URL}
	if f.Description != nil {
		h.Description = *f.Description
	}
	if f.Types != nil {
		h.Types = *f.Types
	}
	if f.Services != nil {
		h.Services = *f.Services
	}
	if f.Teams != nil {
		h.Teams = *f.Teams
	}
	p, ok := s.webhookScope(w, r, h.Services, h.Teams)
	if !ok {
		return
	}
	h.CreatedBy = p.ID
	secret, err := s.bus.CreateWebhook(r.Context(), h)
	if errors.Is(err, events.ErrNoSigningKey) {
		s.problem(w, r, 422, "validation_failed", "webhooks are not enabled on this server: set api.webhook_signing_key_ref")
		return
	}
	if err != nil {
		s.problem(w, r, 500, "internal", err.Error())
		return
	}
	s.audit(r, "webhook.create", "", "", h.ID+" "+h.URL)
	w.Header().Set("Location", "/v2/webhooks/"+h.ID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"webhook": toWebhookV2(h), "secret": secret})
}

func (s *Server) v2ListWebhooks(w http.ResponseWriter, r *http.Request) {
	pr, ok := s.pageParams(w, r)
	if !ok {
		return
	}
	p := auth.FromContext(r.Context())
	admin := p.Can(auth.ActManage, auth.Service{})
	var items []webhookV2
	for _, h := range s.bus.Webhooks() {
		if admin || h.CreatedBy == p.ID {
			items = append(items, toWebhookV2(h))
		}
	}
	out, next := paginate(items, func(h webhookV2) string { return timeKey(h.CreatedAt, h.ID) }, false, pr)
	if out == nil {
		out = []webhookV2{}
	}
	writeJSON(w, 200, page[webhookV2]{Items: out, NextCursor: next})
}

func (s *Server) v2GetWebhook(w http.ResponseWriter, r *http.Request) {
	h, _, ok := s.ownWebhook(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, toWebhookV2(h))
}

func (s *Server) v2UpdateWebhook(w http.ResponseWriter, r *http.Request) {
	cur, _, ok := s.ownWebhook(w, r)
	if !ok {
		return
	}
	var f webhookFields
	if !s.decodeStrict(w, r, &f, false) {
		return
	}
	if errs := s.validateWebhook(f, false); len(errs) > 0 {
		s.problem(w, r, 422, "validation_failed", "invalid webhook change", errs...)
		return
	}
	services, teams := cur.Services, cur.Teams
	if f.Services != nil {
		services = *f.Services
	}
	if f.Teams != nil {
		teams = *f.Teams
	}
	if _, ok := s.webhookScope(w, r, services, teams); !ok {
		return
	}
	h, err := s.bus.UpdateWebhook(cur.ID, func(h *model.Webhook) error {
		if f.URL != nil {
			h.URL = *f.URL
		}
		if f.Description != nil {
			h.Description = *f.Description
		}
		if f.Types != nil {
			h.Types = *f.Types
		}
		h.Services, h.Teams = services, teams
		if f.Active != nil {
			h.Active = *f.Active
		}
		return nil
	})
	if err != nil {
		s.problem(w, r, 404, "not_found", err.Error())
		return
	}
	s.audit(r, "webhook.update", "", "", h.ID+" "+h.URL)
	writeJSON(w, 200, toWebhookV2(h))
}

func (s *Server) v2DeleteWebhook(w http.ResponseWriter, r *http.Request) {
	h, _, ok := s.ownWebhook(w, r)
	if !ok {
		return
	}
	if err := s.bus.DeleteWebhook(h.ID); err != nil {
		s.problem(w, r, 404, "not_found", err.Error())
		return
	}
	s.audit(r, "webhook.delete", "", "", h.ID+" "+h.URL)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) v2RotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	h, _, ok := s.ownWebhook(w, r)
	if !ok {
		return
	}
	secret, h, err := s.bus.RotateSecret(r.Context(), h.ID)
	if err != nil {
		s.problem(w, r, 500, "internal", err.Error())
		return
	}
	s.audit(r, "webhook.rotate", "", "", h.ID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"webhook": toWebhookV2(h), "secret": secret})
}

func (s *Server) v2WebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	h, _, ok := s.ownWebhook(w, r)
	if !ok {
		return
	}
	items, _ := s.bus.Deliveries(h.ID)
	if items == nil {
		items = []events.Delivery{}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) v2Redeliver(w http.ResponseWriter, r *http.Request) {
	h, _, ok := s.ownWebhook(w, r)
	if !ok {
		return
	}
	var req struct {
		Sequence int64 `json:"sequence"`
	}
	if !s.decodeStrict(w, r, &req, false) {
		return
	}
	switch err := s.bus.Redeliver(h.ID, req.Sequence); {
	case errors.Is(err, events.ErrEventGone):
		s.problem(w, r, 422, "validation_failed", fmt.Sprintf("event %d is no longer kept", req.Sequence), fieldError{"sequence", "not kept"})
		return
	case err != nil:
		s.problem(w, r, 404, "not_found", err.Error())
		return
	}
	s.audit(r, "webhook.redeliver", "", "", fmt.Sprintf("%s event %d", h.ID, req.Sequence))
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "sequence": req.Sequence})
}

func (s *Server) v2PingWebhook(w http.ResponseWriter, r *http.Request) {
	h, p, ok := s.ownWebhook(w, r)
	if !ok {
		return
	}
	ev, err := s.bus.Ping(h.ID, p.ID)
	if err != nil {
		s.problem(w, r, http.StatusServiceUnavailable, "not_leader", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, ev)
}

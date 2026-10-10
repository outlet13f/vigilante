// Package itsm connects vigilante to an IT service management system.
// ServiceNow (Table API) is supported: change tickets gate new deployments,
// failed rollbacks and an open circuit raise incidents, and deployment
// outcomes are written to the change ticket as work notes.
//
// None of this is on the rollback path: if ServiceNow is down, rollbacks
// still run; only the change gate can refuse new deployments, and only when
// its on_error policy says so.
package itsm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/secrets"
)

// Change is the part of a ServiceNow change_request vigilante reads.
type Change struct {
	SysID            string `json:"sys_id"`
	Number           string `json:"number"`
	State            string `json:"state"`
	Approval         string `json:"approval"`
	StartDate        string `json:"start_date"` // planned window, UTC "2006-01-02 15:04:05"
	EndDate          string `json:"end_date"`
	ShortDescription string `json:"short_description"`
}

// StateNames are the default change_request states.
var StateNames = map[string]string{"-5": "New", "-4": "Assess", "-3": "Authorize", "-2": "Scheduled",
	"-1": "Implement", "0": "Review", "3": "Closed", "4": "Canceled"}

func stateName(s string) string {
	if n, ok := StateNames[s]; ok {
		return n + " (" + s + ")"
	}
	return s
}

// ErrInvalidChange explains why a change ticket does not allow the deployment.
type ErrInvalidChange struct{ Reason string }

func (e *ErrInvalidChange) Error() string { return e.Reason }

// ServiceNow is a Table API client.
type ServiceNow struct {
	cfg   config.ServiceNow
	creds map[string]config.Credential
	http  *http.Client
	Now   func() time.Time
}

func NewServiceNow(cfg config.ServiceNow, creds map[string]config.Credential) *ServiceNow {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: cfg.TLSSkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec // opt-in
	return &ServiceNow{cfg: cfg, creds: creds, http: &http.Client{Transport: tr, Timeout: 15 * time.Second}, Now: time.Now}
}

// Config returns the client's configuration.
func (s *ServiceNow) Config() config.ServiceNow { return s.cfg }

func (s *ServiceNow) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(s.cfg.URL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c, ok := s.creds[s.cfg.Credential]
	if !ok {
		return fmt.Errorf("servicenow: unknown credential %q", s.cfg.Credential)
	}
	switch c.Type {
	case "token":
		tok, err := secrets.Value(ctx, c.TokenRef, c.TokenEnv)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	default:
		user := c.User
		if c.UsernameRef != "" || c.UsernameEnv != "" {
			if user, err = secrets.Value(ctx, c.UsernameRef, c.UsernameEnv); err != nil {
				return err
			}
		}
		pass, err := secrets.Value(ctx, c.PasswordRef, c.PasswordEnv)
		if err != nil {
			return err
		}
		req.SetBasicAuth(user, pass)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("servicenow: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("servicenow %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Change reads a change request by number.
func (s *ServiceNow) Change(ctx context.Context, number string) (*Change, error) {
	q := url.Values{"sysparm_query": {"number=" + number}, "sysparm_limit": {"1"}, "sysparm_display_value": {"false"},
		"sysparm_fields": {"sys_id,number,state,approval,start_date,end_date,short_description"}}
	var out struct {
		Result []Change `json:"result"`
	}
	if err := s.call(ctx, http.MethodGet, "/api/now/table/change_request?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	if len(out.Result) == 0 {
		return nil, &ErrInvalidChange{Reason: "change " + number + " does not exist"}
	}
	return &out.Result[0], nil
}

const snowTime = "2006-01-02 15:04:05"

// Check reads a change and confirms it allows a deployment now: approved,
// in an allowed state, and (with check_window) inside its planned window.
func (s *ServiceNow) Check(ctx context.Context, number string) (*Change, error) {
	c, err := s.Change(ctx, number)
	if err != nil {
		return nil, err
	}
	g := s.cfg.ChangeGate
	if c.Approval != "approved" {
		return c, &ErrInvalidChange{Reason: fmt.Sprintf("change %s is not approved (approval: %s)", c.Number, c.Approval)}
	}
	if !slices.Contains(g.AllowedStates, c.State) {
		var allowed []string
		for _, st := range g.AllowedStates {
			allowed = append(allowed, stateName(st))
		}
		return c, &ErrInvalidChange{Reason: fmt.Sprintf("change %s is in state %s; deployments need %s", c.Number, stateName(c.State), strings.Join(allowed, " or "))}
	}
	if g.CheckWindow == nil || *g.CheckWindow {
		start, err1 := time.ParseInLocation(snowTime, c.StartDate, time.UTC)
		end, err2 := time.ParseInLocation(snowTime, c.EndDate, time.UTC)
		now := s.Now().UTC()
		switch {
		case err1 != nil || err2 != nil:
			return c, &ErrInvalidChange{Reason: fmt.Sprintf("change %s has no planned window (start %q, end %q)", c.Number, c.StartDate, c.EndDate)}
		case now.Before(start) || now.After(end):
			return c, &ErrInvalidChange{Reason: fmt.Sprintf("change %s is planned for %s .. %s UTC, not now", c.Number, c.StartDate, c.EndDate)}
		}
	}
	return c, nil
}

// EnsureIncident opens an incident unless an active one with the same
// correlation ID exists (so a retry or a second leader does not duplicate).
func (s *ServiceNow) EnsureIncident(ctx context.Context, correlationID, short, description string) (number string, created bool, err error) {
	q := url.Values{"sysparm_query": {"correlation_id=" + correlationID + "^active=true"}, "sysparm_limit": {"1"}, "sysparm_fields": {"number"}}
	var found struct {
		Result []struct {
			Number string `json:"number"`
		} `json:"result"`
	}
	if err := s.call(ctx, http.MethodGet, "/api/now/table/incident?"+q.Encode(), nil, &found); err != nil {
		return "", false, err
	}
	if len(found.Result) > 0 {
		return found.Result[0].Number, false, nil
	}
	p := s.cfg.Incidents
	body := map[string]any{"short_description": truncate(short, 160), "description": description, "correlation_id": correlationID,
		"correlation_display": "vigilante", "urgency": fmt.Sprint(p.Urgency), "impact": fmt.Sprint(p.Impact)}
	if p.AssignmentGroup != "" {
		body["assignment_group"] = p.AssignmentGroup
	}
	if p.CallerID != "" {
		body["caller_id"] = p.CallerID
	}
	var out struct {
		Result struct {
			Number string `json:"number"`
		} `json:"result"`
	}
	if err := s.call(ctx, http.MethodPost, "/api/now/table/incident", body, &out); err != nil {
		return "", false, err
	}
	if out.Result.Number == "" {
		return "", false, errors.New("servicenow: incident created without a number")
	}
	return out.Result.Number, true, nil
}

// WorkNote appends a work note to a change request.
func (s *ServiceNow) WorkNote(ctx context.Context, sysID, note string) error {
	return s.call(ctx, http.MethodPatch, "/api/now/table/change_request/"+url.PathEscape(sysID), map[string]string{"work_notes": note}, nil)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

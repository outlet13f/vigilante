package itsm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/itsm/snowtest"
)

func client(t *testing.T, f *snowtest.Fake) *ServiceNow {
	t.Setenv("VGL_SNOW_PW", "snow-pass")
	cfg := config.ServiceNow{URL: f.Srv.URL, Credential: "snow",
		ChangeGate: config.ChangeGate{Enabled: true, AllowedStates: []string{"-2", "-1"}, OnError: "closed"},
		Incidents:  config.IncidentPolicy{Enabled: true, AssignmentGroup: "SRE", Urgency: 1, Impact: 2}}
	s := NewServiceNow(cfg, map[string]config.Credential{"snow": {Type: "basic", User: "vigilante", PasswordRef: "env:VGL_SNOW_PW"}})
	s.Now = func() time.Time { return time.Date(2026, 10, 10, 6, 0, 0, 0, time.UTC) }
	return s
}

func TestChangeChecks(t *testing.T) {
	f := snowtest.New(t)
	f.AddChange("CHG001", "sys1", "-1", "approved", "2026-10-10 05:00:00", "2026-10-10 08:00:00")
	f.AddChange("CHG002", "sys2", "-3", "requested", "2026-10-10 05:00:00", "2026-10-10 08:00:00")
	f.AddChange("CHG003", "sys3", "-5", "approved", "2026-10-10 05:00:00", "2026-10-10 08:00:00")
	f.AddChange("CHG004", "sys4", "-2", "approved", "2026-10-11 05:00:00", "2026-10-11 08:00:00")
	s := client(t, f)
	ctx := context.Background()
	if c, err := s.Check(ctx, "CHG001"); err != nil || c.SysID != "sys1" {
		t.Fatalf("valid change: %v %+v", err, c)
	}
	for num, want := range map[string]string{
		"CHG002": "not approved", "CHG003": "state New (-5)", "CHG004": "planned for 2026-10-11", "CHG999": "does not exist",
	} {
		_, err := s.Check(ctx, num)
		var inv *ErrInvalidChange
		if !errors.As(err, &inv) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want %q)", num, err, want)
		}
	}
	f.SetDown(true)
	_, err := s.Check(ctx, "CHG001")
	var inv *ErrInvalidChange
	if err == nil || errors.As(err, &inv) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("outage must be a transport error, not an invalid change: %v", err)
	}
}

func TestIncidentsAreDeduplicatedAndNotesWritten(t *testing.T) {
	f := snowtest.New(t)
	s := client(t, f)
	ctx := context.Background()
	n1, created, err := s.EnsureIncident(ctx, "vigilante:d1:rollback_failed", "rollback failed", "details")
	if err != nil || !created || n1 != "INC0000001" {
		t.Fatalf("create: %s %v %v", n1, created, err)
	}
	n2, created, err := s.EnsureIncident(ctx, "vigilante:d1:rollback_failed", "rollback failed", "details")
	if err != nil || created || n2 != n1 {
		t.Fatalf("second call must find the open incident: %s %v %v", n2, created, err)
	}
	inc, _ := f.Snapshot()
	if len(inc) != 1 || inc[0]["assignment_group"] != "SRE" || inc[0]["urgency"] != "1" || inc[0]["impact"] != "2" {
		t.Fatalf("incident fields: %v", inc)
	}
	if err := s.WorkNote(ctx, "sys1", "[vigilante] rollback completed"); err != nil {
		t.Fatal(err)
	}
	if _, wn := f.Snapshot(); len(wn["sys1"]) != 1 {
		t.Fatalf("work notes: %v", wn)
	}
}

func fastRetries(s *ServiceNow) {
	s.Backoff = []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
}

func TestIncidentRetriedUntilServiceNowRecovers(t *testing.T) {
	if len(DefaultBackoff) != 3 || DefaultBackoff[0] != time.Second || DefaultBackoff[2] != 4*time.Second {
		t.Fatalf("default backoff: %v", DefaultBackoff)
	}
	f := snowtest.New(t)
	s := client(t, f)
	fastRetries(s)
	f.FailIncidents(2) // the search fails twice, then all is well
	var number string
	var created bool
	attempts, err := s.Retry(context.Background(), func(ctx context.Context) (err error) {
		number, created, err = s.EnsureIncident(ctx, "vigilante:d1:rollback_failed", "rollback failed", "details")
		return err
	})
	if err != nil || attempts != 3 || !created || number != "INC0000001" {
		t.Fatalf("attempts %d, %s %v %v", attempts, number, created, err)
	}
	if inc, _ := f.Snapshot(); len(inc) != 1 || inc[0]["correlation_id"] != "vigilante:d1:rollback_failed" {
		t.Fatalf("incidents: %v", inc)
	}
}

// A create that reached ServiceNow but whose answer was lost is found by its
// correlation_id on the retry, not created twice.
func TestRetryAfterLostCreateDoesNotDuplicate(t *testing.T) {
	f := snowtest.New(t)
	s := client(t, f)
	fastRetries(s)
	f.LoseCreates(1)
	var number string
	var created bool
	attempts, err := s.Retry(context.Background(), func(ctx context.Context) (err error) {
		number, created, err = s.EnsureIncident(ctx, "vigilante:circuit:7", "circuit open", "details")
		return err
	})
	if err != nil || attempts != 2 || created || number != "INC0000001" {
		t.Fatalf("attempts %d, %s %v %v", attempts, number, created, err)
	}
	if inc, _ := f.Snapshot(); len(inc) != 1 {
		t.Fatalf("duplicate incidents: %v", inc)
	}
}

func TestRetryIsBounded(t *testing.T) {
	f := snowtest.New(t)
	s := client(t, f)
	fastRetries(s)
	f.SetDown(true)
	attempts, err := s.Retry(context.Background(), func(ctx context.Context) error {
		_, _, err := s.EnsureIncident(ctx, "c", "s", "d")
		return err
	})
	var se *StatusError
	if attempts != 4 || !errors.As(err, &se) || se.Code != 503 {
		t.Fatalf("down: %d attempts, %v", attempts, err)
	}

	// A 4xx will not get better: no retry.
	f.SetDown(false)
	t.Setenv("VGL_SNOW_BAD", "wrong")
	bad := NewServiceNow(s.Config(), map[string]config.Credential{"snow": {Type: "basic", User: "vigilante", PasswordRef: "env:VGL_SNOW_BAD"}})
	fastRetries(bad)
	before := f.IncidentRequests()
	if attempts, err := bad.Retry(context.Background(), func(ctx context.Context) error {
		_, _, err := bad.EnsureIncident(ctx, "c", "s", "d")
		return err
	}); attempts != 1 || !errors.As(err, &se) || se.Code != 401 || f.IncidentRequests() != before+1 {
		t.Fatalf("401: %d attempts, %v", attempts, err)
	}

	// The context ends the wait between attempts.
	f.SetDown(true)
	s.Backoff = []time.Duration{time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	attempts, err = s.Retry(ctx, func(ctx context.Context) error {
		_, _, err := s.EnsureIncident(ctx, "c", "s", "d")
		return err
	})
	if attempts != 1 || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("cancelled: %d attempts, %v after %s", attempts, err, time.Since(start))
	}
}

func TestRetryable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&StatusError{Code: 503}, true},
		{&StatusError{Code: 500}, true},
		{&StatusError{Code: 429}, true},
		{&StatusError{Code: 400}, false},
		{&StatusError{Code: 403}, false},
		{errors.New("servicenow: dial tcp 10.0.0.1:443: connect: connection refused"), true},
		{&ErrInvalidChange{Reason: "change CHG1 does not exist"}, false},
		{nil, false},
	} {
		if got := Retryable(tc.err); got != tc.want {
			t.Errorf("Retryable(%v) = %v", tc.err, got)
		}
	}
}

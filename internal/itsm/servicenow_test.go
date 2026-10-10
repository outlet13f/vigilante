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

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeV1 answers POST /v1/deployments with create (status, body) and
// GET /v1/deployments/{id} with a ROLLED_BACK verdict; it keeps what the
// CLI sent.
type fakeV1 struct {
	mu     sync.Mutex
	status int
	answer string
	body   map[string]any
	ticket string // X-Change-Ticket
}

func (f *fakeV1) serve(t *testing.T) *httptest.Server {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/deployments":
			f.body = nil
			_ = json.NewDecoder(r.Body).Decode(&f.body)
			f.ticket = r.Header.Get("X-Change-Ticket")
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.answer))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/deployments/d1":
			_, _ = w.Write([]byte(`{"deployment":{"id":"d1","service":"svc","state":"ROLLED_BACK"},"exit_code":2}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hs.Close)
	return hs
}

// sent returns the last create body and X-Change-Ticket header.
func (f *fakeV1) sent() (map[string]any, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body, f.ticket
}

func watchArgs(url string, extra ...string) []string {
	return append([]string{"--server", url, "--service", "svc", "--version", "v2", "--previous", "v1", "--id", "d1", "--phase", "canary"}, extra...)
}

func TestWatchRemoteSendsTicketAndFreezeOverride(t *testing.T) {
	old := remotePoll
	remotePoll = 10 * time.Millisecond
	t.Cleanup(func() { remotePoll = old })
	f := &fakeV1{status: 201, answer: `{"id":"d1","service":"svc","state":"PENDING"}`}
	hs := f.serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code, err := cmdWatch(ctx, watchArgs(hs.URL, "--ticket", "CHG0012345", "--freeze-override", "hotfix for INC42"))
	if err != nil || code != 2 {
		t.Fatalf("watch: code %d err %v (want the server's exit code 2)", code, err)
	}
	body, ticket := f.sent()
	if body["change_ticket"] != "CHG0012345" || ticket != "CHG0012345" {
		t.Errorf("ticket not forwarded: body %v header %q", body, ticket)
	}
	if body["freeze_override"] != "hotfix for INC42" || body["phase"] != "canary" {
		t.Errorf("freeze override not forwarded: %v", body)
	}

	// Without the flags nothing extra is sent.
	code, err = cmdWatch(ctx, watchArgs(hs.URL))
	if err != nil || code != 2 {
		t.Fatalf("watch: code %d err %v", code, err)
	}
	body, ticket = f.sent()
	if _, ok := body["freeze_override"]; ok || body["change_ticket"] != nil || ticket != "" {
		t.Errorf("unexpected gate fields: %v %q", body, ticket)
	}
}

func TestWatchRemoteGateRefusalExitsThree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name   string
		status int
		answer string
		want   int
	}{
		{"freeze", 409, `{"error":"deployment gate closed: change freeze \"q4\"","code":"change_frozen"}`, 3},
		{"no ticket", 409, `{"error":"deployment gate closed: change ticket required for svc","code":"change_ticket_invalid"}`, 3},
		{"itsm down", 503, `{"error":"deployment gate closed: ITSM unavailable","code":"itsm_unavailable"}`, 3},
		{"circuit", 409, `{"error":"deployment gate closed: circuit open","code":"circuit_open"}`, 3},
		{"older server", 409, `{"error":"deployment gate closed: change freeze"}`, 3},
		{"v2 problem", 409, `{"type":"x","title":"Conflict","status":409,"code":"change_frozen"}`, 3},
		{"bad request", 400, `{"error":"service and version are required"}`, 1},
		{"forbidden", 403, `{"error":"forbidden: only admins may override a change freeze (sa:ci)"}`, 1},
		{"not leader", 503, `{"error":"no leader available yet; retry shortly"}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeV1{status: tc.status, answer: tc.answer}
			hs := f.serve(t)
			code, err := cmdWatch(ctx, watchArgs(hs.URL, "--ticket", "CHG1"))
			if err == nil || code != tc.want {
				t.Fatalf("code %d err %v, want %d and an error", code, err, tc.want)
			}
		})
	}
}

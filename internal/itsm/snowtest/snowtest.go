// Package snowtest is an in-process fake of the ServiceNow Table API parts
// vigilante uses: change_request reads and work notes, incident search and
// creation. Basic auth user "vigilante" / password "snow-pass".
package snowtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type Fake struct {
	Srv *httptest.Server

	mu        sync.Mutex
	Changes   map[string]map[string]string // number -> fields
	Incidents []map[string]any
	WorkNotes map[string][]string // sys_id -> notes
	Down      bool                // answer 503 to everything
	// failIncident answers 503 to that many incident requests;
	// lostCreates stores that many incidents but answers 504, like a
	// create whose response never arrived.
	failIncident, lostCreates int
	incidentReqs              int
}

// IncidentRequests counts incident requests (searches and creates).
func (f *Fake) IncidentRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.incidentReqs
}

// FailIncidents makes the next n incident requests (search or create) fail
// with 503.
func (f *Fake) FailIncidents(n int) {
	f.mu.Lock()
	f.failIncident = n
	f.mu.Unlock()
}

// LoseCreates makes the next n incident creates succeed in ServiceNow but
// answer 504 to the caller.
func (f *Fake) LoseCreates(n int) {
	f.mu.Lock()
	f.lostCreates = n
	f.mu.Unlock()
}

func New(t testing.TB) *Fake {
	f := &Fake{Changes: map[string]map[string]string{}, WorkNotes: map[string][]string{}}
	f.Srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Srv.Close)
	return f
}

// AddChange registers a change request.
func (f *Fake) AddChange(number, sysID, state, approval, start, end string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Changes[number] = map[string]string{"number": number, "sys_id": sysID, "state": state, "approval": approval,
		"start_date": start, "end_date": end, "short_description": "deploy " + number}
}

// SetDown makes the fake unreachable-ish (503).
func (f *Fake) SetDown(down bool) {
	f.mu.Lock()
	f.Down = down
	f.mu.Unlock()
}

// Snapshot returns copies of incidents and work notes.
func (f *Fake) Snapshot() ([]map[string]any, map[string][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inc := append([]map[string]any(nil), f.Incidents...)
	wn := map[string][]string{}
	for k, v := range f.WorkNotes {
		wn[k] = append([]string(nil), v...)
	}
	return inc, wn
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	incident := r.URL.Path == "/api/now/table/incident"
	if incident {
		f.incidentReqs++
	}
	if f.Down {
		reply(w, 503, map[string]any{"error": map[string]string{"message": "maintenance"}})
		return
	}
	if u, p, ok := r.BasicAuth(); !ok || u != "vigilante" || p != "snow-pass" {
		reply(w, 401, map[string]any{"error": map[string]string{"message": "User Not Authenticated"}})
		return
	}
	if incident {
		if f.failIncident > 0 {
			f.failIncident--
			reply(w, 503, map[string]any{"error": map[string]string{"message": "instance busy"}})
			return
		}
	}
	q := r.URL.Query().Get("sysparm_query")
	switch {
	case r.URL.Path == "/api/now/table/change_request" && r.Method == http.MethodGet:
		num := strings.TrimPrefix(q, "number=")
		res := []map[string]string{}
		if c, ok := f.Changes[num]; ok {
			res = append(res, c)
		}
		reply(w, 200, map[string]any{"result": res})
	case strings.HasPrefix(r.URL.Path, "/api/now/table/change_request/") && r.Method == http.MethodPatch:
		id := strings.TrimPrefix(r.URL.Path, "/api/now/table/change_request/")
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.WorkNotes[id] = append(f.WorkNotes[id], body["work_notes"])
		reply(w, 200, map[string]any{"result": map[string]string{"sys_id": id}})
	case r.URL.Path == "/api/now/table/incident" && r.Method == http.MethodGet:
		corr, _, _ := strings.Cut(strings.TrimPrefix(q, "correlation_id="), "^")
		res := []map[string]any{}
		for _, inc := range f.Incidents {
			if inc["correlation_id"] == corr {
				res = append(res, map[string]any{"number": inc["number"]})
			}
		}
		reply(w, 200, map[string]any{"result": res})
	case r.URL.Path == "/api/now/table/incident" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["number"] = fmt.Sprintf("INC%07d", len(f.Incidents)+1)
		f.Incidents = append(f.Incidents, body)
		if f.lostCreates > 0 {
			f.lostCreates--
			reply(w, 504, map[string]any{"error": map[string]string{"message": "gateway timeout"}})
			return
		}
		reply(w, 201, map[string]any{"result": map[string]any{"number": body["number"]}})
	default:
		reply(w, 404, map[string]any{"error": map[string]string{"message": "no route " + r.URL.Path}})
	}
}

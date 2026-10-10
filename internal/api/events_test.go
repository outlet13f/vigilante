package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vigilante/internal/events"
	"vigilante/internal/model"
)

// receiver is a webhook endpoint that checks signatures.
type receiver struct {
	t      *testing.T
	secret string
	fail   atomic.Bool
	mu     sync.Mutex
	got    []model.CloudEvent
	srv    *httptest.Server
}

func newReceiver(t *testing.T) *receiver {
	rc := &receiver{t: t}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if rc.fail.Load() {
			http.Error(w, "down for maintenance", 503)
			return
		}
		rc.mu.Lock()
		secret := rc.secret
		rc.mu.Unlock()
		if !events.Verify(secret, r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature"), body) {
			t.Errorf("bad signature on %s", body)
			http.Error(w, "bad signature", 401)
			return
		}
		if r.Header.Get("Content-Type") != "application/cloudevents+json" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		var ev model.CloudEvent
		_ = json.Unmarshal(body, &ev)
		if ev.ID != r.Header.Get("webhook-id") {
			t.Errorf("webhook-id %s != event id %s", r.Header.Get("webhook-id"), ev.ID)
		}
		rc.mu.Lock()
		rc.got = append(rc.got, ev)
		rc.mu.Unlock()
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *receiver) types() []string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []string
	for _, e := range rc.got {
		out = append(out, strings.TrimPrefix(e.Type, "vigilante."))
	}
	return out
}

func (rc *receiver) waitFor(t *testing.T, typ string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, got := range rc.types() {
			if got == typ {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s delivered; got %v", typ, rc.types())
}

func TestV2WebhooksDeliverInOrderSignedAndRecover(t *testing.T) {
	t.Setenv("VGL_TEST_WEBHOOK_KEY", "0123456789abcdef-master")
	viewerTok, viewer := sa(t, "team-search", "viewer", "team=search")
	s, base := newV2Server(t, "api: {webhook_signing_key_ref: \"env:VGL_TEST_WEBHOOK_KEY\"}\nauth:\n  service_accounts:\n"+viewer)
	s.bus.Backoff = []time.Duration{10 * time.Millisecond}
	s.bus.DisableAfter = 2
	admin := &v2Client{t: t, base: base, token: "tok"}
	rc := newReceiver(t)

	wantProblem(t, admin.do("POST", "/v2/webhooks", `{"url":"ftp://x"}`, nil), 422, "validation_failed")
	wantProblem(t, admin.do("POST", "/v2/webhooks", `{"url":"`+rc.srv.URL+`","types":["vigilante.nope"]}`, nil), 422, "validation_failed")
	// A team viewer cannot subscribe to every service, or to another team.
	team := &v2Client{t: t, base: base, token: viewerTok}
	wantProblem(t, team.do("POST", "/v2/webhooks", `{"url":"`+rc.srv.URL+`"}`, nil), 403, "forbidden")
	wantProblem(t, team.do("POST", "/v2/webhooks", `{"url":"`+rc.srv.URL+`","teams":["payments"]}`, nil), 403, "forbidden")
	if r := team.do("POST", "/v2/webhooks", `{"url":"`+rc.srv.URL+`","teams":["search"]}`, nil); r.StatusCode != 201 {
		t.Fatalf("team subscription: %d %s", r.StatusCode, r.Body)
	}

	r := admin.do("POST", "/v2/webhooks", `{"url":"`+rc.srv.URL+`","services":["svc"],"description":"incident bot"}`, nil)
	if r.StatusCode != 201 || !strings.HasPrefix(r.JSON["secret"].(string), "whsec_") || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %s", r.StatusCode, r.Body)
	}
	rc.mu.Lock()
	rc.secret = r.JSON["secret"].(string)
	rc.mu.Unlock()
	id := r.JSON["webhook"].(map[string]any)["id"].(string)

	// A failing canary produces the whole story, in order.
	admin.do("PUT", "/v2/services/svc/last-good", `{"version":"v1"}`, nil)
	admin.do("POST", "/v2/deployments", `{"id":"w1","service":"svc","version":"v2"}`, nil)
	admin.do("POST", "/v2/deployments/w1/observations", `{"phase":"canary"}`, nil)
	rc.waitFor(t, "rollback.completed")
	want := "deployment.marked_good deployment.created observation.started observation.failed rollback.started rollback.completed"
	if got := strings.Join(rc.types(), " "); got != want {
		t.Fatalf("delivered:\n got  %s\n want %s", got, want)
	}
	rc.mu.Lock()
	for i := 1; i < len(rc.got); i++ {
		if rc.got[i].Sequence <= rc.got[i-1].Sequence {
			t.Fatalf("out of order: %v", rc.got)
		}
	}
	rc.mu.Unlock()

	// Ping reaches only this subscription.
	if r := admin.do("POST", "/v2/webhooks/"+id+"/pings", "", nil); r.StatusCode != 202 || r.JSON["type"] != model.EvPing {
		t.Fatalf("ping: %d %s", r.StatusCode, r.Body)
	}
	rc.waitFor(t, "ping")

	// Receiver down: retries, dead letters, then automatic disable.
	rc.fail.Store(true)
	admin.do("POST", "/v2/circuit/trip", `{"reason":"maintenance"}`, nil)
	admin.do("POST", "/v2/circuit/reset", `{"reason":"done"}`, nil)
	deadline := time.Now().Add(15 * time.Second)
	var hook map[string]any
	for time.Now().Before(deadline) {
		hook = admin.do("GET", "/v2/webhooks/"+id, "", nil).JSON
		if hook["active"] == false {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if hook["active"] != false || !strings.Contains(hook["disabled_reason"].(string), "503") || len(hook["dead_letters"].([]any)) != 2 {
		t.Fatalf("not disabled after 2 dead letters: %v", hook)
	}
	dl := admin.do("GET", "/v2/webhooks/"+id+"/deliveries", "", nil)
	if items := dl.JSON["items"].([]any); len(items) < 4 || items[0].(map[string]any)["outcome"] != "dead" {
		t.Fatalf("deliveries: %s", dl.Body)
	}

	// Back up: re-enable, and redeliver the first dead letter.
	rc.fail.Store(false)
	first := hook["dead_letters"].([]any)[0].(map[string]any)
	seq := int64(first["sequence"].(float64))
	if r := admin.do("PATCH", "/v2/webhooks/"+id, `{"active":true}`, nil); r.JSON["active"] != true || r.JSON["consecutive_failures"].(float64) != 0 {
		t.Fatalf("re-enable: %s", r.Body)
	}
	if r := admin.do("POST", "/v2/webhooks/"+id+"/redeliveries", `{"sequence":`+strconv.FormatInt(seq, 10)+`}`, nil); r.StatusCode != 202 {
		t.Fatalf("redeliver: %d %s", r.StatusCode, r.Body)
	}
	rc.waitFor(t, "circuit.opened")
	if h := admin.do("GET", "/v2/webhooks/"+id, "", nil).JSON; len(h["dead_letters"].([]any)) != 1 {
		t.Fatalf("redelivered event leaves the dead-letter list: %v", h["dead_letters"])
	}
	wantProblem(t, admin.do("POST", "/v2/webhooks/"+id+"/redeliveries", `{"sequence":999999}`, nil), 422, "validation_failed")

	// Rotation changes the signature key.
	r = admin.do("POST", "/v2/webhooks/"+id+"/secret", "", nil)
	if r.StatusCode != 200 || r.JSON["secret"] == rc.secret {
		t.Fatalf("rotate: %s", r.Body)
	}
	rc.mu.Lock()
	rc.secret = r.JSON["secret"].(string)
	rc.mu.Unlock()
	admin.do("POST", "/v2/webhooks/"+id+"/pings", "", nil)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && strings.Count(strings.Join(rc.types(), " "), "ping") < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Count(strings.Join(rc.types(), " "), "ping") < 2 {
		t.Fatalf("ping after rotation not delivered: %v", rc.types())
	}

	// Ownership: the team viewer does not see or touch the admin's webhook.
	if r := team.do("GET", "/v2/webhooks", "", nil); len(r.JSON["items"].([]any)) != 1 {
		t.Fatalf("own subscriptions only: %s", r.Body)
	}
	wantProblem(t, team.do("DELETE", "/v2/webhooks/"+id, "", nil), 403, "forbidden")
	if r := admin.do("DELETE", "/v2/webhooks/"+id, "", nil); r.StatusCode != 204 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	wantProblem(t, admin.do("GET", "/v2/webhooks/"+id, "", nil), 404, "not_found")
	if r := admin.do("GET", "/v2/event-types", "", nil); len(r.JSON["items"].([]any)) != len(model.EventTypes) {
		t.Fatalf("catalog: %s", r.Body)
	}
}

func TestV2WebhooksNeedSigningKey(t *testing.T) {
	_, base := newV2Server(t, "")
	admin := &v2Client{t: t, base: base, token: "tok"}
	r := admin.do("POST", "/v2/webhooks", `{"url":"https://hooks.example.internal/x"}`, nil)
	wantProblem(t, r, 422, "validation_failed")
	if !strings.Contains(r.JSON["detail"].(string), "webhook_signing_key_ref") {
		t.Fatalf("detail: %s", r.Body)
	}
}

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	id, typ string
	data    model.CloudEvent
}

// readSSE reads until n events arrived or the timeout passes.
func readSSE(t *testing.T, url, token, lastID string, n int) []sseEvent {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("sse: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var out []sseEvent
	sc := bufio.NewScanner(resp.Body)
	var cur sseEvent
	for len(out) < n && sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			cur.typ = line[7:]
		case strings.HasPrefix(line, "data: "):
			_ = json.Unmarshal([]byte(line[6:]), &cur.data)
		case line == "" && cur.id != "":
			out = append(out, cur)
			cur = sseEvent{}
		}
	}
	return out
}

func TestV2EventStreamResumesAndFilters(t *testing.T) {
	searchTok, search := sa(t, "search-viewer", "viewer", "team=search")
	_, base := newV2Server(t, "auth:\n  service_accounts:\n"+search)
	admin := &v2Client{t: t, base: base, token: "tok"}
	admin.do("PUT", "/v2/services/svc/last-good", `{"version":"v1"}`, nil)
	admin.do("POST", "/v2/deployments", `{"id":"e1","service":"svc","version":"v2"}`, nil)
	admin.do("POST", "/v2/circuit/trip", `{"reason":"test"}`, nil)

	// From the start: marked_good, created, circuit.opened.
	got := readSSE(t, base+"/v2/events", "tok", "0", 3)
	if len(got) != 3 || got[0].typ != model.EvDeploymentMarkedGood || got[2].typ != model.EvCircuitOpened || got[2].id != strconv.FormatInt(got[2].data.Sequence, 10) {
		t.Fatalf("backlog: %+v", got)
	}
	// Resume after the second event: only the third, then live ones.
	go func() {
		time.Sleep(300 * time.Millisecond)
		admin.do("POST", "/v2/circuit/reset", `{"reason":"test over"}`, nil)
	}()
	got = readSSE(t, base+"/v2/events", "tok", got[1].id, 2)
	if len(got) != 2 || got[0].typ != model.EvCircuitOpened || got[1].typ != model.EvCircuitClosed {
		t.Fatalf("resume + live: %+v", got)
	}
	// A search-team viewer sees the global circuit events but not svc's.
	got = readSSE(t, base+"/v2/events?types=vigilante.circuit.opened,vigilante.deployment.created", searchTok, "0", 1)
	if len(got) != 1 || got[0].typ != model.EvCircuitOpened {
		t.Fatalf("scoped stream: %+v", got)
	}
	wantProblem(t, admin.do("GET", "/v2/events?types=bogus", "", nil), 400, "bad_request")
}

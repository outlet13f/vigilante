package probe

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/model"
	"vigilante/internal/tmpl"
	"vigilante/internal/transport"
)

var testLog = slog.New(slog.NewTextHandler(io.Discard, nil))

type sink struct {
	mu sync.Mutex
	s  []model.Sample
}

func (k *sink) add(s model.Sample) { k.mu.Lock(); k.s = append(k.s, s); k.mu.Unlock() }

func (k *sink) values(metric string) []float64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []float64
	for _, s := range k.s {
		if s.Metric == metric {
			out = append(out, s.Value)
		}
	}
	return out
}

func sum(xs []float64) float64 {
	t := 0.0
	for _, x := range xs {
		t += x
	}
	return t
}

func runFor(t *testing.T, d time.Duration, jobs []Job, runner transport.Runner) *sink {
	t.Helper()
	k := &sink{}
	c := &Collector{Sink: k.add, Log: testLog, Runners: func(string) (transport.Runner, error) {
		if runner == nil {
			return nil, transport.ErrNoRunner
		}
		return runner, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	c.Run(ctx, jobs)
	return k
}

func TestHTTPProbeAndJSONPath(t *testing.T) {
	status := "UP"
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"status": status, "components": map[string]any{"db": map[string]any{"status": status}}})
	}))
	defer srv.Close()
	spec := config.Probe{ID: "health", Type: "http", Interval: 20 * time.Millisecond, Timeout: time.Second,
		HTTP: &config.HTTPProbe{URL: srv.URL + "/{{.Name}}", Method: "GET", JSONPath: "components.db.status", JSONExpect: "UP"}}
	k := runFor(t, 120*time.Millisecond, []Job{{Target: config.Target{Name: "a"}, Spec: spec}}, nil)
	if ups := k.values("health.up"); len(ups) < 3 || sum(ups) != float64(len(ups)) {
		t.Fatalf("up samples %v", ups)
	}
	if lat := k.values("health.latency_ms"); len(lat) == 0 {
		t.Fatal("no latency samples")
	}
	mu.Lock()
	status = "DOWN"
	mu.Unlock()
	k = runFor(t, 120*time.Millisecond, []Job{{Target: config.Target{Name: "a"}, Spec: spec}}, nil)
	cf := k.values("health.consecutive_failures")
	if len(cf) < 3 || cf[len(cf)-1] < 3 {
		t.Fatalf("consecutive failures %v", cf)
	}
}

func TestHTTPTimeoutsCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()
	spec := config.Probe{ID: "h", Type: "http", Interval: 10 * time.Millisecond, Timeout: 20 * time.Millisecond, HTTP: &config.HTTPProbe{URL: srv.URL, Method: "GET"}}
	k := runFor(t, 200*time.Millisecond, []Job{{Target: config.Target{Name: "a"}, Spec: spec}}, nil)
	ct := k.values("h.consecutive_timeouts")
	if len(ct) == 0 || ct[len(ct)-1] < 2 {
		t.Fatalf("timeouts %v", ct)
	}
}

func TestParseAccessLine(t *testing.T) {
	comb := &config.AccessLogProbe{Format: "combined", LatencyUnit: "s"}
	st, lat, ok := parseAccessLine(comb, `10.0.0.9 - - [09/Oct/2026:22:00:01 +0900] "GET /api/orders HTTP/1.1" 503 12 "-" "curl/8" 0.254`)
	if !ok || st != 503 || lat != 254 {
		t.Fatalf("combined: %d %v %v", st, lat, ok)
	}
	st, lat, ok = parseAccessLine(comb, `10.0.0.9 - - [09/Oct/2026:22:00:01 +0900] "GET / HTTP/1.1" 200 12`)
	if !ok || st != 200 || lat != -1 {
		t.Fatalf("common: %d %v %v", st, lat, ok)
	}
	js := &config.AccessLogProbe{Format: "json", StatusField: "status", LatencyField: "duration_ms", LatencyUnit: "ms"}
	st, lat, ok = parseAccessLine(js, `{"status":"502","duration_ms":31.5}`)
	if !ok || st != 502 || lat != 31.5 {
		t.Fatalf("json: %d %v %v", st, lat, ok)
	}
	if _, _, ok := parseAccessLine(comb, "garbage"); ok {
		t.Fatal("garbage parsed")
	}
}

func TestAccessAndAppLogLocalFollowWithRotation(t *testing.T) {
	dir := t.TempDir()
	access := filepath.Join(dir, "access.log")
	app := filepath.Join(dir, "app.log")
	os.WriteFile(access, []byte("old line ignored\n"), 0o644)
	os.WriteFile(app, nil, 0o644)
	jobs := []Job{
		{Target: config.Target{Name: "a"}, Spec: config.Probe{ID: "access", Type: "access_log", AccessLog: &config.AccessLogProbe{Path: access, Format: "combined", LatencyUnit: "s"}}},
		{Target: config.Target{Name: "a"}, Spec: config.Probe{ID: "applog", Type: "log", Log: &config.LogProbe{Path: app, Patterns: map[string]string{"oom": "OutOfMemoryError"}}}},
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		f, _ := os.OpenFile(access, os.O_APPEND|os.O_WRONLY, 0)
		for i := 0; i < 10; i++ {
			code := 200
			if i < 3 {
				code = 500
			}
			io.WriteString(f, `1.2.3.4 - - [09/Oct/2026:22:00:01 +0900] "GET / HTTP/1.1" `+strconv.Itoa(code)+" 5 \"-\" \"x\" 0.010\n")
		}
		f.Close()
		time.Sleep(600 * time.Millisecond)
		// logrotate (copytruncate style): truncate and write fresh lines
		os.WriteFile(access, []byte(`1.2.3.4 - - [09/Oct/2026:22:00:02 +0900] "GET / HTTP/1.1" 502 5 "-" "x" 0.020`+"\n"), 0o644)
		a, _ := os.OpenFile(app, os.O_APPEND|os.O_WRONLY, 0)
		io.WriteString(a, "INFO ok\njava.lang.OutOfMemoryError: Java heap space\n")
		a.Close()
	}()
	k := runFor(t, 2500*time.Millisecond, jobs, &transport.Local{})
	if got := sum(k.values("access.requests")); got != 11 {
		t.Errorf("requests = %v, want 11", got)
	}
	if got := sum(k.values("access.count_5xx")); got != 4 {
		t.Errorf("5xx = %v, want 4", got)
	}
	if got := sum(k.values("applog.match.oom")); got != 1 {
		t.Errorf("oom matches = %v", got)
	}
	if lat := k.values("access.latency_ms"); len(lat) != 11 {
		t.Errorf("latency samples = %d", len(lat))
	}
}

func TestHostParse(t *testing.T) {
	out := "1.50 1.20 1.00 2/300 12345\n@@\ncpu  100 0 100 700 100 0 0 0 0 0\n@@\nMemTotal:       8000000 kB\nMemFree:        1000000 kB\nMemAvailable:   2000000 kB\n@@\n" +
		"   8       0 sda 100 0 0 0 100 0 0 0 0 5000 0\n   8       1 sda1 1 0 0 0 1 0 0 0 0 4000 0\n 259 0 nvme0n1 1 0 0 0 1 0 0 0 0 100 0\n@@\n4\n"
	snap, m, err := parseHost(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m["load1"] != 1.5 || m["load_per_cpu"] != 0.375 || m["mem_available_pct"] != 25 {
		t.Fatalf("metrics %v", m)
	}
	if snap.ioTicks["sda"] != 5000 || snap.ioTicks["nvme0n1"] != 100 || len(snap.ioTicks) != 2 {
		t.Fatalf("io ticks %v", snap.ioTicks)
	}
	if snap.cpuTotal != 1000 || snap.cpuIdle != 800 {
		t.Fatalf("cpu %d/%d", snap.cpuIdle, snap.cpuTotal)
	}
}

func TestHostProbeOverRunner(t *testing.T) {
	m := (&transport.Mock{}).On("/proc/loadavg", "0.5 0 0 1/1 1\n@@\ncpu 1 1 1 1 1 0 0 0\n@@\nMemTotal: 100 kB\nMemAvailable: 3 kB\n@@\n@@\n2\n", nil)
	spec := config.Probe{ID: "host", Type: "host", Interval: 20 * time.Millisecond, Timeout: time.Second}
	k := runFor(t, 70*time.Millisecond, []Job{{Target: config.Target{Name: "a"}, Spec: spec}}, m)
	if v := k.values("host.mem_available_pct"); len(v) == 0 || v[0] != 3 {
		t.Fatalf("mem %v", v)
	}
}

func TestDockerProbe(t *testing.T) {
	restarts := 1
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/pay-api/json"):
			mu.Lock()
			defer mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"Id": "x", "RestartCount": restarts,
				"State": map[string]any{"Running": true, "OOMKilled": restarts > 2, "Health": map[string]any{"Status": "healthy"}}})
			restarts++
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.(http.Flusher).Flush()
			io.WriteString(w, `{"Type":"container","Action":"oom","Actor":{"ID":"x"}}`+"\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	defer srv.Close()
	spec := config.Probe{ID: "ctr", Type: "docker", Interval: 20 * time.Millisecond, Timeout: time.Second,
		Docker: &config.DockerProbe{Host: srv.URL, Container: "{{.Name}}"}}
	k := runFor(t, 120*time.Millisecond, []Job{{Target: config.Target{Name: "pay-api"}, Spec: spec}}, nil)
	r := k.values("ctr.restarts")
	if len(r) < 3 || r[0] != 0 || r[len(r)-1] < 2 {
		t.Fatalf("restarts delta %v", r)
	}
	if sum(k.values("ctr.oom_events")) != 1 {
		t.Fatalf("oom events %v", k.values("ctr.oom_events"))
	}
	if oom := k.values("ctr.oom_killed"); oom[len(oom)-1] != 1 {
		t.Fatalf("oom_killed %v", oom)
	}
}

func TestCollectorRestartsFailingProbe(t *testing.T) {
	spec := config.Probe{ID: "app", Type: "log", Log: &config.LogProbe{Path: "/x", Patterns: map[string]string{"e": "E"}}}
	// No runner: the log probe fails to start; the collector reports probe_error and keeps retrying.
	k := runFor(t, 1500*time.Millisecond, []Job{{Target: config.Target{Name: "a"}, Spec: spec}}, nil)
	if len(k.values("app.probe_error")) < 1 {
		t.Fatal("expected probe_error samples")
	}
}

func TestTemplateDataInURL(t *testing.T) {
	d := tmpl.ForTarget(config.Target{Name: "web-1", Address: "10.0.0.7", Labels: map[string]string{"port": "8081"}})
	got, err := tmpl.Render("http://{{.Address}}:{{.Labels.port}}/health", d)
	if err != nil || got != "http://10.0.0.7:8081/health" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := tmpl.Render("{{.Labels.missing}}", d); err == nil {
		t.Fatal("missing label must be an error, not an empty string")
	}
}

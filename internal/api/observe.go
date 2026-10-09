package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"vigilante/internal/auth"
	"vigilante/internal/safety"
	"vigilante/internal/telemetry"
)

// Self-observability endpoints. Like /healthz they are answered by every
// node itself and never forwarded to the HA leader.
var local = map[string]bool{"/healthz": true, "/readyz": true, "/metrics": true}

var processStart = time.Now()

// readyz reports whether this node can serve: the state store answers, and
// in HA mode a leader is known (followers forward every call to it).
// /healthz stays a pure liveness check.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	ready := true
	checks := map[string]string{"store": "ok"}
	if err := s.E.Journal.Ping(ctx); err != nil {
		ready = false
		checks["store"] = "unreachable"
		s.E.Log.Warn("readiness: state store unreachable", "store", s.E.Journal.Describe(), "err", err)
	}
	if s.HA != nil {
		node, addr := s.HA.Leader()
		switch {
		case s.HA.IsLeader():
			checks["leader"] = "self"
		case addr != "":
			checks["leader"] = node
		default:
			ready = false
			checks["leader"] = "none"
		}
	}
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"ready": ready, "checks": checks})
}

// metricsHandler serves the Prometheus text format. Unless
// server.metrics_public is set, the scraper needs a viewer token for all
// services (metrics carry service names).
func (s *Server) metricsHandler() http.HandlerFunc {
	serve := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", telemetry.ContentType)
		telemetry.Write(w, s.scrapeSamples())
	}
	if s.E.Cfg.Server.MetricsPublic {
		return serve
	}
	return s.authn(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.allow(w, r, auth.ActRead, auth.Service{}); ok {
			serve(w, r)
		}
	})
}

// scrapeSamples are point-in-time gauges owned by this node.
func (s *Server) scrapeSamples() []telemetry.Sample {
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	leader := s.HA == nil || s.HA.IsLeader()
	out := []telemetry.Sample{
		{Name: "vigilante_build_info", Help: "Build information.", Labels: map[string]string{"version": telemetry.Version, "go_version": runtime.Version()}, Value: 1},
		{Name: "vigilante_leader", Help: "1 if this node is the HA leader (always 1 without HA).", Value: b2f(leader)},
		{Name: "vigilante_engine_active", Help: "1 if this node may judge and roll back (leader, not fenced).", Value: b2f(s.E.Active())},
		{Name: "vigilante_dry_run", Help: "1 if changing actions are only logged.", Value: b2f(s.E.DryRun)},
		{Name: "process_start_time_seconds", Help: "Start time of the process since unix epoch in seconds.", Value: float64(processStart.Unix())},
		{Name: "go_goroutines", Help: "Number of goroutines that currently exist.", Value: float64(runtime.NumGoroutine())},
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out = append(out, telemetry.Sample{Name: "go_memstats_heap_alloc_bytes", Help: "Bytes of allocated heap objects.", Value: float64(ms.HeapAlloc)})

	cur := s.E.Breaker.State().State
	for _, st := range []safety.CircuitStateName{safety.Closed, safety.Open, safety.HalfOpen} {
		out = append(out, telemetry.Sample{Name: "vigilante_circuit_state", Help: "Safety circuit state (1 = current).",
			Labels: map[string]string{"state": strings.ToLower(string(st))}, Value: b2f(cur == st)})
	}
	byState := map[string]float64{}
	for _, d := range s.E.Deployments() {
		byState[string(d.State)]++
	}
	for st, n := range byState {
		out = append(out, telemetry.Sample{Name: "vigilante_deployments", Help: "Known deployments by state.", Labels: map[string]string{"state": st}, Value: n})
	}
	s.mu.Lock()
	agents := 0
	for _, t := range s.agents {
		if time.Since(t) < 30*time.Second {
			agents++
		}
	}
	s.mu.Unlock()
	out = append(out, telemetry.Sample{Name: "vigilante_agents_connected", Help: "Agents with a heartbeat in the last 30s.", Value: float64(agents)})
	return out
}

// statusWriter records the response code; Unwrap keeps http.ResponseController
// (flush for streaming proxies) working.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

const requestIDHeader = "X-Request-ID"

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// requestID reuses the caller's X-Request-ID, else the W3C traceparent trace
// id, else makes one. A follower forwards the header, so one request carries
// the same id in the logs of both nodes.
func requestID(r *http.Request) string {
	if id := r.Header.Get(requestIDHeader); validRequestID.MatchString(id) {
		return id
	}
	if tp := strings.Split(r.Header.Get("traceparent"), "-"); len(tp) == 4 && len(tp[1]) == 32 {
		return tp[1]
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// instrument assigns a request id, counts and times requests per route, and
// logs them: changes always, reads only when they fail (debug otherwise).
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestID(r)
		r.Header.Set(requestIDHeader, id)
		w.Header().Set(requestIDHeader, id)
		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(sw, r)
		if sw.code == 0 {
			sw.code = http.StatusOK
		}
		route := r.Pattern
		if route == "" {
			route = "forwarded"
			if !local[r.URL.Path] && (s.HA == nil || s.HA.IsLeader()) {
				route = "unmatched"
			}
		} else if _, path, ok := strings.Cut(route, " "); ok {
			route = path
		}
		telemetry.APIRequests.Inc(r.Method, route, strconv.Itoa(sw.code))
		telemetry.APILatency.Since(start, route)
		lvl := slog.LevelDebug
		if r.Method != http.MethodGet || sw.code >= 400 {
			lvl = slog.LevelInfo
		}
		if local[r.URL.Path] && sw.code < 400 {
			return
		}
		s.E.Log.Log(r.Context(), lvl, "api request", "request_id", id, "method", r.Method, "route", route,
			"code", sw.code, "ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}

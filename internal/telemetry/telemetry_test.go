package telemetry

import (
	"strings"
	"testing"
)

func TestExpositionFormat(t *testing.T) {
	c := NewCounter("test_requests_total", "Requests.", "route", "code")
	c.Inc("/v1/x", "200")
	c.Add(2, "/v1/x", "200")
	c.Inc(`/q"uote`, "500")
	g := NewGauge("test_temperature", "Temp.")
	g.Set(-1.5)
	h := NewHistogram("test_latency_seconds", "Latency.", []float64{0.1, 1}, "route")
	h.Observe(0.05, "/a")
	h.Observe(0.5, "/a")
	h.Observe(5, "/a")

	var b strings.Builder
	Write(&b, []Sample{{Name: "test_leader", Help: "Leader.", Value: 1},
		{Name: "test_state", Help: "State.", Labels: map[string]string{"state": "open"}, Value: 0},
		{Name: "test_state", Help: "State.", Labels: map[string]string{"state": "closed"}, Value: 1}})
	out := b.String()
	for _, want := range []string{
		"# HELP test_requests_total Requests.\n# TYPE test_requests_total counter\n",
		`test_requests_total{route="/v1/x",code="200"} 3` + "\n",
		`test_requests_total{route="/q\"uote",code="500"} 1` + "\n",
		"# TYPE test_temperature gauge\ntest_temperature -1.5\n",
		"# TYPE test_latency_seconds histogram\n",
		`test_latency_seconds_bucket{route="/a",le="0.1"} 1` + "\n",
		`test_latency_seconds_bucket{route="/a",le="1"} 2` + "\n",
		`test_latency_seconds_bucket{route="/a",le="+Inf"} 3` + "\n",
		`test_latency_seconds_sum{route="/a"} 5.55` + "\n",
		`test_latency_seconds_count{route="/a"} 3` + "\n",
		"# TYPE test_leader gauge\ntest_leader 1\n",
		"# TYPE test_state gauge\ntest_state{state=\"open\"} 0\ntest_state{state=\"closed\"} 1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "# TYPE test_state ") != 1 {
		t.Error("one TYPE line per metric family")
	}
	if c.Value("/v1/x", "200") != 3 || h.Count("/a") != 3 {
		t.Error("readback")
	}
}

func TestDuplicateAndLabelArityPanic(t *testing.T) {
	NewCounter("test_dup_total", "x")
	for name, f := range map[string]func(){
		"duplicate": func() { NewCounter("test_dup_total", "x") },
		"arity":     func() { ProbeSamples.Inc() },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s must panic", name)
				}
			}()
			f()
		}()
	}
}

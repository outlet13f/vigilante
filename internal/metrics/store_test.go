package metrics

import (
	"testing"
	"time"

	"vigilante/internal/model"
)

func TestAggregations(t *testing.T) {
	s := NewStore(time.Hour)
	now := time.Now()
	for i := 1; i <= 100; i++ {
		s.Add(model.Sample{Target: "a", Metric: "lat", Value: float64(i), Time: now.Add(time.Duration(i-100) * 100 * time.Millisecond)})
	}
	pts := s.Window("a", "lat", time.Minute, now, "")
	if len(pts) != 100 {
		t.Fatalf("window len = %d", len(pts))
	}
	cases := map[string]float64{"p50": 50, "p95": 95, "p99": 99, "max": 100, "min": 1, "avg": 50.5, "count": 100, "last": 100, "sum": 5050}
	for agg, want := range cases {
		got, ok := Aggregate(pts, agg, time.Minute)
		if !ok || got != want {
			t.Errorf("%s = %v (ok=%v), want %v", agg, got, ok, want)
		}
	}
	if r, _ := Aggregate(pts, "rate", 10*time.Second); r != 505 {
		t.Errorf("rate = %v, want 505", r)
	}
}

func TestWindowBoundsAndSource(t *testing.T) {
	s := NewStore(time.Hour)
	now := time.Now()
	s.Add(model.Sample{Target: "a", Metric: "m", Value: 1, Time: now.Add(-2 * time.Minute)})
	s.Add(model.Sample{Target: "a", Metric: "m", Value: 2, Time: now.Add(-10 * time.Second), Source: "agent:a"})
	s.Add(model.Sample{Target: "a", Metric: "m", Value: 3, Time: now.Add(-5 * time.Second)})
	if n := len(s.Window("a", "m", time.Minute, now, "")); n != 2 {
		t.Fatalf("got %d points in 1m window, want 2", n)
	}
	if pts := s.Window("a", "m", time.Minute, now, "agent:a"); len(pts) != 1 || pts[0].V != 2 {
		t.Fatalf("source filter: %+v", pts)
	}
	if src := s.Sources("a", "m", time.Minute, now); len(src) != 2 {
		t.Fatalf("sources = %v", src)
	}
}

func TestOutOfOrderAndRetention(t *testing.T) {
	s := NewStore(time.Minute)
	now := time.Now()
	s.Add(model.Sample{Target: "a", Metric: "m", Value: 2, Time: now})
	s.Add(model.Sample{Target: "a", Metric: "m", Value: 1, Time: now.Add(-time.Second)})
	pts := s.Window("a", "m", time.Hour, now, "")
	if pts[0].V != 1 || pts[1].V != 2 {
		t.Fatalf("not sorted: %+v", pts)
	}
	s.Add(model.Sample{Target: "a", Metric: "m", Value: 9, Time: now.Add(2 * time.Minute)})
	if n := len(s.Window("a", "m", time.Hour, now.Add(2*time.Minute), "")); n != 1 {
		t.Fatalf("retention kept %d points", n)
	}
}

func TestCounterAggOnEmpty(t *testing.T) {
	if v, ok := Aggregate(nil, "rate", time.Second); !ok || v != 0 {
		t.Fatal("rate over no events should be 0")
	}
	if _, ok := Aggregate(nil, "p99", time.Second); ok {
		t.Fatal("p99 over no samples must be unknown")
	}
}

// A full series must not copy itself on every insert (that made a 256/s
// latency stream O(n^2) once the series reached MaxPoints or Retention).
func TestSustainedHighRateStaysLinear(t *testing.T) {
	s := NewStore(30 * time.Minute)
	s.MaxPoints = 50_000
	start := time.Now().Add(-time.Hour)
	begin := time.Now()
	for i := 0; i < 400_000; i++ { // 400k samples at 256/s ~ 26 minutes of data
		s.Add(model.Sample{Target: "t", Metric: "lat", Value: float64(i), Time: start.Add(time.Duration(i) * 4 * time.Millisecond)})
	}
	if el := time.Since(begin); el > 5*time.Second {
		t.Fatalf("400k inserts took %s", el)
	}
	last := start.Add(399_999 * 4 * time.Millisecond)
	pts := s.Window("t", "lat", time.Hour, last, "")
	if len(pts) < 50_000 || len(pts) > 50_000*4/3+1 {
		t.Fatalf("series length %d not bounded near MaxPoints", len(pts))
	}
	if pts[len(pts)-1].V != 399_999 {
		t.Fatal("newest point lost")
	}
}

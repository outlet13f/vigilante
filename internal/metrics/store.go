// Package metrics is the engine's built-in time-series store: per (target,
// metric) ring buffers with windowed aggregation (avg, rate, p95, p99, ...).
// It replaces an external TSDB for the short observation windows a rollout needs.
package metrics

import (
	"math"
	"sort"
	"sync"
	"time"

	"vigilante/internal/model"
)

type Point struct {
	T      time.Time
	V      float64
	Source string
}

type series struct {
	pts []Point // sorted by time (probes emit roughly in order; we insert-sort the tail)
}

// Store is safe for concurrent use. Points older than Retention are pruned on insert.
type Store struct {
	mu        sync.RWMutex
	data      map[string]map[string]*series // target -> metric -> series
	Retention time.Duration
	MaxPoints int // per series; may be exceeded by up to a third before compaction
}

func NewStore(retention time.Duration) *Store {
	return &Store{data: map[string]map[string]*series{}, Retention: retention, MaxPoints: 200_000}
}

func (s *Store) Add(sm model.Sample) {
	if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
		return
	}
	src := sm.Source
	if src == "" {
		src = model.SourceCentral
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byMetric, ok := s.data[sm.Target]
	if !ok {
		byMetric = map[string]*series{}
		s.data[sm.Target] = byMetric
	}
	ser, ok := byMetric[sm.Metric]
	if !ok {
		ser = &series{}
		byMetric[sm.Metric] = ser
	}
	p := Point{T: sm.Time, V: sm.Value, Source: src}
	n := len(ser.pts)
	ser.pts = append(ser.pts, p)
	for i := n; i > 0 && ser.pts[i-1].T.After(ser.pts[i].T); i-- {
		ser.pts[i-1], ser.pts[i] = ser.pts[i], ser.pts[i-1]
	}
	// Prune lazily. Once a series is full, every insert expires one point;
	// copying the series each time would cost O(n) per sample. Instead the
	// expired prefix is left in place (queries filter by time) and dropped in
	// one copy when it reaches a quarter of the series.
	cut := 0
	if s.Retention > 0 {
		limit := sm.Time.Add(-s.Retention)
		cut = sort.Search(len(ser.pts), func(i int) bool { return !ser.pts[i].T.Before(limit) })
	}
	if over := len(ser.pts) - cut - s.MaxPoints; over > 0 {
		cut += over
	}
	if cut > 0 && cut >= max(1024, len(ser.pts)/4) {
		ser.pts = append(ser.pts[:0:0], ser.pts[cut:]...)
	}
}

// live returns the points within Retention of the newest point, hiding the
// expired prefix that Add keeps until its next compaction.
func (s *Store) live(ser *series) []Point {
	if s.Retention <= 0 || len(ser.pts) == 0 {
		return ser.pts
	}
	limit := ser.pts[len(ser.pts)-1].T.Add(-s.Retention)
	lo := sort.Search(len(ser.pts), func(i int) bool { return !ser.pts[i].T.Before(limit) })
	return ser.pts[lo:]
}

// Window returns a copy of the points in (to-d, to] for one target/metric.
// source == "" selects all vantage points.
func (s *Store) Window(target, metric string, d time.Duration, to time.Time, source string) []Point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ser := s.data[target][metric]
	if ser == nil {
		return nil
	}
	pts := s.live(ser)
	from := to.Add(-d)
	lo := sort.Search(len(pts), func(i int) bool { return pts[i].T.After(from) })
	var out []Point
	for _, p := range pts[lo:] {
		if p.T.After(to) {
			break
		}
		if source == "" || p.Source == source {
			out = append(out, p)
		}
	}
	return out
}

// Sources lists the vantage points that reported `metric` for `target` within the window.
func (s *Store) Sources(target, metric string, d time.Duration, to time.Time) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range s.Window(target, metric, d, to, "") {
		if !seen[p.Source] {
			seen[p.Source] = true
			out = append(out, p.Source)
		}
	}
	sort.Strings(out)
	return out
}

// Count returns the number of points recorded for the given targets since `since`.
func (s *Store) Count(targets []string, since time.Time) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, t := range targets {
		for _, ser := range s.data[t] {
			pts := s.live(ser)
			lo := sort.Search(len(pts), func(i int) bool { return !pts[i].T.Before(since) })
			n += len(pts) - lo
		}
	}
	return n
}

// Metrics lists metric names known for a target (used by the status API).
func (s *Store) Metrics(target string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for m := range s.data[target] {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Aggregate reduces points with the named aggregation. `window` is used by
// "rate" (events per second). ok is false when there are no points.
func Aggregate(pts []Point, agg string, window time.Duration) (v float64, ok bool) {
	if len(pts) == 0 {
		if agg == "count" || agg == "rate" || agg == "sum" {
			return 0, true // absence of events is a valid zero for counters
		}
		return 0, false
	}
	switch agg {
	case "last":
		return pts[len(pts)-1].V, true
	case "count":
		return float64(len(pts)), true
	case "sum", "rate":
		sum := 0.0
		for _, p := range pts {
			sum += p.V
		}
		if agg == "rate" {
			if window <= 0 {
				return 0, false
			}
			return sum / window.Seconds(), true
		}
		return sum, true
	case "avg":
		sum := 0.0
		for _, p := range pts {
			sum += p.V
		}
		return sum / float64(len(pts)), true
	case "min", "max":
		v = pts[0].V
		for _, p := range pts[1:] {
			if (agg == "min" && p.V < v) || (agg == "max" && p.V > v) {
				v = p.V
			}
		}
		return v, true
	case "p50":
		return Percentile(pts, 50), true
	case "p90":
		return Percentile(pts, 90), true
	case "p95":
		return Percentile(pts, 95), true
	case "p99":
		return Percentile(pts, 99), true
	}
	return 0, false
}

// Percentile uses the nearest-rank method on a sorted copy.
func Percentile(pts []Point, p float64) float64 {
	if len(pts) == 0 {
		return 0
	}
	vals := make([]float64, len(pts))
	for i, pt := range pts {
		vals[i] = pt.V
	}
	sort.Float64s(vals)
	rank := int(math.Ceil(p/100*float64(len(vals)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(vals) {
		rank = len(vals) - 1
	}
	return vals[rank]
}

// IsCounterAgg reports aggregations for which "no samples" means zero, not unknown.
func IsCounterAgg(agg string) bool { return agg == "count" || agg == "rate" || agg == "sum" }

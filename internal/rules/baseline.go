package rules

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/metrics"
)

// BaselineKey identifies the (metric, aggregation) a baseline value belongs to.
func BaselineKey(c config.Condition) string {
	if c.RatioOf != "" {
		return c.Metric + "/" + c.RatioOf
	}
	return c.Metric + "|" + c.Agg
}

// Snapshot is a baseline captured before the deployment (pre-deploy window)
// and persisted as JSON so separate CI steps can share it.
type Snapshot struct {
	Service    string             `json:"service"`
	CapturedAt time.Time          `json:"captured_at"`
	Window     string             `json:"window"`
	Targets    []string           `json:"targets"`
	Values     map[string]float64 `json:"values"`
	Samples    int                `json:"samples"`
}

func (s *Snapshot) Lookup(c config.Condition, _ time.Time) (float64, bool) {
	if s == nil {
		return 0, false
	}
	v, ok := s.Values[BaselineKey(c)]
	return v, ok
}

// Capture aggregates every baseline-referenced metric across targets over the
// trailing window. Each condition uses the full baseline window rather than its
// own evaluation window, which gives a steadier reference.
func Capture(store *metrics.Store, service string, rs []config.Rule, targets []string, window time.Duration, now time.Time) *Snapshot {
	snap := &Snapshot{Service: service, CapturedAt: now, Window: window.String(), Targets: targets, Values: map[string]float64{}}
	snap.Samples = store.Count(targets, now.Add(-window))
	for _, c := range BaselineConditions(rs) {
		c.Window = window
		if v, ok := valueOver(store, c, targets, "", now); ok {
			snap.Values[BaselineKey(c)] = v
		}
	}
	return snap
}

func (s *Snapshot) Save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func LoadSnapshot(path string) (*Snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("baseline %s: %w", path, err)
	}
	return &s, nil
}

// Control compares against untouched control targets observed in the same
// window, so diurnal traffic changes do not look like regressions.
type Control struct {
	Store   *metrics.Store
	Targets []string
}

func (c *Control) Lookup(cond config.Condition, now time.Time) (float64, bool) {
	if c == nil || len(c.Targets) == 0 {
		return 0, false
	}
	return valueOver(c.Store, cond, c.Targets, "", now)
}

// Chain tries baselines in order (e.g. live control group first, then the
// pre-deploy snapshot).
type Chain []Baseline

func (ch Chain) Lookup(c config.Condition, now time.Time) (float64, bool) {
	for _, b := range ch {
		if b == nil {
			continue
		}
		if v, ok := b.Lookup(c, now); ok {
			return v, true
		}
	}
	return 0, false
}

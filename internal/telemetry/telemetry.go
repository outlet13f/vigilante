// Package telemetry is vigilante's own instrumentation: a small Prometheus
// text-format registry (no client library, no external stack needed) that
// the server exposes at /metrics. Packages declare metrics as globals and
// increment them; the server adds point-in-time gauges at scrape time.
package telemetry

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type kind string

const (
	counter   kind = "counter"
	gauge     kind = "gauge"
	histogram kind = "histogram"
)

type metric interface {
	desc() (name, help string, k kind)
	write(w io.Writer)
}

var (
	regMu    sync.Mutex
	registry = map[string]metric{}
)

func register(m metric) {
	name, _, _ := m.desc()
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("telemetry: duplicate metric " + name)
	}
	registry[name] = m
}

// Vec is a counter or gauge with labels.
type Vec struct {
	name, help string
	kind       kind
	labels     []string
	mu         sync.Mutex
	vals       map[string]float64 // joined label values -> value
}

// NewCounter declares a monotonically increasing counter.
func NewCounter(name, help string, labels ...string) *Vec {
	v := &Vec{name: name, help: help, kind: counter, labels: labels, vals: map[string]float64{}}
	register(v)
	return v
}

// NewGauge declares a value that goes up and down.
func NewGauge(name, help string, labels ...string) *Vec {
	v := &Vec{name: name, help: help, kind: gauge, labels: labels, vals: map[string]float64{}}
	register(v)
	return v
}

func key(n int, lv []string) string {
	if len(lv) != n {
		panic(fmt.Sprintf("telemetry: want %d label values, got %d", n, len(lv)))
	}
	return strings.Join(lv, "\x00")
}

// Add adds d (counters: d >= 0) for the label values.
func (v *Vec) Add(d float64, lv ...string) {
	k := key(len(v.labels), lv)
	v.mu.Lock()
	v.vals[k] += d
	v.mu.Unlock()
}

func (v *Vec) Inc(lv ...string) { v.Add(1, lv...) }

// Set sets a gauge.
func (v *Vec) Set(x float64, lv ...string) {
	k := key(len(v.labels), lv)
	v.mu.Lock()
	v.vals[k] = x
	v.mu.Unlock()
}

// Value reads the current value (tests, /healthz).
func (v *Vec) Value(lv ...string) float64 {
	k := key(len(v.labels), lv)
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.vals[k]
}

func (v *Vec) desc() (string, string, kind) { return v.name, v.help, v.kind }

func (v *Vec) write(w io.Writer) {
	v.mu.Lock()
	keys := make([]string, 0, len(v.vals))
	for k := range v.vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %s\n", v.name, labelStr(v.labels, splitKey(k, len(v.labels)), "", ""), num(v.vals[k]))
	}
	v.mu.Unlock()
}

// Histogram records observations in cumulative buckets.
type Histogram struct {
	name, help string
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*hseries
}

type hseries struct {
	counts []uint64 // per bucket, non-cumulative
	sum    float64
	n      uint64
}

// DurationBuckets suit operations from milliseconds to minutes (seconds).
var DurationBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

func NewHistogram(name, help string, buckets []float64, labels ...string) *Histogram {
	h := &Histogram{name: name, help: help, labels: labels, buckets: buckets, series: map[string]*hseries{}}
	register(h)
	return h
}

func (h *Histogram) Observe(x float64, lv ...string) {
	k := key(len(h.labels), lv)
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.series[k]
	if !ok {
		s = &hseries{counts: make([]uint64, len(h.buckets))}
		h.series[k] = s
	}
	for i, b := range h.buckets {
		if x <= b {
			s.counts[i]++
			break
		}
	}
	s.sum += x
	s.n++
}

// Since observes the seconds elapsed since t.
func (h *Histogram) Since(t time.Time, lv ...string) { h.Observe(time.Since(t).Seconds(), lv...) }

// Count reads the number of observations (tests).
func (h *Histogram) Count(lv ...string) uint64 {
	k := key(len(h.labels), lv)
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.series[k]; ok {
		return s.n
	}
	return 0
}

func (h *Histogram) desc() (string, string, kind) { return h.name, h.help, histogram }

func (h *Histogram) write(w io.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s, lv := h.series[k], splitKey(k, len(h.labels))
		var cum uint64
		for i, b := range h.buckets {
			cum += s.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelStr(h.labels, lv, "le", num(b)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelStr(h.labels, lv, "le", "+Inf"), s.n)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.name, labelStr(h.labels, lv, "", ""), num(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.name, labelStr(h.labels, lv, "", ""), s.n)
	}
}

func splitKey(k string, n int) []string {
	if n == 0 {
		return nil
	}
	return strings.Split(k, "\x00")
}

func labelStr(names, vals []string, extraName, extraVal string) string {
	if len(names) == 0 && extraName == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escape(vals[i]))
		b.WriteByte('"')
	}
	if extraName != "" {
		if len(names) > 0 {
			b.WriteByte(',')
		}
		b.WriteString(extraName)
		b.WriteString(`="`)
		b.WriteString(extraVal)
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

var escaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escape(s string) string { return escaper.Replace(s) }

func num(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Sample is a point-in-time value computed at scrape time (leader, circuit
// state, active deployments) by the component that owns it.
type Sample struct {
	Name, Help string
	Type       string // gauge | counter
	Labels     map[string]string
	Value      float64
}

// Write renders every registered metric, then extra, in the Prometheus text
// exposition format (version 0.0.4).
func Write(w io.Writer, extra []Sample) {
	regMu.Lock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	ms := make(map[string]metric, len(registry))
	for n, m := range registry {
		ms[n] = m
	}
	regMu.Unlock()

	byName := map[string][]Sample{}
	for _, s := range extra {
		if _, ok := byName[s.Name]; !ok {
			names = append(names, s.Name)
		}
		byName[s.Name] = append(byName[s.Name], s)
	}
	sort.Strings(names)
	for _, n := range names {
		if m, ok := ms[n]; ok {
			_, help, k := m.desc()
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", n, help, n, k)
			m.write(w)
			continue
		}
		ss := byName[n]
		typ := ss[0].Type
		if typ == "" {
			typ = string(gauge)
		}
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", n, ss[0].Help, n, typ)
		for _, s := range ss {
			ln := make([]string, 0, len(s.Labels))
			for k := range s.Labels {
				ln = append(ln, k)
			}
			sort.Strings(ln)
			lv := make([]string, len(ln))
			for i, k := range ln {
				lv[i] = s.Labels[k]
			}
			fmt.Fprintf(w, "%s%s %s\n", n, labelStr(ln, lv, "", ""), num(s.Value))
		}
	}
}

// Version is reported as vigilante_build_info{version}; set by main.
var Version = "dev"

// ContentType is the Prometheus text exposition media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

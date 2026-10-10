// Package observer watches Vigilante itself as a measuring instrument. When
// the orchestrator is starved of CPU, out of sockets or cut off from the
// network, its probes time out and every target looks broken at once. A
// rollback decided on that evidence would take down healthy releases, so
// the decision engine asks the Guard whether the observer was degraded while
// the evidence was gathered and holds instead of rolling back.
//
// Three independent signals mark the observer degraded:
//
//   - scheduling lag: a 250ms ticker that wakes up late by more than max_lag
//     (CPU starvation, GC thrash, a suspended VM);
//   - loopback: a round trip to an in-process TCP echo listener that takes
//     longer than loopback_timeout (socket or network-stack exhaustion);
//   - spread: probe timeouts on at least timeout_share of the observed
//     targets, across at least min_services services. One bad release cannot
//     trip it; a problem shared by unrelated services is not the release's.
package observer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/model"
	"vigilante/internal/telemetry"
)

const (
	tick         = 250 * time.Millisecond
	spreadWindow = 30 * time.Second
	keepSpans    = 2 * time.Hour
)

type span struct {
	from, to time.Time
	reason   string
}

type probeState struct {
	at        time.Time
	timingOut bool
}

// Guard tracks when the observer was degraded. The zero value is not usable;
// call New.
type Guard struct {
	cfg       config.ObserverGuard
	serviceOf map[string][]string // target -> services that list it
	Log       *slog.Logger
	Now       func() time.Time

	mu     sync.Mutex
	spans  []span // closed and open degraded spans, oldest first
	open   bool   // the last span is still open
	probes map[string]probeState
	users  int
	stop   context.CancelFunc
	done   chan struct{}
}

// New builds a guard for the services in cfg.
func New(cfg *config.Config, log *slog.Logger) *Guard {
	g := &Guard{cfg: cfg.Safety.ObserverGuard, serviceOf: map[string][]string{}, Log: log, Now: time.Now, probes: map[string]probeState{}}
	for _, s := range cfg.Services {
		for _, t := range s.Targets {
			g.serviceOf[t] = append(g.serviceOf[t], s.Name)
		}
	}
	if g.Log == nil {
		g.Log = slog.Default()
	}
	return g
}

// Enabled reports whether the guard may hold breaches.
func (g *Guard) Enabled() bool { return g != nil && !g.cfg.Disabled }

// Start runs the self-checks while at least one observation is using them;
// the returned function releases this use.
func (g *Guard) Start() (release func()) {
	if !g.Enabled() {
		return func() {}
	}
	g.mu.Lock()
	g.users++
	if g.users == 1 {
		ctx, cancel := context.WithCancel(context.Background())
		g.stop, g.done = cancel, make(chan struct{})
		go g.run(ctx, g.done)
	}
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.users--
			var stop context.CancelFunc
			var done chan struct{}
			if g.users == 0 {
				stop, done = g.stop, g.done
				g.stop, g.done = nil, nil
				g.probes = map[string]probeState{}
			}
			g.mu.Unlock()
			if stop != nil {
				stop()
				<-done
			}
		})
	}
}

// Observe feeds central probe samples into the spread signal.
func (g *Guard) Observe(s model.Sample) {
	if !g.Enabled() || s.Source != model.SourceCentral || !strings.HasSuffix(s.Metric, ".consecutive_timeouts") {
		return
	}
	g.mu.Lock()
	if g.users > 0 {
		g.probes[s.Target+"|"+strings.TrimSuffix(s.Metric, ".consecutive_timeouts")] = probeState{at: s.Time, timingOut: s.Value > 0}
	}
	g.mu.Unlock()
}

// Degraded reports whether the observer was degraded at any time between
// from - grace and to, and why.
func (g *Guard) Degraded(from, to time.Time) (bool, string) {
	if !g.Enabled() {
		return false, ""
	}
	from = from.Add(-g.cfg.Grace)
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := len(g.spans) - 1; i >= 0; i-- {
		sp := g.spans[i]
		end := sp.to
		if i == len(g.spans)-1 && g.open {
			end = to
		}
		if !sp.from.After(to) && !end.Before(from) {
			return true, sp.reason
		}
	}
	return false, ""
}

// Report marks the observer degraded at now. Consecutive reports extend one
// span; Clear closes it.
func (g *Guard) Report(now time.Time, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.open {
		last := &g.spans[len(g.spans)-1]
		last.to = now
		return
	}
	g.spans = append(g.spans, span{from: now, to: now, reason: reason})
	g.open = true
	cut := 0
	for cut < len(g.spans)-1 && now.Sub(g.spans[cut].to) > keepSpans {
		cut++
	}
	g.spans = g.spans[cut:]
	telemetry.ObserverDegraded.Set(1)
	telemetry.ObserverDegradations.Inc(strings.SplitN(reason, ":", 2)[0])
	g.Log.Warn("observer degraded: probe failures will hold instead of rolling back", "reason", reason)
}

// Clear closes an open degraded span at now.
func (g *Guard) Clear(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		return
	}
	g.spans[len(g.spans)-1].to = now
	g.open = false
	telemetry.ObserverDegraded.Set(0)
	g.Log.Info("observer recovered", "degraded_for", now.Sub(g.spans[len(g.spans)-1].from).Round(time.Millisecond))
}

// Spread evaluates the probe-timeout spread at now ("" = healthy).
func (g *Guard) Spread(now time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	observed, timingOut := map[string]bool{}, map[string]bool{}
	for k, p := range g.probes {
		if now.Sub(p.at) > spreadWindow {
			delete(g.probes, k)
			continue
		}
		t, _, _ := strings.Cut(k, "|")
		observed[t] = true
		if p.timingOut {
			timingOut[t] = true
		}
	}
	services := map[string]bool{}
	for t := range timingOut {
		for _, s := range g.serviceOf[t] {
			services[s] = true
		}
	}
	if len(observed) == 0 || len(services) < g.cfg.MinServices {
		return ""
	}
	share := float64(len(timingOut)) / float64(len(observed))
	if share < g.cfg.TimeoutShare {
		return ""
	}
	return fmt.Sprintf("spread: probes time out on %d of %d targets across %d services", len(timingOut), len(observed), len(services))
}

func (g *Guard) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	loop, err := newLoopback()
	if err != nil {
		g.Log.Warn("observer loopback check unavailable", "err", err)
	} else {
		defer loop.Close()
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	last := time.Now()
	n := 0
	var loopRes chan string
	for {
		select {
		case <-ctx.Done():
			g.Clear(g.Now())
			return
		case <-t.C:
		}
		wall := time.Now()
		now := g.Now()
		var reasons []string
		if lag := wall.Sub(last) - tick; lag > g.cfg.MaxLag {
			reasons = append(reasons, fmt.Sprintf("scheduling lag: woke %s late", lag.Round(time.Millisecond)))
		}
		last = wall
		n++
		if loopRes != nil {
			select {
			case r := <-loopRes:
				loopRes = nil
				if r != "" {
					reasons = append(reasons, r)
				}
			default:
				if n%4 == 0 { // still waiting after a full second: as slow as a timeout
					reasons = append(reasons, "loopback: no echo within "+g.cfg.LoopbackTimeout.String())
				}
			}
		}
		if loop != nil && loopRes == nil && n%4 == 0 {
			loopRes = make(chan string, 1)
			go func(ch chan string) { ch <- loop.check(g.cfg.LoopbackTimeout) }(loopRes)
		}
		if n%4 == 0 {
			if r := g.Spread(now); r != "" {
				reasons = append(reasons, r)
			}
		}
		switch {
		case len(reasons) > 0:
			g.Report(now, strings.Join(reasons, "; "))
		case n%4 == 0:
			g.Clear(now)
		}
	}
}

// loopback is an in-process TCP echo server.
type loopback struct {
	ln net.Listener
}

func newLoopback() (*loopback, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	l := &loopback{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				_, _ = io.CopyN(c, c, 1)
			}()
		}
	}()
	return l, nil
}

func (l *loopback) Close() { l.ln.Close() }

// check returns "" when a one-byte echo completes within timeout.
func (l *loopback) check(timeout time.Duration) string {
	start := time.Now()
	c, err := net.DialTimeout("tcp", l.ln.Addr().String(), timeout)
	if err != nil {
		return "loopback: " + err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(start.Add(timeout))
	buf := []byte{1}
	if _, err := c.Write(buf); err != nil {
		return "loopback: " + err.Error()
	}
	if _, err := io.ReadFull(c, buf); err != nil {
		return fmt.Sprintf("loopback: no echo within %s", timeout)
	}
	return ""
}

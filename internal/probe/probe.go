// Package probe implements the plugin-based multi-target collectors. Every
// probe type registers a Factory; the Collector runs one goroutine per
// (target x probe), restarts crashed probes with backoff and funnels samples
// into a single sink.
package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/model"
	"vigilante/internal/tmpl"
	"vigilante/internal/transport"
)

// Emit records one metric value; the collector prefixes the probe id and
// stamps target, time and source.
type Emit func(metric string, value float64)

// Probe runs until ctx is cancelled (polling or streaming).
type Probe interface {
	Run(ctx context.Context, emit Emit) error
}

// Checker is implemented by probes that can answer a single synchronous
// health question; the rollback plan's probe.verify step uses it.
type Checker interface {
	Check(ctx context.Context) error
}

// Env is everything a probe instance may need about its target.
type Env struct {
	Target config.Target
	Data   tmpl.Data
	Runner transport.Runner // nil when the target has no command connection
	Log    *slog.Logger
}

type Factory func(spec config.Probe, env Env) (Probe, error)

var (
	regMu    sync.RWMutex
	registry = map[string]Factory{}
)

// Register adds a probe type. Called from init() of each probe file; new
// in-house probe types plug in the same way.
func Register(typ string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[typ] = f
}

func Types() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	var out []string
	for t := range registry {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func New(spec config.Probe, env Env) (Probe, error) {
	regMu.RLock()
	f, ok := registry[spec.Type]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown probe type %q", spec.Type)
	}
	return f(spec, env)
}

// Job is one (target, probe spec) pair.
type Job struct {
	Target config.Target
	Spec   config.Probe
}

// RunnerFor resolves the command transport for a target name.
type RunnerFor func(target string) (transport.Runner, error)

// Collector executes jobs concurrently.
type Collector struct {
	Runners RunnerFor
	Sink    func(model.Sample)
	Source  string
	Log     *slog.Logger
	// Data decorates the per-target template data (deployment fields).
	Data func(t config.Target) tmpl.Data
}

// Build instantiates a probe for one job.
func (c *Collector) Build(j Job) (Probe, error) {
	env := Env{Target: j.Target, Log: c.Log.With("target", j.Target.Name, "probe", j.Spec.ID)}
	if c.Data != nil {
		env.Data = c.Data(j.Target)
	} else {
		env.Data = tmpl.ForTarget(j.Target)
	}
	if c.Runners != nil {
		r, err := c.Runners(j.Target.Name)
		if err != nil && !errors.Is(err, transport.ErrNoRunner) {
			return nil, err
		}
		env.Runner = r
	}
	return New(j.Spec, env)
}

// Run starts every job and blocks until ctx is done.
func (c *Collector) Run(ctx context.Context, jobs []Job) {
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			c.supervise(ctx, j)
		}(j)
	}
	wg.Wait()
}

func (c *Collector) supervise(ctx context.Context, j Job) {
	log := c.Log.With("target", j.Target.Name, "probe", j.Spec.ID)
	source := c.Source
	if source == "" {
		source = model.SourceCentral
	}
	emit := func(metric string, v float64) {
		if ctx.Err() != nil {
			return // a check cut short by shutdown is not a failure of the target
		}
		c.Sink(model.Sample{Target: j.Target.Name, Metric: j.Spec.ID + "." + metric, Value: v, Time: time.Now(), Source: source})
	}
	backoff := time.Second
	for ctx.Err() == nil {
		p, err := c.Build(j)
		if err == nil {
			started := time.Now()
			err = p.Run(ctx, emit)
			if time.Since(started) > time.Minute {
				backoff = time.Second
			}
		}
		if ctx.Err() != nil {
			return
		}
		// A dead probe is itself a signal: rules can use "<id>.probe_error".
		emit("probe_error", 1)
		log.Warn("probe stopped, restarting", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// poll runs fn immediately and then every interval until ctx is done.
func poll(ctx context.Context, interval time.Duration, fn func(ctx context.Context)) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// failureCounter tracks consecutive failures/timeouts for polling probes.
type failureCounter struct {
	failures, timeouts int
}

func (f *failureCounter) observe(emit Emit, ok, timeout bool) {
	if ok {
		f.failures, f.timeouts = 0, 0
	} else {
		f.failures++
		if timeout {
			f.timeouts++
		} else {
			f.timeouts = 0
		}
	}
	up := 0.0
	if ok {
		up = 1
	}
	emit("up", up)
	emit("consecutive_failures", float64(f.failures))
	emit("consecutive_timeouts", float64(f.timeouts))
	if timeout {
		emit("timeout", 1)
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

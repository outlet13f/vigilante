// Package agent is the optional on-host mode of the same binary. It runs the
// service probes locally (high-rate log tailing, hosts without SSH from the
// orchestrator), pushes samples to the orchestrator, and acts as a dead-man's
// switch: if the orchestrator disappears while a deployment is being observed
// on this host, the agent can judge and roll back its own target.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/metrics"
	"vigilante/internal/model"
	"vigilante/internal/orchestrator"
	"vigilante/internal/probe"
	"vigilante/internal/rules"
	"vigilante/internal/tlsconf"
	"vigilante/internal/transport"
)

type Agent struct {
	Cfg    *config.Config
	Target string
	Server string // orchestrator base URL
	Token  string
	Log    *slog.Logger

	store  *metrics.Store
	client *http.Client

	mu      sync.Mutex
	buf     []model.Sample
	lastOK  time.Time
	active  *model.Deployment
	failed  bool // failsafe already acted for the active deployment
	dropped int
}

const maxBuffer = 50_000

func (a *Agent) source() string { return "agent:" + a.Target }

func (a *Agent) services() []*config.Service {
	var out []*config.Service
	for i := range a.Cfg.Services {
		for _, t := range a.Cfg.Services[i].Targets {
			if t == a.Target {
				out = append(out, &a.Cfg.Services[i])
			}
		}
	}
	return out
}

func (a *Agent) Run(ctx context.Context) error {
	t, ok := a.Cfg.Target(a.Target)
	if !ok {
		return fmt.Errorf("target %q not in config", a.Target)
	}
	local := *t
	local.Connection.Type = "local"
	a.store = metrics.NewStore(30 * time.Minute)
	tc, err := tlsconf.Client(a.Cfg.Agent.TLS)
	if err != nil {
		return err
	}
	a.client = &http.Client{Timeout: 10 * time.Second}
	if tc != nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = tc
		a.client.Transport = tr
	}
	a.lastOK = time.Now()

	var jobs []probe.Job
	for _, svc := range a.services() {
		for _, p := range svc.Probes {
			jobs = append(jobs, probe.Job{Target: local, Spec: p})
		}
	}
	if len(jobs) == 0 {
		return fmt.Errorf("no service uses target %q", a.Target)
	}
	col := &probe.Collector{
		Runners: func(string) (transport.Runner, error) { return &transport.Local{Sudo: t.Connection.Sudo}, nil },
		Sink: func(s model.Sample) {
			a.store.Add(s)
			a.mu.Lock()
			if len(a.buf) >= maxBuffer {
				a.buf = a.buf[1:]
				a.dropped++
			}
			a.buf = append(a.buf, s)
			a.mu.Unlock()
		},
		Source: a.source(),
		Log:    a.Log,
	}
	a.Log.Info("agent started", "target", a.Target, "server", a.Server, "probes", len(jobs), "failsafe", a.Cfg.Agent.Failsafe)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); col.Run(ctx, jobs) }()
	go func() { defer wg.Done(); a.every(ctx, a.Cfg.Agent.PushInterval, a.push) }()
	go func() { defer wg.Done(); a.every(ctx, a.Cfg.Agent.HeartbeatInterval, a.heartbeat) }()
	a.every(ctx, a.Cfg.Agent.HeartbeatInterval, a.failsafe)
	wg.Wait()
	return nil
}

func (a *Agent) every(ctx context.Context, d time.Duration, fn func(context.Context)) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

func (a *Agent) request(ctx context.Context, method, path string, body any, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(a.Server, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (a *Agent) push(ctx context.Context) {
	a.mu.Lock()
	batch := a.buf
	a.buf = nil
	a.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	if err := a.request(ctx, http.MethodPost, "/v1/samples", batch, nil); err != nil {
		a.mu.Lock()
		// Re-queue (bounded) so a short outage loses nothing.
		a.buf = append(batch, a.buf...)
		if over := len(a.buf) - maxBuffer; over > 0 {
			a.buf = a.buf[over:]
			a.dropped += over
		}
		a.mu.Unlock()
		a.Log.Debug("push failed", "err", err, "buffered", len(batch))
	}
}

func (a *Agent) heartbeat(ctx context.Context) {
	var hb struct {
		Active *model.Deployment `json:"active"`
	}
	if err := a.request(ctx, http.MethodGet, "/v1/agents/"+a.Target+"/heartbeat", nil, &hb); err != nil {
		a.Log.Debug("heartbeat failed", "err", err)
		return
	}
	a.mu.Lock()
	a.lastOK = time.Now()
	if hb.Active == nil || a.active == nil || hb.Active.ID != a.active.ID {
		a.failed = false
	}
	a.active = hb.Active
	a.mu.Unlock()
}

// failsafe evaluates the service rules locally once the orchestrator has been
// unreachable for failsafe_after while a deployment is active on this target.
func (a *Agent) failsafe(ctx context.Context) {
	a.mu.Lock()
	active, since, acted := a.active, time.Since(a.lastOK), a.failed
	a.mu.Unlock()
	if active == nil || acted || since < a.Cfg.Agent.FailsafeAfter {
		return
	}
	svc, ok := a.Cfg.Service(active.Service)
	if !ok {
		return
	}
	ev := rules.NewEvaluator(a.store, nil)
	var fired []string
	for _, r := range svc.Rules {
		if r.Action != "rollback" {
			continue
		}
		if res := ev.Eval(r, rules.Scope{Target: a.Target, ServiceTargets: []string{a.Target}}, time.Now()); res.State == rules.True {
			fired = append(fired, r.Name+": "+res.Detail)
		}
	}
	if len(fired) == 0 {
		return
	}
	a.mu.Lock()
	a.failed = true
	a.mu.Unlock()
	reason := fmt.Sprintf("orchestrator unreachable for %s and local rules fired: %s", since.Round(time.Second), strings.Join(fired, "; "))
	if a.Cfg.Agent.Failsafe != "rollback" || svc.Rollback.RollbackMode() == "approve" { // approve mode: a person decides, never the agent alone
		a.Log.Error("FAILSAFE HOLD (no action taken; agent.failsafe=hold)", "deployment", active.ID, "reason", reason)
		return
	}
	a.Log.Error("FAILSAFE ROLLBACK of local target", "deployment", active.ID, "reason", reason)
	if err := a.localRollback(ctx, active, reason); err != nil {
		a.Log.Error("failsafe rollback failed", "err", err)
	}
}

// localRollback runs the service's rollback plan for this target only, with
// traffic steps removed (the LB may be unreachable from here too) and a
// separate local journal.
func (a *Agent) localRollback(ctx context.Context, d *model.Deployment, reason string) error {
	cfg := *a.Cfg
	cfg.Server.JournalPath = filepath.Join(filepath.Dir(a.Cfg.Server.JournalPath), "vigilante-agent-"+a.Target+".jsonl")
	cfg.Server.State = config.State{Backend: "file"} // the shared store may be unreachable too
	cfg.Server.HA = config.HA{}
	cfg.Services = append([]config.Service(nil), a.Cfg.Services...)
	cfg.Targets = append([]config.Target(nil), a.Cfg.Targets...)
	for i := range cfg.Targets {
		if cfg.Targets[i].Name == a.Target {
			cfg.Targets[i].Connection.Type = "local"
		}
	}
	for i := range cfg.Services {
		if cfg.Services[i].Name != d.Service {
			continue
		}
		rb := &cfg.Services[i].Rollback
		rb.Traffic = ""
		var plan []config.Step
		for _, st := range rb.Plan {
			if !strings.HasPrefix(st.Action, "traffic.") {
				plan = append(plan, st)
			}
		}
		rb.Plan = plan
	}
	e, err := orchestrator.New(&cfg, orchestrator.Options{Log: a.Log})
	if err != nil {
		return err
	}
	defer e.Close()
	dep, err := e.Create(d.ID+"-failsafe-"+a.Target, d.Service, d.Version, d.PreviousVersion)
	if err != nil {
		return err
	}
	dep.Targets = []string{a.Target}
	dep.Checkpoints = d.Checkpoints
	return e.Rollback(ctx, dep, orchestrator.RollbackOptions{Reason: "agent failsafe: " + reason, Targets: []string{a.Target}})
}

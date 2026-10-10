// Package executor contains the rollback strategies (A: symlink/systemd,
// B: container image switch, D: hypervisor snapshot restore, plus generic
// exec/webhook) and the traffic controllers (strategy C: Nginx, HAProxy,
// Envoy, F5 BIG-IP, AWS ALB). Everything is behind two small interfaces so a
// rollback plan can combine any strategy with any traffic layer.
package executor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/secrets"
	"vigilante/internal/tmpl"
	"vigilante/internal/transport"
)

// RunContext is what a strategy needs to act on one target.
type RunContext struct {
	Target     config.Target
	Data       tmpl.Data        // target + deployment fields (Version, PreviousVersion, ...)
	Runner     transport.Runner // the target's own runner; nil for API-only targets
	Runners    func(target string) (transport.Runner, error)
	Creds      map[string]config.Credential
	Checkpoint map[string]string // values captured by Prepare before the deployment
	DryRun     bool
	Log        *slog.Logger
}

// Executor restores one target to its previous known-good state.
// Rollback must be idempotent: after a crash the orchestrator replays it.
type Executor interface {
	Rollback(ctx context.Context, rc *RunContext) error
	// Verify confirms the target really is on the previous version (link
	// target, image tag, VM power state) — independent of health probes.
	Verify(ctx context.Context, rc *RunContext) error
}

// SudoRule is a command a strategy runs with sudo when the host's
// connection.sudo_scope is "changes". Args are as sudo sees them (after
// the shell removed quotes); "*" stands for values only known at rollback
// time (a release directory, a snapshot name).
type SudoRule struct {
	Host    string `json:"host"` // target the command runs on
	Command string `json:"command"`
	Args    string `json:"args"`
	Why     string `json:"why"`
}

// SudoRules is implemented by executors that change things with commands.
type SudoRules interface {
	SudoRules(rc *RunContext) ([]SudoRule, error)
}

// TrafficSudoRules is the same for traffic controllers.
type TrafficSudoRules interface {
	SudoRules() []SudoRule
}

// Preparer is implemented by strategies that must capture state before the
// deployment (VM snapshots, current symlink target). `vigilante prepare`
// calls it; the result is stored as the deployment checkpoint.
type Preparer interface {
	Prepare(ctx context.Context, rc *RunContext) (map[string]string, error)
}

type Factory func(name string, spec config.Executor) (Executor, error)

// Member is a pool member derived from a target.
type Member struct {
	Target string
	Data   tmpl.Data
}

// PoolMember is the controller's view of one member (including members that
// vigilante does not manage, which matter for blast-radius math).
type PoolMember struct {
	ID      string
	Enabled bool
}

// TrafficController takes instances out of / back into rotation.
type TrafficController interface {
	MemberID(m Member) (string, error)
	Pool(ctx context.Context) ([]PoolMember, error)
	Drain(ctx context.Context, ms []Member) error
	Enable(ctx context.Context, ms []Member) error
}

type TrafficEnv struct {
	Runners func(target string) (transport.Runner, error)
	Creds   map[string]config.Credential
	DryRun  bool
	Log     *slog.Logger
	HTTP    *http.Client // optional override (tests)
}

type TrafficFactory func(name string, spec config.Traffic, env TrafficEnv) (TrafficController, error)

var (
	regMu   sync.RWMutex
	execReg = map[string]Factory{}
	trafReg = map[string]TrafficFactory{}
)

func Register(typ string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	execReg[typ] = f
}

func RegisterTraffic(typ string, f TrafficFactory) {
	regMu.Lock()
	defer regMu.Unlock()
	trafReg[typ] = f
}

func New(name string, spec config.Executor) (Executor, error) {
	regMu.RLock()
	f, ok := execReg[spec.Type]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown executor type %q", spec.Type)
	}
	return f(name, spec)
}

func NewTraffic(name string, spec config.Traffic, env TrafficEnv) (TrafficController, error) {
	regMu.RLock()
	f, ok := trafReg[spec.Type]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown traffic type %q", spec.Type)
	}
	return f(name, spec, env)
}

func Types() (execs, traffic []string) {
	regMu.RLock()
	defer regMu.RUnlock()
	for k := range execReg {
		execs = append(execs, k)
	}
	for k := range trafReg {
		traffic = append(traffic, k)
	}
	sort.Strings(execs)
	sort.Strings(traffic)
	return
}

// runner returns rc.Runner, wrapped for dry-run.
func (rc *RunContext) runner() (transport.Runner, error) {
	if rc.Runner == nil {
		return nil, transport.ErrNoRunner
	}
	return rc.wrap(rc.Runner), nil
}

func (rc *RunContext) runnerFor(target string) (transport.Runner, error) {
	if rc.Runners == nil {
		return nil, transport.ErrNoRunner
	}
	r, err := rc.Runners(target)
	if err != nil {
		return nil, err
	}
	return rc.wrap(r), nil
}

func (rc *RunContext) wrap(r transport.Runner) transport.Runner {
	if rc.DryRun {
		return &transport.DryRun{Inner: r, Log: rc.Log}
	}
	return r
}

func (rc *RunContext) render(s string) (string, error) { return tmpl.Render(s, rc.Data) }

// basicAuth resolves a credential's username/password from the environment.
func basicAuth(creds map[string]config.Credential, name string) (user, pass string, err error) {
	if name == "" {
		return "", "", nil
	}
	c, ok := creds[name]
	if !ok {
		return "", "", fmt.Errorf("unknown credential %q", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	user = c.User
	if c.UsernameRef != "" || c.UsernameEnv != "" {
		if user, err = secrets.Value(ctx, c.UsernameRef, c.UsernameEnv); err != nil {
			return "", "", err
		}
	}
	if pass, err = secrets.Value(ctx, c.PasswordRef, c.PasswordEnv); err != nil {
		return "", "", err
	}
	return user, pass, nil
}

func bearer(creds map[string]config.Credential, name string) string {
	c, ok := creds[name]
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	v, _ := secrets.Value(ctx, c.TokenRef, c.TokenEnv)
	return v
}

// Finding is one read-only pre-flight check result (`vigilante doctor`).
// Status: ok | warn | fail | skip.
type Finding struct {
	Name   string
	Status string
	Detail string
}

// Diagnoser is implemented by strategies that can verify, without changing
// anything, that a rollback on rc's target would have what it needs.
type Diagnoser interface {
	Diagnose(ctx context.Context, rc *RunContext) []Finding
}

// TrafficDiagnoser is the same for traffic controllers, beyond the generic
// pool-membership check every controller gets.
type TrafficDiagnoser interface {
	Diagnose(ctx context.Context, members []Member) []Finding
}

func finding(name string, err error, okDetail string) Finding {
	if err != nil {
		return Finding{Name: name, Status: "fail", Detail: err.Error()}
	}
	return Finding{Name: name, Status: "ok", Detail: okDetail}
}

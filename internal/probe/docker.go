package probe

import (
	"context"
	"net"

	"vigilante/internal/config"
	"vigilante/internal/dockerapi"
	"vigilante/internal/tmpl"
)

func init() { Register("docker", newDocker) }

// Docker probe watches one container through the engine socket (tunnelled
// over SSH for remote hosts).
//
// Polled metrics: running, restart_count, restarts (delta since probe start),
// oom_killed, health_ok (1 healthy / 0 unhealthy, only when a HEALTHCHECK exists).
// Event metrics (value 1 per event): oom_events, die_events, restart_events.
type dockerProbe struct {
	spec      config.Probe
	env       Env
	client    *dockerapi.Client
	container string
}

func newDocker(spec config.Probe, env Env) (Probe, error) {
	name, err := tmpl.Render(spec.Docker.Container, env.Data)
	if err != nil {
		return nil, err
	}
	socket := spec.Docker.Socket
	if socket == "" && spec.Docker.Host == "" {
		socket = "/var/run/docker.sock"
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if env.Runner == nil {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
		return env.Runner.Dial(ctx, network, addr)
	}
	return &dockerProbe{spec: spec, env: env, container: name, client: dockerapi.New(dial, socket, spec.Docker.Host)}, nil
}

func (p *dockerProbe) Run(ctx context.Context, emit Emit) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		// The event stream complements polling (it catches OOM kills between
		// polls); if it drops, polling keeps working.
		err := p.client.Events(ctx, p.container, func(ev dockerapi.Event) {
			switch ev.Action {
			case "oom":
				emit("oom_events", 1)
			case "die":
				emit("die_events", 1)
			case "restart":
				emit("restart_events", 1)
			}
		})
		if ctx.Err() == nil {
			p.env.Log.Debug("docker event stream ended", "err", err)
		}
	}()
	base := -1
	return poll(ctx, p.spec.Interval, func(ctx context.Context) {
		cctx, cancel := context.WithTimeout(ctx, p.spec.Timeout)
		defer cancel()
		ct, err := p.client.Inspect(cctx, p.container)
		if err != nil {
			p.env.Log.Debug("docker inspect failed", "err", err)
			emit("running", 0)
			return
		}
		if base < 0 {
			base = ct.RestartCount
		}
		emit("running", b2f(ct.State.Running))
		emit("restart_count", float64(ct.RestartCount))
		emit("restarts", float64(ct.RestartCount-base))
		emit("oom_killed", b2f(ct.State.OOMKilled))
		if ct.State.Health != nil {
			emit("health_ok", b2f(ct.State.Health.Status == "healthy"))
		}
	})
}

func (p *dockerProbe) Check(ctx context.Context) error {
	ct, err := p.client.Inspect(ctx, p.container)
	if err != nil {
		return err
	}
	if !ct.State.Running {
		return errContainerNotRunning(p.container, ct.State.Status)
	}
	if ct.State.Health != nil && ct.State.Health.Status == "unhealthy" {
		return errContainerNotRunning(p.container, "unhealthy")
	}
	return nil
}

type containerStateError struct{ name, state string }

func (e containerStateError) Error() string           { return "container " + e.name + " is " + e.state }
func errContainerNotRunning(name, state string) error { return containerStateError{name, state} }

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

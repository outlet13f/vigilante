package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/tmpl"
	"vigilante/internal/transport"
)

func init() {
	RegisterTraffic("nginx", newNginx)
	RegisterTraffic("haproxy", newHAProxy)
	RegisterTraffic("envoy", newEnvoy)
}

// ---------------------------------------------------------------------------
// Nginx: mark upstream members `down` in a dedicated upstream include file,
// validate with `nginx -t`, reload. A failed config test restores the backup,
// so a broken edit can never take the LB down.

type nginxTraffic struct {
	spec *config.NginxTraffic
	env  TrafficEnv
}

func newNginx(_ string, spec config.Traffic, env TrafficEnv) (TrafficController, error) {
	return &nginxTraffic{spec: spec.Nginx, env: env}, nil
}

func (n *nginxTraffic) MemberID(m Member) (string, error) {
	return tmpl.Render(n.spec.MemberFormat, m.Data)
}

var upstreamServerRe = regexp.MustCompile(`(?m)^(\s*server\s+)(\S+)([^;\n]*);`)
var downFlagRe = regexp.MustCompile(`\s+down\b`)

// SetNginxDown rewrites `server` lines of the given addresses. Exported for tests.
func SetNginxDown(conf string, ids map[string]bool, down bool) (string, []string) {
	found := map[string]bool{}
	out := upstreamServerRe.ReplaceAllStringFunc(conf, func(line string) string {
		m := upstreamServerRe.FindStringSubmatch(line)
		if !ids[m[2]] {
			return line
		}
		found[m[2]] = true
		params := downFlagRe.ReplaceAllString(m[3], "")
		if down {
			params += " down"
		}
		return m[1] + m[2] + params + ";"
	})
	var missing []string
	for id := range ids {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	return out, missing
}

func (n *nginxTraffic) runner(host string) (transport.Runner, error) {
	r, err := n.env.Runners(host)
	if err != nil {
		return nil, err
	}
	if n.env.DryRun {
		return &transport.DryRun{Inner: r, Log: n.env.Log}, nil
	}
	return r, nil
}

func (n *nginxTraffic) Pool(ctx context.Context) ([]PoolMember, error) {
	r, err := n.env.Runners(n.spec.Hosts[0])
	if err != nil {
		return nil, err
	}
	conf, err := r.Run(ctx, "cat "+transport.ShellQuote(n.spec.UpstreamFile), nil)
	if err != nil {
		return nil, err
	}
	var out []PoolMember
	for _, m := range upstreamServerRe.FindAllStringSubmatch(conf, -1) {
		out = append(out, PoolMember{ID: m[2], Enabled: !downFlagRe.MatchString(m[3]) && !strings.Contains(m[3], "backup")})
	}
	return out, nil
}

func (n *nginxTraffic) set(ctx context.Context, ms []Member, down bool) error {
	ids := map[string]bool{}
	for _, m := range ms {
		id, err := n.MemberID(m)
		if err != nil {
			return err
		}
		ids[id] = true
	}
	f := transport.ShellQuote(n.spec.UpstreamFile)
	var errs []error
	for _, host := range n.spec.Hosts {
		read, err := n.env.Runners(host)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		conf, err := read.Run(ctx, "cat "+f, nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", host, err))
			continue
		}
		next, missing := SetNginxDown(conf, ids, down)
		if len(missing) > 0 {
			errs = append(errs, fmt.Errorf("%s: members %v not found in %s", host, missing, n.spec.UpstreamFile))
			continue
		}
		if next == conf {
			continue // already in the desired state (idempotent replay)
		}
		w, err := n.runner(host)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		bak := transport.ShellQuote(n.spec.UpstreamFile + ".vigilante-bak")
		tmp := transport.ShellQuote(n.spec.UpstreamFile + ".vigilante-new")
		sudo := transport.Sudo(w)
		test, reload := n.spec.TestCmd, n.spec.ReloadCmd
		if test == defaultNginxTest {
			test = sudo + test
		}
		if reload == defaultNginxReload {
			reload = sudo + reload
		}
		script := fmt.Sprintf(`set -e
%[6]scp -p %[1]s %[2]s
%[7]s
%[6]smv %[3]s %[1]s
if ! %[4]s 2>&1; then %[6]scp -p %[2]s %[1]s; echo "config test failed; restored previous upstream file" >&2; exit 1; fi
%[5]s`, f, bak, tmp, test, reload, sudo, writeFile(sudo, tmp))
		if _, err := w.Run(ctx, script, strings.NewReader(next)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", host, err))
		}
	}
	return errors.Join(errs...)
}

// The built-in commands; customised test_cmd / reload_cmd are run as written.
const (
	defaultNginxTest   = "nginx -t"
	defaultNginxReload = "nginx -s reload"
)

// writeFile writes stdin to a file, through sudo tee when the file needs it.
func writeFile(sudo, quotedPath string) string {
	if sudo == "" {
		return "cat > " + quotedPath
	}
	return sudo + "tee " + quotedPath + " > /dev/null"
}

// SudoRules: replace the upstream file, test and reload nginx on each LB host.
func (n *nginxTraffic) SudoRules() []SudoRule {
	f := n.spec.UpstreamFile
	var out []SudoRule
	for _, h := range n.spec.Hosts {
		out = append(out,
			SudoRule{h, "cp", "-p " + f + " " + f + ".vigilante-bak", "keep the current upstream file"},
			SudoRule{h, "tee", f + ".vigilante-new", "write the new upstream file"},
			SudoRule{h, "mv", f + ".vigilante-new " + f, "replace the upstream file"},
			SudoRule{h, "cp", "-p " + f + ".vigilante-bak " + f, "restore it when the config test fails"})
		if n.spec.TestCmd == defaultNginxTest {
			out = append(out, SudoRule{h, "nginx", "-t", "test the configuration"})
		}
		if n.spec.ReloadCmd == defaultNginxReload {
			out = append(out, SudoRule{h, "nginx", "-s reload", "apply it"})
		}
	}
	return out
}

func (n *nginxTraffic) Drain(ctx context.Context, ms []Member) error  { return n.set(ctx, ms, true) }
func (n *nginxTraffic) Enable(ctx context.Context, ms []Member) error { return n.set(ctx, ms, false) }

// ---------------------------------------------------------------------------
// HAProxy: Runtime API. `drain` stops new sessions while existing ones finish;
// optionally followed by `maint`. The admin socket on the LB host is reached
// through an SSH stream-local tunnel, or directly when exposed on TCP.

type haproxyTraffic struct {
	spec *config.HAProxyTraffic
	env  TrafficEnv
	wait time.Duration
}

func newHAProxy(_ string, spec config.Traffic, env TrafficEnv) (TrafficController, error) {
	return &haproxyTraffic{spec: spec.HAProxy, env: env, wait: spec.DrainWait}, nil
}

func (h *haproxyTraffic) MemberID(m Member) (string, error) {
	return tmpl.Render(h.spec.ServerName, m.Data)
}

func (h *haproxyTraffic) dialers() []func(ctx context.Context) (net.Conn, error) {
	if h.spec.Address != "" {
		return []func(ctx context.Context) (net.Conn, error){func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", h.spec.Address)
		}}
	}
	var out []func(ctx context.Context) (net.Conn, error)
	for _, host := range h.spec.Hosts {
		host := host
		out = append(out, func(ctx context.Context) (net.Conn, error) {
			r, err := h.env.Runners(host)
			if err != nil {
				return nil, err
			}
			return r.Dial(ctx, "unix", h.spec.Socket)
		})
	}
	return out
}

// HAProxyCommand sends one runtime API command and returns the response.
func HAProxyCommand(ctx context.Context, dial func(ctx context.Context) (net.Conn, error), cmd string) (string, error) {
	c, err := dial(ctx)
	if err != nil {
		return "", err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	} else {
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	}
	if _, err := io.WriteString(c, cmd+"\n"); err != nil {
		return "", err
	}
	b, err := io.ReadAll(c)
	return strings.TrimSpace(string(b)), err
}

func (h *haproxyTraffic) Pool(ctx context.Context) ([]PoolMember, error) {
	out, err := HAProxyCommand(ctx, h.dialers()[0], "show servers state "+h.spec.Backend)
	if err != nil {
		return nil, err
	}
	var members []PoolMember
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 7 || strings.HasPrefix(f[0], "#") {
			continue
		}
		admin, _ := strconv.Atoi(f[6]) // srv_admin_state: 0 = ready
		members = append(members, PoolMember{ID: f[3], Enabled: admin == 0})
	}
	return members, nil
}

func (h *haproxyTraffic) command(ctx context.Context, ms []Member, state string) error {
	var errs []error
	for _, m := range ms {
		srv, err := h.MemberID(m)
		if err != nil {
			return err
		}
		cmd := fmt.Sprintf("set server %s/%s state %s", h.spec.Backend, srv, state)
		if h.env.DryRun {
			h.env.Log.Info("DRY-RUN: haproxy runtime api", "cmd", cmd)
			continue
		}
		for _, dial := range h.dialers() {
			resp, err := HAProxyCommand(ctx, dial, cmd)
			if err == nil && resp != "" {
				err = fmt.Errorf("haproxy: %s", resp)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", cmd, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (h *haproxyTraffic) Drain(ctx context.Context, ms []Member) error {
	if err := h.command(ctx, ms, "drain"); err != nil {
		return err
	}
	if !h.spec.DrainToMaint {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(h.wait):
	}
	return h.command(ctx, ms, "maint")
}

func (h *haproxyTraffic) Enable(ctx context.Context, ms []Member) error {
	return h.command(ctx, ms, "ready")
}

// ---------------------------------------------------------------------------
// Envoy: file-based EDS (path_config_source). Endpoints are flipped to
// health_status DRAINING and the file is replaced atomically with mv, which
// Envoy's file watcher picks up without a restart.

type envoyTraffic struct {
	spec *config.EnvoyTraffic
	env  TrafficEnv
}

func newEnvoy(_ string, spec config.Traffic, env TrafficEnv) (TrafficController, error) {
	return &envoyTraffic{spec: spec.Envoy, env: env}, nil
}

func (e *envoyTraffic) MemberID(m Member) (string, error) {
	return tmpl.Render(e.spec.MemberFormat, m.Data)
}

// walkEnvoyEndpoints calls fn(id, lbEndpoint) for every lb_endpoint in an EDS document.
func walkEnvoyEndpoints(doc map[string]any, fn func(id string, ep map[string]any)) {
	res, _ := doc["resources"].([]any)
	for _, r := range res {
		cla, _ := r.(map[string]any)
		eps, _ := cla["endpoints"].([]any)
		for _, le := range eps {
			lem, _ := le.(map[string]any)
			lbs, _ := lem["lb_endpoints"].([]any)
			for _, lb := range lbs {
				lbm, _ := lb.(map[string]any)
				ep, _ := lbm["endpoint"].(map[string]any)
				addr, _ := ep["address"].(map[string]any)
				sa, _ := addr["socket_address"].(map[string]any)
				host, _ := sa["address"].(string)
				port := fmt.Sprint(sa["port_value"])
				fn(host+":"+port, lbm)
			}
		}
	}
}

func (e *envoyTraffic) Pool(ctx context.Context) ([]PoolMember, error) {
	r, err := e.env.Runners(e.spec.Hosts[0])
	if err != nil {
		return nil, err
	}
	raw, err := r.Run(ctx, "cat "+transport.ShellQuote(e.spec.EDSFile), nil)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	var out []PoolMember
	walkEnvoyEndpoints(doc, func(id string, ep map[string]any) {
		hs, _ := ep["health_status"].(string)
		out = append(out, PoolMember{ID: id, Enabled: hs == "" || hs == "HEALTHY" || hs == "UNKNOWN"})
	})
	return out, nil
}

func (e *envoyTraffic) set(ctx context.Context, ms []Member, draining bool) error {
	ids := map[string]bool{}
	for _, m := range ms {
		id, err := e.MemberID(m)
		if err != nil {
			return err
		}
		ids[id] = true
	}
	f := transport.ShellQuote(e.spec.EDSFile)
	var errs []error
	for _, host := range e.spec.Hosts {
		r, err := e.env.Runners(host)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		raw, err := r.Run(ctx, "cat "+f, nil)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			errs = append(errs, fmt.Errorf("%s: parse EDS: %w", host, err))
			continue
		}
		walkEnvoyEndpoints(doc, func(id string, ep map[string]any) {
			if !ids[id] {
				return
			}
			if draining {
				ep["health_status"] = "DRAINING"
			} else {
				delete(ep, "health_status")
			}
		})
		doc["version_info"] = fmt.Sprintf("vigilante-%d", time.Now().UnixNano())
		next, _ := json.MarshalIndent(doc, "", "  ")
		w := r
		if e.env.DryRun {
			w = &transport.DryRun{Inner: r, Log: e.env.Log}
		}
		tmp := transport.ShellQuote(e.spec.EDSFile + ".vigilante-new")
		sudo := transport.Sudo(w)
		script := fmt.Sprintf("set -e\n%[3]s\n%[4]smv %[2]s %[1]s", f, tmp, writeFile(sudo, tmp), sudo)
		if _, err := w.Run(ctx, script, strings.NewReader(string(next))); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", host, err))
		}
	}
	return errors.Join(errs...)
}

// SudoRules: replace the EDS file on each Envoy host.
func (e *envoyTraffic) SudoRules() []SudoRule {
	f := e.spec.EDSFile
	var out []SudoRule
	for _, h := range e.spec.Hosts {
		out = append(out,
			SudoRule{h, "tee", f + ".vigilante-new", "write the new endpoints file"},
			SudoRule{h, "mv", f + ".vigilante-new " + f, "replace the endpoints file (Envoy reloads it)"})
	}
	return out
}

func (e *envoyTraffic) Drain(ctx context.Context, ms []Member) error  { return e.set(ctx, ms, true) }
func (e *envoyTraffic) Enable(ctx context.Context, ms []Member) error { return e.set(ctx, ms, false) }

// Package sudoers turns the commands Vigilante runs with sudo (targets with
// connection.sudo_scope: changes) into sudoers rules, so a host allows
// exactly those commands instead of unrestricted sudo. `vigilante sudoers`
// prints them and `vigilante doctor` checks them with `sudo -n -l`.
package sudoers

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"vigilante/internal/config"
	"vigilante/internal/executor"
	"vigilante/internal/tmpl"
	"vigilante/internal/transport"
)

// Rule is a sudo rule with the service and strategy that need it.
type Rule struct {
	executor.SudoRule
	Service string `json:"service"`
	Source  string `json:"source"` // executor or traffic controller name
}

// Rules collects every rule the configuration needs, per host, without
// contacting anything. Notes name strategies whose commands are written by
// the operator (exec, a custom restart_cmd) and need rules added by hand.
func Rules(cfg *config.Config) (rules []Rule, notes []string) {
	seen := map[string]bool{}
	add := func(svc, src string, rs []executor.SudoRule) {
		for _, r := range rs {
			k := r.Host + "\x00" + r.Command + "\x00" + r.Args
			if !seen[k] {
				seen[k] = true
				rules = append(rules, Rule{SudoRule: r, Service: svc, Source: src})
			}
		}
	}
	noted := map[string]bool{}
	note := func(s string) {
		if !noted[s] {
			noted[s] = true
			notes = append(notes, s)
		}
	}
	for i := range cfg.Services {
		svc := &cfg.Services[i]
		names := []string{svc.Rollback.Executor}
		for _, esc := range svc.Rollback.Escalation {
			names = append(names, esc.Executor)
		}
		for _, name := range names {
			spec, ok := cfg.Executors[name]
			if !ok {
				continue
			}
			switch spec.Type {
			case "exec":
				note(fmt.Sprintf("executor %s (exec) runs its commands as written: add sudoers rules for them yourself", name))
				continue
			case "symlink":
				if spec.Symlink != nil && spec.Symlink.RestartCmd != "" {
					note(fmt.Sprintf("executor %s: the custom restart_cmd runs as written: add a rule for it (with its own sudo -n)", name))
				}
			}
			ex, err := executor.New(name, spec)
			if err != nil {
				continue
			}
			sr, ok := ex.(executor.SudoRules)
			if !ok {
				continue
			}
			for _, tn := range svc.Targets {
				t, ok := cfg.Target(tn)
				if !ok {
					continue
				}
				data := tmpl.ForTarget(*t)
				data.Service, data.Version, data.PreviousVersion = svc.Name, "*", "*"
				rs, err := sr.SudoRules(&executor.RunContext{Target: *t, Data: data})
				if err != nil {
					note(fmt.Sprintf("executor %s on %s: %v", name, tn, err))
					continue
				}
				add(svc.Name, name, rs)
			}
		}
		if tname := svc.Rollback.Traffic; tname != "" {
			spec := cfg.Traffic[tname]
			if spec.Type != "nginx" && spec.Type != "envoy" {
				continue // API-driven controllers need no commands on hosts
			}
			tc, err := executor.NewTraffic(tname, spec, executor.TrafficEnv{})
			if err != nil {
				continue
			}
			if sr, ok := tc.(executor.TrafficSudoRules); ok {
				add(svc.Name, tname, sr.SudoRules())
				if spec.Nginx != nil && (spec.Nginx.TestCmd != "nginx -t" || spec.Nginx.ReloadCmd != "nginx -s reload") {
					note(fmt.Sprintf("traffic %s: a custom test_cmd or reload_cmd runs as written: add a rule for it", tname))
				}
			}
		}
	}
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].Host < rules[j].Host })
	return rules, notes
}

// DefaultPaths are used when the host cannot be asked (`command -v`).
var DefaultPaths = map[string]string{
	"ln": "/usr/bin/ln", "mv": "/usr/bin/mv", "cp": "/usr/bin/cp", "tee": "/usr/bin/tee",
	"systemctl": "/usr/bin/systemctl", "nginx": "/usr/sbin/nginx", "virsh": "/usr/bin/virsh",
}

// Resolve finds the absolute path of a command on the host (sudoers needs
// it). ok is false when it fell back to DefaultPaths.
func Resolve(ctx context.Context, r transport.Runner, command string) (path string, ok bool) {
	if strings.HasPrefix(command, "/") {
		return command, true
	}
	if r != nil {
		if out, err := r.Run(ctx, "command -v "+transport.ShellQuote(command), nil); err == nil {
			if p := strings.TrimSpace(out); strings.HasPrefix(p, "/") {
				return p, true
			}
		}
	}
	if p, found := DefaultPaths[command]; found {
		return p, false
	}
	return "/usr/bin/" + command, false
}

// escape quotes the characters sudoers treats specially in command arguments.
func escape(args string) string {
	return strings.NewReplacer(`\`, `\\`, `,`, `\,`, `:`, `\:`, `=`, `\=`).Replace(args)
}

// Host is one host's rules, ready to print.
type Host struct {
	Name     string
	User     string
	Scope    string // connection.sudo_scope as configured ("", all, changes)
	Sudo     bool
	Rules    []Rule
	Paths    map[string]string // command -> absolute path
	Guessed  map[string]bool   // paths not confirmed on the host
	Comments []string
}

// Group splits rules per host and fills in the login user from the target's credential.
func Group(cfg *config.Config, rules []Rule) []*Host {
	by := map[string]*Host{}
	var order []string
	for _, r := range rules {
		h, ok := by[r.Host]
		if !ok {
			h = &Host{Name: r.Host, User: "vigilante", Paths: map[string]string{}, Guessed: map[string]bool{}}
			if t, ok := cfg.Target(r.Host); ok {
				h.Sudo, h.Scope = t.Connection.Sudo, t.Connection.SudoScope
				if c, ok := cfg.Credentials[t.Connection.Credential]; ok && c.User != "" {
					h.User = c.User
				}
			}
			by[r.Host] = h
			order = append(order, r.Host)
		}
		h.Rules = append(h.Rules, r)
	}
	out := make([]*Host, 0, len(order))
	for _, n := range order {
		out = append(out, by[n])
	}
	return out
}

// Render prints a sudoers.d file for one host.
func (h *Host) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# /etc/sudoers.d/vigilante on %s, generated by `vigilante sudoers`.\n", h.Name)
	b.WriteString("# Install with: visudo -cf FILE && install -m 0440 FILE /etc/sudoers.d/vigilante\n")
	if !h.Sudo || h.Scope != "changes" {
		b.WriteString("# NOTE: this target does not have connection.sudo: true with sudo_scope: changes yet;\n#       these rules take effect once it does.\n")
	}
	b.WriteString("# '*' matches any text, spaces included: keep the release and snapshot directories\n# writable only by trusted accounts.\n")
	for _, c := range h.Comments {
		b.WriteString("# " + c + "\n")
	}
	fmt.Fprintf(&b, "Defaults:%s !requiretty\n", h.User)
	for _, r := range h.Rules {
		path := h.Paths[r.Command]
		if path == "" {
			path, _ = Resolve(context.Background(), nil, r.Command)
		}
		guess := ""
		if h.Guessed[r.Command] {
			guess = " (path not confirmed on the host)"
		}
		fmt.Fprintf(&b, "# %s / %s: %s%s\n", r.Service, r.Source, r.Why, guess)
		fmt.Fprintf(&b, "%s ALL=(root) NOPASSWD: %s %s\n", h.User, path, escape(r.Args))
	}
	return b.String()
}

// Example replaces wildcards with a sample value, for `sudo -n -l` checks.
func Example(args string) string { return strings.ReplaceAll(args, "*", "vigilante-check") }

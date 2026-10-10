package executor

import (
	"context"
	"fmt"
	"path"
	"strings"

	"vigilante/internal/config"
	q "vigilante/internal/transport"
)

func init() { Register("symlink", newSymlink) }

// Strategy A — VM / bare-metal directory switch.
//
// Layout assumed (Capistrano-style):
//
//	/opt/app/releases/v41   /opt/app/releases/v42   /opt/app/current -> releases/v42
//
// Rollback re-points `current` atomically (ln -s to a temp name + rename(2),
// so there is never a moment without a link) and restarts the service.
type symlinkExec struct {
	spec *config.SymlinkExec
}

func newSymlink(_ string, spec config.Executor) (Executor, error) {
	return &symlinkExec{spec: spec.Symlink}, nil
}

func (s *symlinkExec) atomic() bool { return s.spec.Atomic == nil || *s.spec.Atomic }

// Prepare records where `current` points before the deployment touches it.
func (s *symlinkExec) Prepare(ctx context.Context, rc *RunContext) (map[string]string, error) {
	r, err := rc.runner()
	if err != nil {
		return nil, err
	}
	link, err := rc.render(s.spec.Link)
	if err != nil {
		return nil, err
	}
	out, err := r.Run(ctx, "readlink -f "+q.ShellQuote(link), nil)
	if err != nil {
		return nil, err
	}
	return map[string]string{"symlink.previous": strings.TrimSpace(out)}, nil
}

func (s *symlinkExec) release(rc *RunContext) (link, target string, err error) {
	if link, err = rc.render(s.spec.Link); err != nil {
		return
	}
	// A checkpoint taken by `prepare` wins over the computed path.
	if prev := rc.Checkpoint["symlink.previous"]; prev != "" {
		return link, prev, nil
	}
	dir, err := rc.render(s.spec.ReleasesDir)
	if err != nil {
		return
	}
	rel, err := rc.render(s.spec.Release)
	if err != nil {
		return
	}
	if rel == "" {
		return "", "", fmt.Errorf("no previous release known (set previous_version or run prepare)")
	}
	if strings.HasPrefix(rel, "/") {
		return link, rel, nil
	}
	return link, path.Join(dir, rel), nil
}

// restartCmd is the service restart; sudo prefixes the built-in ones (a
// custom restart_cmd is used as written).
func (s *symlinkExec) restartCmd(rc *RunContext, sudo string) (string, error) {
	if s.spec.RestartCmd != "" {
		return rc.render(s.spec.RestartCmd)
	}
	unit, err := rc.render(s.spec.Unit)
	if err != nil {
		return "", err
	}
	switch s.spec.Init {
	case "systemd":
		return sudo + "systemctl restart " + q.ShellQuote(unit), nil
	case "sysv":
		return sudo + "/etc/init.d/" + unit + " restart", nil
	}
	return "", nil
}

// SudoRules: switching the link and restarting the unit.
func (s *symlinkExec) SudoRules(rc *RunContext) ([]SudoRule, error) {
	link, err := rc.render(s.spec.Link)
	if err != nil {
		return nil, err
	}
	dir, err := rc.render(s.spec.ReleasesDir)
	if err != nil {
		return nil, err
	}
	host := rc.Target.Name
	var out []SudoRule
	if s.atomic() {
		out = append(out,
			SudoRule{host, "ln", "-sfn " + dir + "/* " + link + ".vigilante-tmp", "point a temporary link at the previous release"},
			SudoRule{host, "mv", "-Tf " + link + ".vigilante-tmp " + link, "swap the release link atomically"})
	} else {
		out = append(out, SudoRule{host, "ln", "-sfn " + dir + "/* " + link, "point the release link at the previous release"})
	}
	if s.spec.RestartCmd == "" {
		unit, err := rc.render(s.spec.Unit)
		if err != nil {
			return nil, err
		}
		switch s.spec.Init {
		case "systemd":
			out = append(out, SudoRule{host, "systemctl", "restart " + unit, "restart the service"})
		case "sysv":
			out = append(out, SudoRule{host, "/etc/init.d/" + unit, "restart", "restart the service"})
		}
	}
	return out, nil
}

func (s *symlinkExec) Rollback(ctx context.Context, rc *RunContext) error {
	r, err := rc.runner()
	if err != nil {
		return err
	}
	link, target, err := s.release(rc)
	if err != nil {
		return err
	}
	tmp := link + ".vigilante-tmp"
	sudo := q.Sudo(r)
	var b strings.Builder
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "test -d %s\n", q.ShellQuote(target))
	if s.atomic() {
		fmt.Fprintf(&b, "%sln -sfn %s %s\n", sudo, q.ShellQuote(target), q.ShellQuote(tmp))
		fmt.Fprintf(&b, "%smv -Tf %s %s\n", sudo, q.ShellQuote(tmp), q.ShellQuote(link))
	} else { // AIX / Solaris: no mv -T; ln -sfn is the best available
		fmt.Fprintf(&b, "%sln -sfn %s %s\n", sudo, q.ShellQuote(target), q.ShellQuote(link))
	}
	restart, err := s.restartCmd(rc, sudo)
	if err != nil {
		return err
	}
	if restart != "" {
		b.WriteString(restart + "\n")
	}
	if _, err := r.Run(ctx, b.String(), nil); err != nil {
		return fmt.Errorf("symlink rollback to %s: %w", target, err)
	}
	return nil
}

func (s *symlinkExec) Verify(ctx context.Context, rc *RunContext) error {
	if rc.DryRun {
		return nil
	}
	r, err := rc.runner()
	if err != nil {
		return err
	}
	link, target, err := s.release(rc)
	if err != nil {
		return err
	}
	got, err := r.Run(ctx, "readlink -f "+q.ShellQuote(link), nil)
	if err != nil {
		return err
	}
	want, err := r.Run(ctx, "readlink -f "+q.ShellQuote(target), nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(got) != strings.TrimSpace(want) {
		return fmt.Errorf("%s points to %s, want %s", link, strings.TrimSpace(got), strings.TrimSpace(want))
	}
	if s.spec.Init == "systemd" && s.spec.RestartCmd == "" {
		unit, _ := rc.render(s.spec.Unit)
		out, err := r.Run(ctx, "systemctl is-active "+q.ShellQuote(unit), nil)
		if err != nil || strings.TrimSpace(out) != "active" {
			return fmt.Errorf("unit %s not active (%s): %v", unit, strings.TrimSpace(out), err)
		}
	}
	return nil
}

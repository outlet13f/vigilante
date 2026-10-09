// Package transport gives probes and executors one way to reach a target no
// matter where it lives: a local shell, SSH (optionally through a bastion), or
// nothing at all for API-only targets. It also tunnels unix sockets (docker.sock,
// HAProxy runtime API) over SSH so no agent or exposed TCP port is needed.
package transport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// Runner executes shell commands on a target.
type Runner interface {
	// Run executes cmd and returns stdout. stdin may be nil.
	Run(ctx context.Context, cmd string, stdin io.Reader) (string, error)
	// Stream executes a long-running cmd (e.g. tail -F) and calls onLine per
	// stdout line until ctx is cancelled or the command exits.
	Stream(ctx context.Context, cmd string, onLine func(string)) error
	// Dial opens a connection from the target's point of view
	// (network "unix" or "tcp"); used to reach docker.sock over SSH.
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
	String() string
}

// ErrNoRunner is returned when a target has connection type "none".
var ErrNoRunner = errors.New("target has no command connection (connection.type: none)")

// CommandError carries the exit status and stderr of a failed command.
type CommandError struct {
	Cmd    string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if len(msg) > 400 {
		msg = msg[:400] + "..."
	}
	return fmt.Sprintf("command %q failed: %v: %s", truncate(e.Cmd, 120), e.Err, msg)
}
func (e *CommandError) Unwrap() error { return e.Err }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ShellQuote single-quotes s for POSIX sh.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Local runs commands on the machine executing vigilante.
type Local struct {
	Sudo bool
}

func (l *Local) String() string { return "local" }

func (l *Local) command(ctx context.Context, cmd string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", cmd)
	}
	if l.Sudo {
		return exec.CommandContext(ctx, "sudo", "-n", "sh", "-c", cmd)
	}
	return exec.CommandContext(ctx, "sh", "-c", cmd)
}

func (l *Local) Run(ctx context.Context, cmd string, stdin io.Reader) (string, error) {
	c := l.command(ctx, cmd)
	var out, errb strings.Builder
	c.Stdout, c.Stderr, c.Stdin = &out, &errb, stdin
	if err := c.Run(); err != nil {
		return out.String(), &CommandError{Cmd: cmd, Stderr: errb.String(), Err: err}
	}
	return out.String(), nil
}

func (l *Local) Stream(ctx context.Context, cmd string, onLine func(string)) error {
	c := l.command(ctx, cmd)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return err
	}
	scanLines(stdout, onLine)
	return c.Wait()
}

func (l *Local) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func scanLines(r io.Reader, onLine func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		onLine(sc.Text())
	}
}

// DryRun wraps a Runner: simple read-only commands (readlink, cat, ...) pass
// through so checkpoints still work; everything else is only logged.
type DryRun struct {
	Inner  Runner
	Log    *slog.Logger
	mu     sync.Mutex
	Issued []string
}

func (d *DryRun) String() string { return "dry-run(" + d.Inner.String() + ")" }

var readOnlyPrefixes = []string{"readlink ", "cat ", "test ", "systemctl is-active ", "virsh domstate ", "virsh snapshot-list ", "ls "}

func (d *DryRun) Run(ctx context.Context, cmd string, stdin io.Reader) (string, error) {
	for _, p := range readOnlyPrefixes {
		if strings.HasPrefix(cmd, p) && !strings.ContainsAny(cmd, ";&|>") {
			return d.Inner.Run(ctx, cmd, stdin)
		}
	}
	d.mu.Lock()
	d.Issued = append(d.Issued, cmd)
	d.mu.Unlock()
	d.Log.Info("DRY-RUN: would execute", "runner", d.Inner.String(), "cmd", cmd)
	return "", nil
}

func (d *DryRun) Stream(ctx context.Context, cmd string, onLine func(string)) error {
	return d.Inner.Stream(ctx, cmd, onLine)
}

func (d *DryRun) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return d.Inner.Dial(ctx, network, addr)
}

//go:build !windows

package transport

import (
	"context"
	"os/exec"
)

// shellCommand runs cmd through the POSIX shell.
func shellCommand(ctx context.Context, cmd string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", cmd)
}

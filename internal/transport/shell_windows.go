//go:build windows

package transport

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand runs cmd through cmd.exe. The command line is passed as is:
// Go's argument escaping (backslash before quotes) is not what cmd.exe
// parses, so `curl -X POST "http://host/a?b=c"` would reach curl with
// literal backslashes.
func shellCommand(ctx context.Context, cmd string) *exec.Cmd {
	c := exec.CommandContext(ctx, "cmd")
	c.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd /S /C "` + cmd + `"`}
	return c
}

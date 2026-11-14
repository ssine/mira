//go:build !windows

package clauderuntime

import (
	"os/exec"
	"syscall"
	"time"
)

func configureCommand(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// A worker's native SDK child shares its process group. Clean up remaining
// descendants before acknowledging exit, even when the wrapper crashed.
func guardProcessTree(cmd *exec.Cmd) (func() bool, error) {
	return func() bool {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return err == nil || err == syscall.ESRCH
	}, nil
}
func killCommand(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// settleProcessGroup lets descendants that outlive the worker wrapper (the
// native Claude CLI) finish their exit path before they are killed. The CLI
// saves a rotated OAuth refresh token on exit; killing it, or closing the
// account egress, between the token response and that write leaves a dead
// refresh token on disk and every later turn fails with HTTP 401. Callers
// release account egress only after this returns.
func settleProcessGroup(cmd *exec.Cmd, grace time.Duration) {
	if cmd.Process == nil {
		return
	}
	group := -cmd.Process.Pid
	if syscall.Kill(group, 0) == syscall.ESRCH {
		return
	}
	_ = syscall.Kill(group, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if syscall.Kill(group, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(group, syscall.SIGKILL)
}

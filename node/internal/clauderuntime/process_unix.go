//go:build !windows

package clauderuntime

import (
	"os/exec"
	"syscall"
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

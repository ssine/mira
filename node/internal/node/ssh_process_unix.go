//go:build !windows

package node

import (
	pty "github.com/aymanbagabas/go-pty"
	"os"
	"os/exec"
	"syscall"
)

func configureSSHCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
func configureSSHPTY(cmd *pty.Cmd) {
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
func signalSSHProcess(process *os.Process, name string) error {
	sig := syscall.SIGTERM
	if name == "INT" {
		sig = syscall.SIGINT
	}
	if name == "KILL" {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-process.Pid, sig)
}

func guardSSHProcessTree(process *os.Process) (func(), error) { return func() {}, nil }

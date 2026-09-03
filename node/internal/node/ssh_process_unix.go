//go:build !windows

package node

import (
	"io"
	"os"
	"os/exec"
	"syscall"

	pty "github.com/aymanbagabas/go-pty"
)

func configureSSHCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
func configureSSHPTY(cmd *pty.Cmd) {
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

func sshPTYReader(terminal pty.Pty) (io.Reader, func(), error) { return terminal, func() {}, nil }
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

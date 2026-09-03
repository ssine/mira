//go:build !windows

package node

import (
	"os"
	"os/exec"
	"syscall"
)

func terminateProcess(process *os.Process, signalName string) error {
	signal := syscall.SIGTERM
	switch signalName {
	case "SIGINT":
		signal = syscall.SIGINT
	case "SIGKILL":
		signal = syscall.SIGKILL
	}
	return process.Signal(signal)
}

func processExitSignal(err error) string {
	exitError, ok := err.(*exec.ExitError)
	if !ok {
		return ""
	}
	status, ok := exitError.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	return status.Signal().String()
}

func currentUserIsRoot() bool {
	return os.Geteuid() == 0
}

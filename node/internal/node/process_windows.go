//go:build windows

package node

import (
	"os"
)

func terminateProcess(process *os.Process, signalName string) error {
	if signalName == "SIGKILL" || signalName == "SIGTERM" {
		return process.Kill()
	}
	return process.Signal(os.Interrupt)
}

func processExitSignal(error) string {
	return ""
}

func currentUserIsRoot() bool {
	return false
}

package node

import (
	"os/exec"
	"syscall"
)

// Captured, non-interactive child processes must not create a console when the
// Node has none. Interactive CLI and ConPTY launch paths deliberately bypass this.
func backgroundCommand(command *exec.Cmd) *exec.Cmd {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
	return command
}

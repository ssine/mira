//go:build !windows

package node

import "os/exec"

func backgroundCommand(command *exec.Cmd) *exec.Cmd { return command }

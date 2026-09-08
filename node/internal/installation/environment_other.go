//go:build !linux

package installation

import "os/exec"

func configureServiceEnvironment(command *exec.Cmd) {}

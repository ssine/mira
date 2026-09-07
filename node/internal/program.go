package node

import (
	"context"
	"errors"
	"fmt"
	"io"
)

var ErrSupervisorHandoff = errors.New("Mira Supervisor handed off to the installed release")

// RunSystemRole recognizes explicit process roles that must bypass normal
// remote CLI authentication. Mira roles are selected only by arguments; the
// executable name is reserved for the embedded OpenSSH entry points.
func RunSystemRole(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (bool, int) {
	if !isSystemRole(args) {
		return false, 0
	}
	if args[0] == "ssh-command-worker" {
		code, err := RunSSHCommandWorker(ctx, args[1:], stdin, stdout, stderr)
		if err != nil && err != context.Canceled {
			fmt.Fprintln(stderr, err)
		}
		return true, code
	}
	var err error
	switch args[0] {
	case "node-worker":
		err = RunNodeWorker(ctx, args[1:])
	case "server-worker":
		err = RunServerWorker(ctx, args[1:])
	case "server":
		err = RunServerAdmin(ctx, args[2:], stdin, stdout)
	case "supervisor":
		err = RunSupervisor(ctx, args[1:])
	case "supervisor-check":
		err = ValidateSupervisorCandidate(args[1:])
	case "ssh-worker", "--internal-ssh-worker":
		err = RunSSHWorker(ctx)
	}
	if err != nil && err != context.Canceled {
		fmt.Fprintln(stderr, err)
		return true, 1
	}
	return true, 0
}

func isSystemRole(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "node-worker" || args[0] == "server-worker" || args[0] == "supervisor" || args[0] == "supervisor-check" || args[0] == "ssh-worker" || args[0] == "--internal-ssh-worker" || args[0] == "ssh-command-worker" {
		return true
	}
	return len(args) >= 2 && args[0] == "server" && args[1] == "admin"
}

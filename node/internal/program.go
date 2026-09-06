package node

import (
	"context"
	"errors"
	"fmt"
	"io"
)

var ErrSupervisorHandoff = errors.New("Mira Supervisor handed off to the installed release")

// RunSystemRole recognizes process roles that must bypass normal remote CLI
// authentication. The embedded OpenSSH dispatcher inserts a leading "cli"
// when the binary is invoked through its mira alias, so both forms are
// accepted without exposing a second executable.
func RunSystemRole(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (bool, int) {
	roleArgs := args
	if len(roleArgs) > 1 && roleArgs[0] == "cli" && isSystemRole(roleArgs[1:]) {
		roleArgs = roleArgs[1:]
	}
	if !isSystemRole(roleArgs) {
		return false, 0
	}
	var err error
	switch roleArgs[0] {
	case "node-worker":
		err = RunNodeWorker(ctx, roleArgs[1:])
	case "server-worker":
		err = RunServerWorker(ctx, roleArgs[1:])
	case "server":
		err = RunServerAdmin(ctx, roleArgs[2:], stdin, stdout)
	case "supervisor":
		err = RunSupervisor(ctx, roleArgs[1:])
	case "supervisor-check":
		err = ValidateSupervisorCandidate(roleArgs[1:])
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
	if args[0] == "node-worker" || args[0] == "server-worker" || args[0] == "supervisor" || args[0] == "supervisor-check" {
		return true
	}
	return len(args) >= 2 && args[0] == "server" && args[1] == "admin"
}

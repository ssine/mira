//go:build !windows

package node

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// OpenSSH terminates its ProxyCommand with SIGHUP after the session ends.
// HTTP polling needs an explicit close because process exit alone leaves its
// server-side session alive until the lease expires.
func sshProxyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, syscall.SIGHUP)
}

func guardSSHProcessTree(process *os.Process) (func(), error) { return func() {}, nil }

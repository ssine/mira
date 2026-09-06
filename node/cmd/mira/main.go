package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	miranode "github.com/ssine/mira/node/internal"
)

func main() {
	if handled, code := miranode.RunWindowsServiceIfNeeded(os.Args[1:]); handled {
		os.Exit(code)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if handled, code := miranode.RunSystemRole(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	os.Exit(miranode.RunCLI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

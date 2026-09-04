package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	miranode "github.com/ssine/mira/node/internal"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	os.Exit(miranode.RunCLI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	miranode "github.com/ssine/mira/node/internal/node"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := miranode.Run(ctx); err != nil && err != context.Canceled {
		miranode.Log("Mira Node failed", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
	miranode.Log("Mira Node stopped", nil)
}

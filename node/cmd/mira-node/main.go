package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	miranode "github.com/ssine/mira/node/internal/node"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		if err := miranode.PrintVersion("mira-node", false); err != nil {
			os.Exit(1)
		}
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := miranode.Run(ctx); err != nil && err != context.Canceled {
		miranode.Log("Mira Node failed", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
	miranode.Log("Mira Node stopped", nil)
}

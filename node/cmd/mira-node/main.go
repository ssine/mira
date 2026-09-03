package main

import (
	"context"
	"fmt"
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
	// APKs ship one executable. The same embedded client can be invoked from
	// a Node process/shell with its normal MIRA_IDENTITY_FILE environment.
	if len(os.Args) > 1 && os.Args[1] == "cli" {
		os.Exit(miranode.RunCLI(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) == 2 && os.Args[1] == "--internal-ssh-worker" {
		if err := miranode.RunSSHWorker(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "SSH worker:", err)
			os.Exit(1)
		}
		return
	}
	if err := miranode.Run(ctx); err != nil && err != context.Canceled {
		miranode.Log("Mira Node failed", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
	miranode.Log("Mira Node stopped", nil)
}

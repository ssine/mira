package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	miranode "github.com/ssine/mira/node/internal"
)

func main() {
	if handled, code := miranode.RunWindowsServiceIfNeeded(os.Args[1:]); handled {
		os.Exit(code)
	}
	if len(os.Args) == 2 && os.Args[1] == "--mira-tray-build" {
		if !miranode.SupportsTray() {
			os.Exit(1)
		}
		fmt.Println("MIRA_WINDOWS_TRAY_V1")
		return
	}
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		if err := miranode.PrintVersion("mira", false); err != nil {
			os.Exit(1)
		}
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if handled, code := miranode.RunSystemRole(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	if len(os.Args) > 1 && os.Args[1] == "cli" {
		os.Exit(miranode.RunCLI(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "--tray" {
		if err := miranode.RunTray(ctx, os.Args[2:]); err != nil && err != context.Canceled {
			miranode.Log("Mira tray failed", map[string]any{"error": err.Error()})
			os.Exit(1)
		}
		return
	}
	os.Exit(miranode.RunCLI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

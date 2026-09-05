//go:build !windows

package node

import (
	"context"
	"fmt"
)

func RunTray(context.Context, []string) error {
	return fmt.Errorf("--tray is only supported on Windows; run mira-node without --tray on this platform")
}

func SupportsTray() bool { return false }

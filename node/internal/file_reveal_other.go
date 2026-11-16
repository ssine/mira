//go:build !windows && (!linux || android)

package node

import (
	"context"
	"fmt"
)

func desktopRevealAvailable() bool { return false }

func revealInFileManager(context.Context, executionRequest, string, bool) (string, error) {
	return "", fmt.Errorf("this Node has no desktop file manager: only Windows and WSL with interop can reveal files")
}

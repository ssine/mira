//go:build !windows

package node

import (
	"context"
	"fmt"
	"io"
)

func RunSSHCommandWorker(context.Context, []string, io.Reader, io.Writer, io.Writer) (int, error) {
	return 255, fmt.Errorf("Windows SSH text worker is unavailable on this platform")
}

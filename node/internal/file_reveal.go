package node

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// revealResolvedFile shows an authorized, existing path in the desktop file
// manager of the machine this Node runs on. It starts one fixed system program
// with that path; the target itself is never opened or executed.
func revealResolvedFile(ctx context.Context, request executionRequest, value any) (any, error) {
	stat, _ := value.(map[string]any)
	target, _ := stat["path"].(string)
	if target == "" {
		return nil, fmt.Errorf("reveal target is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	desktopPath, err := revealInFileManager(ctx, request, target, stat["type"] == "directory")
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": target, "type": stat["type"], "revealed": true, "desktopPath": desktopPath}, nil
}

// reapFileManager collects the short-lived launcher. Explorer hands the request
// to the running shell and exits non-zero even on success, so the status is
// not a result.
func reapFileManager(command *exec.Cmd) {
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = command.Process.Kill()
		<-done
	}
}

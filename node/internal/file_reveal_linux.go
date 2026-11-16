//go:build linux && !android

package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var wslExplorer struct {
	sync.Mutex
	path      string
	checkedAt time.Time
}

// wslExplorerPath locates Windows Explorer through WSL interop. A Node started
// as a service usually has no Windows directories on PATH, so the automount
// translation is the reliable source. A miss is retried, since interop and
// automount can become ready after the Node starts, but rarely and briefly:
// this runs while the Node reports its capabilities with each heartbeat.
func wslExplorerPath() string {
	if detectNodeMode() != "wsl" {
		return ""
	}
	wslExplorer.Lock()
	defer wslExplorer.Unlock()
	if wslExplorer.path != "" || time.Since(wslExplorer.checkedAt) < 5*time.Minute {
		return wslExplorer.path
	}
	wslExplorer.checkedAt = time.Now()
	if path, err := exec.LookPath("explorer.exe"); err == nil {
		wslExplorer.path = path
		return path
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := commandOutput(ctx, "wslpath", "-u", `C:\Windows\explorer.exe`)
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(output)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		wslExplorer.path = path
	}
	return wslExplorer.path
}

func desktopRevealAvailable() bool { return wslExplorerPath() != "" }

func revealInFileManager(ctx context.Context, _ executionRequest, target string, directory bool) (string, error) {
	explorer := wslExplorerPath()
	if explorer == "" {
		return "", fmt.Errorf("this Node has no desktop file manager: only Windows and WSL with interop can reveal files")
	}
	translate, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// wslpath yields \\wsl.localhost\<distribution>\... for distribution files
	// and a drive path for Windows mounts.
	output, err := commandOutput(translate, "wslpath", "-w", target)
	if err != nil {
		return "", fmt.Errorf("translate path for Windows Explorer: %w", err)
	}
	desktopPath := strings.TrimRight(output, "\r\n")
	if desktopPath == "" {
		return "", fmt.Errorf("translate path for Windows Explorer: empty result")
	}
	// Interop quotes each argument that needs it, producing the documented
	// `/select, "path"` form; one combined argument would be quoted as a whole
	// and Explorer would not recognize the switch.
	args := []string{desktopPath}
	if !directory {
		args = []string{"/select,", desktopPath}
	}
	command := exec.Command(explorer, args...)
	command.Dir = filepath.Dir(explorer)
	if err := command.Start(); err != nil {
		return "", fmt.Errorf("start Windows Explorer: %w", err)
	}
	go reapFileManager(command)
	return desktopPath, nil
}

package node

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// localMiraCLIPath returns the absolute control-CLI path that belongs to this
// Mira installation. App Server clients use it to tell Codex how to open
// the end-to-end SSH transport without assuming that mira is on PATH.
func localMiraCLIPath() string {
	if runtime.GOOS == "android" {
		return ""
	}
	name := "mira"
	if runtime.GOOS == "windows" {
		name = "mira.exe"
	}
	if executable, err := os.Executable(); err == nil {
		// Unix installations expose <state>/current/mira, which follows updates.
		// A versioned path would keep long-lived conversations on the old CLI.
		// Windows keeps the versioned image: its current pointer is a file.
		if stateDir, installed := installedStateDir(executable); installed && runtime.GOOS != "windows" {
			if candidate := existingAbsoluteFile(filepath.Join(stateDir, "current", name)); candidate != "" {
				return candidate
			}
		}
		if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
			executable = resolved
		}
		if candidate := existingAbsoluteFile(filepath.Join(filepath.Dir(executable), name)); candidate != "" {
			return candidate
		}
	}
	if candidate, err := exec.LookPath(name); err == nil {
		return existingAbsoluteFile(candidate)
	}
	return ""
}

func existingAbsoluteFile(candidate string) string {
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return ""
	}
	info, err := os.Stat(absolute)
	if err != nil || info.IsDir() {
		return ""
	}
	return filepath.Clean(absolute)
}

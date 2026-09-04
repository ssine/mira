package node

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// localMiraCLIPath returns the absolute control-CLI path that belongs to this
// installed Mira release. App Server clients use it to tell Codex how to open
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

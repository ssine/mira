//go:build !windows

package node

import (
	"os"
	"os/exec"
	"path/filepath"
)

func codexCandidatePaths(configured string) []string {
	if configured != "" {
		return []string{configured}
	}
	result := []string{}
	if executable, err := os.Executable(); err == nil {
		directory := filepath.Dir(executable)
		for _, bundled := range []string{
			filepath.Join(directory, "mira-codex-package", "bin", "codex"),
			filepath.Join(directory, "mira-codex"), // pre-canonical development bundles
		} {
			if info, statErr := os.Stat(bundled); statErr == nil && !info.IsDir() {
				result = append(result, bundled)
			}
		}
	}
	if candidate, err := exec.LookPath("codex"); err == nil {
		result = append(result, candidate)
	}
	return result
}

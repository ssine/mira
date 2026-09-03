//go:build !windows

package node

import "os/exec"

func codexCandidatePaths(configured string) []string {
	if configured != "" {
		return []string{configured}
	}
	if candidate, err := exec.LookPath("codex"); err == nil {
		return []string{candidate}
	}
	return nil
}

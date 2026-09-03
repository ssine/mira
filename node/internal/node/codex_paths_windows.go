//go:build windows

package node

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func codexCandidatePaths(configured string) []string {
	candidates := []string{}
	if configured != "" {
		candidates = append(candidates, configured)
	} else {
		if executable, err := os.Executable(); err == nil {
			bundled := filepath.Join(filepath.Dir(executable), "mira-codex.exe")
			if info, statErr := os.Stat(bundled); statErr == nil && !info.IsDir() {
				candidates = append(candidates, bundled)
			}
		}
		for _, name := range []string{"codex.exe", "codex.cmd", "codex"} {
			if candidate, err := exec.LookPath(name); err == nil {
				candidates = append(candidates, candidate)
			}
		}
	}
	result := []string{}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		paths := []string{candidate}
		if strings.EqualFold(filepath.Ext(candidate), ".cmd") || strings.EqualFold(filepath.Ext(candidate), ".ps1") {
			if native := nativeCodexFromNPMShim(candidate); native != "" {
				paths = []string{native}
			}
		}
		for _, path := range paths {
			key := strings.ToLower(path)
			if !seen[key] {
				seen[key] = true
				result = append(result, path)
			}
		}
	}
	return result
}

func nativeCodexFromNPMShim(shim string) string {
	arch, triple := "x64", "x86_64-pc-windows-msvc"
	if runtime.GOARCH == "arm64" {
		arch, triple = "arm64", "aarch64-pc-windows-msvc"
	}
	base := filepath.Dir(shim)
	packageRoot := filepath.Join(base, "node_modules", "@openai", "codex")
	vendors := []string{
		filepath.Join(packageRoot, "node_modules", "@openai", "codex-win32-"+arch, "vendor"),
		filepath.Join(base, "node_modules", "@openai", "codex-win32-"+arch, "vendor"),
		filepath.Join(packageRoot, "vendor"),
	}
	for _, vendor := range vendors {
		// Both layouts have shipped in official Codex npm distributions.
		for _, directory := range []string{"bin", "codex"} {
			candidate := filepath.Join(vendor, triple, directory, "codex.exe")
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	return ""
}

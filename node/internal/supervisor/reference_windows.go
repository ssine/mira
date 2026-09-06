//go:build windows

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Windows deliberately uses a version-name file because ordinary user installs
// cannot assume permission to create symlinks. Service wiring must use the path
// returned by CurrentExecutable and explicitly update it during handoff.
func readVersionReference(layout Layout, name string) (string, error) {
	content, err := os.ReadFile(layout.pointerPath(name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(content)), nil
}

func writeVersionReference(layout Layout, name, version string) error {
	return layout.writeStateFile(layout.pointerPath(name), version+"\n")
}

func currentExecutable(layout Layout) (string, error) {
	version, err := layout.CurrentVersion()
	if err != nil {
		return "", err
	}
	directory, err := layout.VersionDir(version)
	if err != nil {
		return "", err
	}
	executable := filepath.Join(directory, "mira.exe")
	if info, err := os.Stat(executable); err == nil && !info.IsDir() {
		return executable, nil
	}
	return "", fmt.Errorf("current Mira version %s has no canonical mira.exe executable", version)
}

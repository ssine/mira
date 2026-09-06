//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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
	for _, name := range []string{"mira", "mira-node"} {
		executable := filepath.Join(directory, name)
		if info, err := os.Stat(executable); err == nil && !info.IsDir() {
			return executable, nil
		}
	}
	return "", fmt.Errorf("current Mira version %s has no executable", version)
}

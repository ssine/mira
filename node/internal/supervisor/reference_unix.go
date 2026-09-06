//go:build aix || android || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
)

func readVersionReference(layout Layout, name string) (string, error) {
	target, err := os.Readlink(layout.pointerPath(name))
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(target) {
		return "", fmt.Errorf("%s version link must be relative", name)
	}
	resolved := filepath.Clean(filepath.Join(layout.StateDir, target))
	relative, err := filepath.Rel(layout.VersionsDir, resolved)
	if err != nil || filepath.Dir(relative) != "." {
		return "", fmt.Errorf("%s version link is outside the versions directory", name)
	}
	return filepath.Base(relative), nil
}

func writeVersionReference(layout Layout, name, version string) error {
	destination, err := layout.VersionDir(version)
	if err != nil {
		return err
	}
	target, err := filepath.Rel(layout.StateDir, destination)
	if err != nil {
		return err
	}
	placeholder, err := os.CreateTemp(layout.StateDir, "."+name+"-*")
	if err != nil {
		return err
	}
	temporary := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		os.Remove(temporary)
		return err
	}
	if err := os.Remove(temporary); err != nil {
		return err
	}
	defer os.Remove(temporary)
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, layout.pointerPath(name)); err != nil {
		return fmt.Errorf("replace %s version link: %w", name, err)
	}
	return syncDirectory(layout.StateDir)
}

func currentExecutable(layout Layout) (string, error) {
	if _, err := layout.CurrentVersion(); err != nil {
		return "", err
	}
	executable := filepath.Join(layout.Current, "mira")
	if info, err := os.Stat(executable); err != nil {
		return "", err
	} else if info.IsDir() {
		return "", fmt.Errorf("current Mira executable is a directory")
	}
	return executable, nil
}

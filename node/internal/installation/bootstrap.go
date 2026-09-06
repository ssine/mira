package installation

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ssine/mira/node/internal/supervisor"
)

// BootstrapRelease installs the currently running single-file Mira release
// into Supervisor's immutable versions directory, then selects it as current.
// It never replaces a different binary under an existing version name.
func BootstrapRelease(source, stateDir, version string) (string, error) {
	layout, err := supervisor.NewLayout(stateDir)
	if err != nil {
		return "", err
	}
	if source == "" {
		source, err = os.Executable()
		if err != nil {
			return "", err
		}
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return "", err
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return "", fmt.Errorf("inspect Mira executable: %w", err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return "", fmt.Errorf("Mira executable is not a regular file")
	}
	directory, err := layout.VersionDir(version)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	name := "mira"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(directory, name)
	if existing, readErr := os.ReadFile(target); readErr == nil {
		sourceContent, err := os.ReadFile(source)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(existing, sourceContent) {
			return "", fmt.Errorf("Mira %s is already installed with different contents", version)
		}
		if err := ensureRoleAliases(directory, target); err != nil {
			return "", err
		}
		if err := layout.InitializeCurrent(version); err != nil {
			return "", err
		}
		return target, nil
	} else if !os.IsNotExist(readErr) {
		return "", readErr
	}
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	temporary, err := os.CreateTemp(directory, ".mira-seed-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := io.Copy(temporary, input); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Chmod(0755); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return "", err
	}
	if err := ensureRoleAliases(directory, target); err != nil {
		return "", err
	}
	if err := layout.InitializeCurrent(version); err != nil {
		return "", err
	}
	return target, nil
}

func ensureRoleAliases(directory, target string) error {
	roles := []string{"ssh", "sshd", "sshd-session", "sshd-auth", "scp", "sftp", "sftp-server", "ssh-keygen"}
	if runtime.GOOS == "windows" {
		roles = append(roles, "ssh-shellhost", "ssh-agent", "ssh-add", "ssh-keyscan", "ssh-sk-helper", "ssh-pkcs11-helper")
	}
	for _, role := range roles {
		name := role
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		alias := filepath.Join(directory, name)
		if info, err := os.Stat(alias); err == nil {
			targetInfo, targetErr := os.Stat(target)
			if targetErr != nil || !os.SameFile(info, targetInfo) {
				return fmt.Errorf("Mira role %s exists but does not reference the installed image", role)
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		var err error
		if runtime.GOOS == "windows" {
			err = os.Link(target, alias)
		} else {
			err = os.Symlink(filepath.Base(target), alias)
		}
		if err != nil {
			return fmt.Errorf("create Mira %s role: %w", role, err)
		}
	}
	return nil
}

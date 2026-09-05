package node

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// The identity remains in the app-owned noBackup directory. Only SSH root home
// and ephemeral host/auth keys live here. Keep this root-owned home private even
// though the generated sshd configuration does not use StrictModes.
func openSSHStateDirectory(state string) (string, error) {
	if !filepath.IsAbs(state) {
		return "", fmt.Errorf("OpenSSH state directory must be absolute")
	}
	if os.Geteuid() != 0 {
		return state, nil
	}
	root := filepath.Join(state, "openssh-root-home")
	if err := os.Mkdir(root, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || stat.Uid != 0 {
		return "", fmt.Errorf("OpenSSH root home must be a root-owned private directory, not a symlink")
	}
	return root, nil
}

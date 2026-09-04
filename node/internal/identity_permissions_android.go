//go:build android

package node

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Root and app mode belong to the same APK. Keep its identity owned and labeled
// like the app-private parent directory, not by the temporary privileged process.
// Otherwise a root enrollment makes the next app-only launch unable to read it.
func protectIdentityFile(file *os.File) error {
	if os.Geteuid() == 0 {
		directory := filepath.Dir(file.Name())
		parent, err := os.Stat(directory)
		if err != nil {
			return err
		}
		stat, ok := parent.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot determine Android identity owner")
		}
		if stat.Uid != 0 {
			if err := file.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
				return fmt.Errorf("preserve Android identity owner: %w", err)
			}
			label := make([]byte, 4096)
			size, err := unix.Getxattr(directory, "security.selinux", label)
			if err != nil {
				return fmt.Errorf("read Android identity directory label: %w", err)
			}
			if err := unix.Fsetxattr(int(file.Fd()), "security.selinux", label[:size], 0); err != nil {
				return fmt.Errorf("preserve Android identity label: %w", err)
			}
		}
	}
	return file.Chmod(0600)
}

func repairIdentityPermissions(path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	file, err := os.OpenFile(path, os.O_RDWR|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Android identity must be a regular file")
	}
	return protectIdentityFile(file)
}

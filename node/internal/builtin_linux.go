//go:build linux

package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func lockBuiltinStart(ctx context.Context, dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "builtin-start.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func detachSupervisor(executable string, args []string) error {
	input, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer input.Close()
	// Keep the last startup diagnostic bounded. Runtime worker logs rotate.
	output, err := os.OpenFile(filepath.Join(filepath.Dir(filepath.Dir(executable)), "builtin-startup.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	cmd := exec.Command(executable, args...)
	cmd.Stdin = input
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// This separate lifetime lock survives exec, including the interval after the
// worker manager releases its lock and before the successor starts workers.
func acquireBuiltinOwner(dir string) (*os.File, error) {
	path := filepath.Join(dir, "builtin-owner.lock")
	var file *os.File
	if value := os.Getenv("MIRA_NODE_BUILTIN_LOCK_FD"); value != "" {
		_ = os.Unsetenv("MIRA_NODE_BUILTIN_LOCK_FD")
		fd, err := strconv.Atoi(value)
		if err != nil || fd < 3 {
			return nil, fmt.Errorf("invalid inherited builtin lock")
		}
		file = os.NewFile(uintptr(fd), path)
		actual, err := file.Stat()
		expected, pathErr := os.Stat(path)
		if err != nil || pathErr != nil || !os.SameFile(actual, expected) {
			file.Close()
			return nil, fmt.Errorf("invalid inherited builtin lock file")
		}
		unix.CloseOnExec(fd)
	} else {
		var err error
		file, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("builtin Supervisor already owns this state directory: %w", err)
	}
	return file, nil
}
func replaceSupervisor(executable string, args []string, owner *os.File) error {
	if !filepath.IsAbs(executable) {
		return fmt.Errorf("Supervisor successor must be absolute")
	}
	if owner == nil {
		return fmt.Errorf("builtin Supervisor ownership lock required")
	}
	fd := int(owner.Fd())
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
		return err
	}
	err := syscall.Exec(executable, append([]string{executable}, args...), append(os.Environ(), "MIRA_NODE_BUILTIN_LOCK_FD="+strconv.Itoa(fd)))
	unix.CloseOnExec(fd)
	return err
}

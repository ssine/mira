//go:build !windows

package installation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type installLock struct{ file *os.File }

func acquireInstallLock(ctx context.Context, stateDir string) (*installLock, error) {
	file, err := os.OpenFile(filepath.Join(stateDir, ".install.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &installLock{file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			file.Close()
			return nil, fmt.Errorf("lock Mira installation: %w", err)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			file.Close()
			return nil, fmt.Errorf("lock Mira installation: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (lock *installLock) Close() error {
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	return errorsJoin(unlockErr, lock.file.Close())
}

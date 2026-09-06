//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package supervisor

import "os"

type instanceLock struct {
	file *os.File
	path string
}

func acquireInstanceLock(path string) (*instanceLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if os.IsExist(err) {
		return nil, ErrAlreadyRunning
	}
	if err != nil {
		return nil, err
	}
	return &instanceLock{file: file, path: path}, nil
}

func (lock *instanceLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	err := lock.file.Close()
	removeErr := os.Remove(lock.path)
	lock.file = nil
	if err != nil {
		return err
	}
	return removeErr
}

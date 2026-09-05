package node

import (
	"fmt"
	"os"
)

// Used by one bounded pipe reader. Splitting oversized writes keeps each file
// bounded even when native child stderr contains very long lines.
type rotatingLog struct {
	path        string
	file        *os.File
	size, limit int64
	backups     int
}

func openRotatingLog(path string, limit int64, backups int) (*rotatingLog, error) {
	if limit <= 0 || backups < 1 {
		return nil, fmt.Errorf("invalid log rotation bounds")
	}
	log := &rotatingLog{path: path, limit: limit, backups: backups}
	return log, log.open()
}

func (log *rotatingLog) open() error {
	file, err := os.OpenFile(log.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	log.file, log.size = file, info.Size()
	return nil
}

func (log *rotatingLog) rotate() error {
	if err := log.file.Close(); err != nil {
		return err
	}
	for index := log.backups; index >= 1; index-- {
		source := log.path
		if index > 1 {
			source = fmt.Sprintf("%s.%d", log.path, index-1)
		}
		destination := fmt.Sprintf("%s.%d", log.path, index)
		if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(source, destination); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return log.open()
}

func (log *rotatingLog) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		if log.size >= log.limit {
			if err := log.rotate(); err != nil {
				return written, err
			}
		}
		length := min(int64(len(data)), log.limit-log.size)
		count, err := log.file.Write(data[:int(length)])
		written, log.size, data = written+count, log.size+int64(count), data[count:]
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (log *rotatingLog) Close() error { return log.file.Close() }

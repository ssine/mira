package node

import (
	"io"
	"os"
)

type ptyProcessHandle struct {
	process   *os.Process
	stdin     io.Writer
	backend   string
	wait      func() (int, string, error)
	terminate func() error
	resize    func(cols, rows int) error
}

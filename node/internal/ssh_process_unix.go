//go:build !windows

package node

import "os"

func guardSSHProcessTree(process *os.Process) (func(), error) { return func() {}, nil }

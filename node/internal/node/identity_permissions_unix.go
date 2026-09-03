//go:build !windows

package node

import "os"

func protectIdentityFile(file *os.File) error { return file.Chmod(0600) }

//go:build !windows && !android

package node

import "os"

func protectIdentityFile(file *os.File) error { return file.Chmod(0600) }

func repairIdentityPermissions(string) error { return nil }

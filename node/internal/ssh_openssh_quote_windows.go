//go:build windows

package node

import "syscall"

func escapeSSHProxyArg(s string) string { return syscall.EscapeArg(s) }

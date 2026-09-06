//go:build !windows

package node

func RunWindowsServiceIfNeeded(_ []string) (bool, int) { return false, 0 }

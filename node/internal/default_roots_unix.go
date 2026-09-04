//go:build !windows

package node

func platformDefaultFileRoots() []string {
	return []string{"/"}
}

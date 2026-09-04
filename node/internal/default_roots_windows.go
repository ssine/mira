//go:build windows

package node

import (
	"fmt"
	"os"
)

func platformDefaultFileRoots() []string {
	roots := make([]string, 0, 4)
	for letter := 'A'; letter <= 'Z'; letter++ {
		root := fmt.Sprintf("%c:\\", letter)
		if value, err := os.Stat(root); err == nil && value.IsDir() {
			roots = append(roots, root)
		}
	}
	return roots
}

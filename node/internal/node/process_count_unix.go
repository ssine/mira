//go:build !windows

package node

import (
	"fmt"
	"os"
)

// systemProcessCount counts numeric /proc entries without transferring the
// potentially large and sensitive system process listing to Mira Server.
func systemProcessCount() (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("read /proc: %w", err)
	}
	count := 0
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name == "" {
			continue
		}
		numeric := true
		for _, value := range name {
			if value < '0' || value > '9' {
				numeric = false
				break
			}
		}
		if numeric {
			count++
		}
	}
	return count, nil
}

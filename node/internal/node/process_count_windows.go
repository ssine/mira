//go:build windows

package node

import (
	"bytes"
	"fmt"
	"os/exec"
)

func systemProcessCount() (int, error) {
	output, err := exec.Command("tasklist", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return 0, fmt.Errorf("tasklist failed: %w", err)
	}
	output = bytes.TrimSpace(output)
	if len(output) == 0 {
		return 0, nil
	}
	return bytes.Count(output, []byte{'\n'}) + 1, nil
}

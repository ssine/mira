//go:build !windows

package node

import (
	"fmt"
	"os/user"
)

func openSSHSystemUsername() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current SSH OS account: %w", err)
	}
	return u.Username, nil
}

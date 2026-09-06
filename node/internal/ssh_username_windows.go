//go:build windows

package node

import (
	"fmt"
	"os"
	"os/user"
	"strings"

	"golang.org/x/sys/windows"
)

func normalizeWindowsOpenSSHUsername(name, computer string, localSystem bool) string {
	if localSystem {
		// GetUserNameEx(NameSamCompatible) reports LocalSystem as a machine
		// account such as WORKGROUP\HOST$. That is not a logon account. Use the
		// canonical name of the well-known S-1-5-18 identity instead.
		return "system"
	}
	name = strings.ToLower(name)
	domain, account, qualified := strings.Cut(name, `\`)
	if qualified && strings.EqualFold(domain, computer) {
		return account
	}
	return name
}

func openSSHSystemUsername() (string, error) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("resolve current SSH OS account SID: %w", err)
	}
	localSystem := tokenUser.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
	if localSystem {
		return "system", nil
	}
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current SSH OS account: %w", err)
	}
	computer, _ := os.Hostname()
	return normalizeWindowsOpenSSHUsername(u.Username, computer, false), nil
}

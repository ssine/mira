//go:build !windows

package node

import "strings"

func escapeSSHProxyArg(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

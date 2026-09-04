//go:build !android

package node

func openSSHStateDirectory(state string) (string, error) { return state, nil }

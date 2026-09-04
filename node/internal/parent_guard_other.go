//go:build !android

package node

func enableParentExitGuard() error {
	return nil
}

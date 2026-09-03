//go:build !android

package node

type androidBridge struct{}

func newAndroidBridge(string, string) *androidBridge {
	return nil
}

//go:build !linux && !android

package node

// Windows supervisor and child jobs provide kill-on-close process-tree cleanup.
func prepareOpenSSHWorker() (func(), error) { return func() {}, nil }

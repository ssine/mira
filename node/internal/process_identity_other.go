//go:build !linux

package node

import "context"

func (runtimeValue *capabilityRuntime) processIdentityStatus(context.Context) map[string]any {
	return nil
}

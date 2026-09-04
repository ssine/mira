//go:build !android

package node

import (
	"context"
	"fmt"
)

func (runtimeValue *capabilityRuntime) screen(context.Context, screenParams) (any, error) {
	return nil, fmt.Errorf("screen capability is only available on Android")
}

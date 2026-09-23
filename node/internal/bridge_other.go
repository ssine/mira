//go:build !android

package node

import (
	"context"
	"encoding/json"
	"fmt"
)

type androidBridge struct{}

func newAndroidBridge(string, string) *androidBridge {
	return nil
}

func (bridge *androidBridge) deliverCompletion(context.Context, json.RawMessage, string) (any, error) {
	return nil, fmt.Errorf("native notifications require Android")
}

//go:build !linux

package node

import (
	"context"
	"fmt"
	"os"
)

func lockBuiltinStart(context.Context, string) (func(), error) {
	return nil, fmt.Errorf("builtin services require Linux")
}
func detachSupervisor(string, []string) error { return fmt.Errorf("builtin services require Linux") }
func acquireBuiltinOwner(string) (*os.File, error) {
	return nil, fmt.Errorf("builtin services require Linux")
}
func replaceSupervisor(string, []string, *os.File) error {
	return fmt.Errorf("Supervisor replacement requires Linux")
}

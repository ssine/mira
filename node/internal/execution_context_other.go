//go:build !windows

package node

import (
	"fmt"
	"os"
	"os/exec"
)

func acquireExecutionIdentity(request executionRequest) (*executionIdentity, error) {
	if err := validateExecutionRequest(request); err != nil {
		return nil, err
	}
	if request.Context != "" {
		return nil, fmt.Errorf("executionContext is currently supported only by Windows Nodes")
	}
	return &executionIdentity{Context: "node", OSIdentity: fmt.Sprintf("uid:%d", os.Geteuid())}, nil
}

func (identity *executionIdentity) close() {}

func (identity *executionIdentity) runImpersonated(operation func() (any, error)) (any, error) {
	return operation()
}

func newExecutionCommand(identity *executionIdentity, name string, args []string, cwd string, overrides map[string]string) (*exec.Cmd, error) {
	command := backgroundCommand(exec.Command(name, args...))
	command.Dir = cwd
	command.Env = mergeExecutionEnvironment(os.Environ(), overrides, false)
	return command, nil
}

func executionContextStatus() map[string]any {
	identity := fmt.Sprintf("uid:%d", os.Geteuid())
	return map[string]any{
		"default":  "node",
		"contexts": []map[string]any{{"name": "node", "available": true, "osIdentity": identity}},
	}
}

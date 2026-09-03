package node

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

type capabilityRuntime struct {
	configuration config
	roots         []string
	realRoots     []string
	bridge        *androidBridge
	processMu     sync.Mutex
	processes     map[string]*managedProcess
	ptyMu         sync.Mutex
	ptys          map[string]*managedPTY
	resourceMu    sync.Mutex
	lastCPU       cpuSample
	hasLastCPU    bool
}

type boundedCommandBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func (buffer *boundedCommandBuffer) Write(value []byte) (int, error) {
	original := len(value)
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	remaining := buffer.limit - len(buffer.data)
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		buffer.data = append(buffer.data, value...)
	}
	if original > remaining {
		buffer.truncated = true
	}
	return original, nil
}

func (buffer *boundedCommandBuffer) result() (string, bool) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return strings.ReplaceAll(string(buffer.data), "\r\n", "\n"), buffer.truncated
}

func newCapabilityRuntime(configuration config) (*capabilityRuntime, error) {
	value := &capabilityRuntime{
		configuration: configuration,
		roots:         make([]string, 0, len(configuration.AllowedRoots)),
		realRoots:     make([]string, 0, len(configuration.AllowedRoots)),
		processes:     make(map[string]*managedProcess),
		ptys:          make(map[string]*managedPTY),
	}
	if configuration.BridgeURL != "" {
		value.bridge = newAndroidBridge(configuration.BridgeURL, configuration.BridgeToken)
	}
	for _, root := range configuration.AllowedRoots {
		cleaned, resolved, err := resolveExistingAncestor(root)
		if err != nil {
			return nil, fmt.Errorf("resolve allowed root %s: %w", root, err)
		}
		value.roots = append(value.roots, cleaned)
		value.realRoots = append(value.realRoots, resolved)
	}
	return value, nil
}

func (runtime *capabilityRuntime) execute(
	ctx context.Context,
	capability string,
	params json.RawMessage,
) (any, error) {
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	switch capability {
	case "status":
		return runtime.machineStatus(ctx)
	case "screen":
		var value screenParams
		if err := json.Unmarshal(params, &value); err != nil {
			return nil, fmt.Errorf("decode screen params: %w", err)
		}
		return runtime.screen(ctx, value)
	case "file":
		var value fileParams
		if err := json.Unmarshal(params, &value); err != nil {
			return nil, fmt.Errorf("decode file params: %w", err)
		}
		return runtime.file(value)
	case "process":
		var value processParams
		if err := json.Unmarshal(params, &value); err != nil {
			return nil, fmt.Errorf("decode process params: %w", err)
		}
		return runtime.process(ctx, value)
	case "pty":
		var value ptyParams
		if err := json.Unmarshal(params, &value); err != nil {
			return nil, fmt.Errorf("decode PTY params: %w", err)
		}
		return runtime.pty(value)
	case "codexSessions":
		var value codexSessionsParams
		if err := json.Unmarshal(params, &value); err != nil {
			return nil, fmt.Errorf("decode Codex sessions params: %w", err)
		}
		return runtime.codexSessions(value)
	default:
		return nil, fmt.Errorf("unsupported capability: %s", capability)
	}
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s failed: %s", name, strings.TrimSpace(string(output)))
	}
	return strings.ReplaceAll(string(output), "\r\n", "\n"), nil
}

func commandOutputLimited(ctx context.Context, maximum int, name string, args ...string) (string, bool, error) {
	output := &boundedCommandBuffer{limit: maximum}
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = output
	command.Stderr = output
	err := command.Run()
	value, truncated := output.result()
	if err != nil {
		return "", truncated, fmt.Errorf("%s failed: %s", name, strings.TrimSpace(value))
	}
	return value, truncated, nil
}

func (runtime *capabilityRuntime) close() {
	runtime.processMu.Lock()
	processes := make([]*managedProcess, 0, len(runtime.processes))
	for _, process := range runtime.processes {
		processes = append(processes, process)
	}
	runtime.processMu.Unlock()
	for _, process := range processes {
		process.mu.Lock()
		running := process.running
		process.mu.Unlock()
		if running && process.command.Process != nil {
			_ = terminateProcess(process.command.Process, "SIGTERM")
		}
	}
	runtime.closePTYs()
}

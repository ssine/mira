package node

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"
)

const maxPTYInputBytes = 1024 * 1024

type ptyParams struct {
	Action    string   `json:"action"`
	SessionID string   `json:"sessionId"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	CWD       string   `json:"cwd"`
	Input     string   `json:"input"`
	Cursor    int64    `json:"cursor"`
	Rows      int      `json:"rows"`
	Cols      int      `json:"cols"`
}

type managedPTY struct {
	mu              sync.Mutex
	process         *os.Process
	stdin           io.Writer
	waitProcess     func() (int, string, error)
	terminate       func() error
	resize          func(cols, rows int) error
	name            string
	args            []string
	cwd             string
	backend         string
	resizeSupported bool
	rows            int
	cols            int
	startedAt       time.Time
	running         bool
	exitCode        *int
	signal          string
	output          outputBuffer
}

func boundedInteger(value, fallback, minimum, maximum int) int {
	if value < minimum || value > maximum {
		return fallback
	}
	return value
}

func (runtimeValue *capabilityRuntime) pty(params ptyParams) (any, error) {
	if runtime.GOOS == "android" {
		return nil, fmt.Errorf("PTY capability is not available on Android")
	}
	switch params.Action {
	case "list":
		return runtimeValue.listPTYs(params.Cursor), nil
	case "open":
		return runtimeValue.openPTY(params)
	case "poll", "write", "resize", "close":
		value, err := runtimeValue.managedPTY(params.SessionID)
		if err != nil {
			return nil, err
		}
		switch params.Action {
		case "poll":
			return value.view(params.SessionID, params.Cursor), nil
		case "write":
			if len(params.Input) > maxPTYInputBytes {
				return nil, fmt.Errorf("PTY input exceeds 1 MiB")
			}
			value.mu.Lock()
			stdin := value.stdin
			running := value.running
			value.mu.Unlock()
			if !running || stdin == nil {
				return nil, fmt.Errorf("PTY input is closed")
			}
			count, err := io.WriteString(stdin, params.Input)
			if err != nil {
				return nil, err
			}
			return map[string]any{"sessionId": params.SessionID, "bytesWritten": count}, nil
		case "resize":
			rows := boundedInteger(params.Rows, 0, 1, 500)
			cols := boundedInteger(params.Cols, 0, 1, 1000)
			if rows == 0 || cols == 0 {
				return nil, fmt.Errorf("PTY resize requires rows between 1 and 500 and cols between 1 and 1000")
			}
			value.mu.Lock()
			resize := value.resize
			running := value.running
			value.mu.Unlock()
			if !running {
				return nil, fmt.Errorf("PTY session is not running")
			}
			if resize == nil {
				return nil, fmt.Errorf("PTY backend %s does not support resize", value.backend)
			}
			if err := resize(cols, rows); err != nil {
				return nil, err
			}
			value.mu.Lock()
			value.rows, value.cols = rows, cols
			value.mu.Unlock()
			return value.view(params.SessionID, params.Cursor), nil
		case "close":
			value.mu.Lock()
			terminate := value.terminate
			running := value.running
			value.mu.Unlock()
			if running && terminate != nil {
				if err := terminate(); err != nil {
					return nil, err
				}
			}
			return map[string]any{"sessionId": params.SessionID, "closed": true}, nil
		}
	}
	return nil, fmt.Errorf("unsupported PTY action: %s", params.Action)
}

func (runtimeValue *capabilityRuntime) cleanPTYSlots() error {
	runtimeValue.ptyMu.Lock()
	defer runtimeValue.ptyMu.Unlock()
	if len(runtimeValue.ptys) < maxProcessCount {
		return nil
	}
	for id, value := range runtimeValue.ptys {
		value.mu.Lock()
		running := value.running
		value.mu.Unlock()
		if !running {
			delete(runtimeValue.ptys, id)
		}
		if len(runtimeValue.ptys) < maxProcessCount {
			return nil
		}
	}
	return fmt.Errorf("PTY session limit of %d reached", maxProcessCount)
}

func (runtimeValue *capabilityRuntime) openPTY(params ptyParams) (any, error) {
	commandName := params.Command
	if commandName == "" {
		if runtime.GOOS == "windows" {
			commandName = os.Getenv("COMSPEC")
			if commandName == "" {
				commandName = "cmd.exe"
			}
		} else {
			commandName = os.Getenv("SHELL")
			if commandName == "" {
				commandName = "/bin/sh"
			}
		}
	}
	if err := validateProcessParams(processParams{Command: commandName, Args: params.Args}); err != nil {
		return nil, err
	}
	if err := runtimeValue.cleanPTYSlots(); err != nil {
		return nil, err
	}
	cwd := params.CWD
	if cwd == "" {
		cwd = runtimeValue.roots[0]
	}
	resolvedCWD, err := runtimeValue.authorize(cwd, true)
	if err != nil {
		return nil, err
	}
	rows := boundedInteger(params.Rows, 24, 1, 500)
	cols := boundedInteger(params.Cols, 80, 1, 1000)
	value := &managedPTY{
		name: commandName, args: append([]string(nil), params.Args...), cwd: resolvedCWD,
		rows: rows, cols: cols, startedAt: time.Now().UTC(), running: true,
	}
	handle, err := startPTYProcess(commandName, params.Args, resolvedCWD, rows, cols, &value.output)
	if err != nil {
		return nil, err
	}
	value.process, value.stdin = handle.process, handle.stdin
	value.waitProcess, value.terminate, value.resize = handle.wait, handle.terminate, handle.resize
	value.backend, value.resizeSupported = handle.backend, handle.resize != nil
	id, err := randomID()
	if err != nil {
		_ = handle.terminate()
		return nil, err
	}
	runtimeValue.ptyMu.Lock()
	runtimeValue.ptys[id] = value
	runtimeValue.ptyMu.Unlock()
	go value.wait()
	return value.view(id, 0), nil
}

func (value *managedPTY) wait() {
	exitCode, signalName, err := value.waitProcess()
	value.output.flush()
	value.mu.Lock()
	defer value.mu.Unlock()
	value.running = false
	value.exitCode = &exitCode
	value.signal = signalName
	if err != nil && value.signal == "" {
		value.output.push("error", err.Error())
	}
}

func (value *managedPTY) view(id string, cursor int64) map[string]any {
	value.mu.Lock()
	running, exitCode, signalName := value.running, value.exitCode, value.signal
	pid := 0
	if value.process != nil {
		pid = value.process.Pid
	}
	name, args, cwd := value.name, append([]string(nil), value.args...), value.cwd
	backend, resizeSupported := value.backend, value.resizeSupported
	rows, cols, startedAt := value.rows, value.cols, value.startedAt
	value.mu.Unlock()
	return map[string]any{
		"sessionId": id, "pid": pid, "command": name, "args": args, "cwd": cwd,
		"backend": backend, "rows": rows, "cols": cols, "resizeSupported": resizeSupported,
		"startedAt": startedAt.Format(time.RFC3339Nano), "exitCode": exitCode,
		"signal": signalName, "running": running, "output": value.output.read(cursor),
	}
}

func (runtimeValue *capabilityRuntime) managedPTY(id string) (*managedPTY, error) {
	runtimeValue.ptyMu.Lock()
	defer runtimeValue.ptyMu.Unlock()
	value := runtimeValue.ptys[id]
	if value == nil {
		return nil, fmt.Errorf("PTY session not found")
	}
	return value, nil
}

func (runtimeValue *capabilityRuntime) listPTYs(cursor int64) map[string]any {
	runtimeValue.ptyMu.Lock()
	ids := make([]string, 0, len(runtimeValue.ptys))
	for id := range runtimeValue.ptys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	values := make([]*managedPTY, 0, len(ids))
	for _, id := range ids {
		values = append(values, runtimeValue.ptys[id])
	}
	runtimeValue.ptyMu.Unlock()
	views := make([]map[string]any, 0, len(ids))
	for index, value := range values {
		views = append(views, value.view(ids[index], cursor))
	}
	return map[string]any{"sessions": views}
}

func (runtimeValue *capabilityRuntime) closePTYs() {
	runtimeValue.ptyMu.Lock()
	values := make([]*managedPTY, 0, len(runtimeValue.ptys))
	for _, value := range runtimeValue.ptys {
		values = append(values, value)
	}
	runtimeValue.ptyMu.Unlock()
	for _, value := range values {
		value.mu.Lock()
		running, terminate := value.running, value.terminate
		value.mu.Unlock()
		if running && terminate != nil {
			_ = terminate()
		}
	}
}

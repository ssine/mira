package node

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
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
	mu        sync.Mutex
	command   *exec.Cmd
	stdin     io.WriteCloser
	name      string
	args      []string
	cwd       string
	backend   string
	rows      int
	cols      int
	startedAt time.Time
	running   bool
	exitCode  *int
	signal    string
	output    outputBuffer
}

func boundedInteger(value, fallback, minimum, maximum int) int {
	if value < minimum || value > maximum {
		return fallback
	}
	return value
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func ptyBackendName() string {
	switch runtime.GOOS {
	case "android":
		return "unsupported"
	case "windows":
		return "pipes-fallback"
	default:
		return "util-linux-script"
	}
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
	case "poll", "write", "close":
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
		case "close":
			value.mu.Lock()
			process := value.command.Process
			running := value.running
			value.mu.Unlock()
			if running && process != nil {
				if err := terminateProcess(process, "SIGTERM"); err != nil {
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
		commandName = os.Getenv("SHELL")
		if commandName == "" {
			if runtime.GOOS == "windows" {
				commandName = "cmd.exe"
			} else {
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
	backend := ptyBackendName()
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.Command(commandName, params.Args...)
	} else {
		parts := append([]string{commandName}, params.Args...)
		for index := range parts {
			parts[index] = shellQuote(parts[index])
		}
		command = exec.Command("script", "-qefc", strings.Join(parts, " "), "/dev/null")
	}
	command.Dir = resolvedCWD
	command.Env = append(os.Environ(), "TERM=xterm-256color", "LINES="+strconv.Itoa(rows), "COLUMNS="+strconv.Itoa(cols))
	value := &managedPTY{
		command: command, name: commandName, args: append([]string(nil), params.Args...),
		cwd: resolvedCWD, backend: backend, rows: rows, cols: cols,
		startedAt: time.Now().UTC(), running: true,
	}
	command.Stdout = streamWriter{buffer: &value.output, stream: "stdout"}
	command.Stderr = streamWriter{buffer: &value.output, stream: "stderr"}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	value.stdin = stdin
	if err := command.Start(); err != nil {
		return nil, err
	}
	id, err := randomID()
	if err != nil {
		_ = command.Process.Kill()
		return nil, err
	}
	runtimeValue.ptyMu.Lock()
	runtimeValue.ptys[id] = value
	runtimeValue.ptyMu.Unlock()
	go value.wait()
	return value.view(id, 0), nil
}

func (value *managedPTY) wait() {
	err := value.command.Wait()
	value.mu.Lock()
	defer value.mu.Unlock()
	value.running = false
	exitCode := value.command.ProcessState.ExitCode()
	value.exitCode = &exitCode
	value.signal = processExitSignal(err)
	if err != nil && value.signal == "" {
		value.output.push("error", err.Error())
	}
}

func (value *managedPTY) view(id string, cursor int64) map[string]any {
	value.mu.Lock()
	running, exitCode, signalName := value.running, value.exitCode, value.signal
	pid := 0
	if value.command.Process != nil {
		pid = value.command.Process.Pid
	}
	name, args, cwd := value.name, append([]string(nil), value.args...), value.cwd
	backend, rows, cols, startedAt := value.backend, value.rows, value.cols, value.startedAt
	value.mu.Unlock()
	return map[string]any{
		"sessionId": id, "pid": pid, "command": name, "args": args, "cwd": cwd,
		"backend": backend, "rows": rows, "cols": cols, "resizeSupported": false,
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
		running, process := value.running, value.command.Process
		value.mu.Unlock()
		if running && process != nil {
			_ = terminateProcess(process, "SIGTERM")
		}
	}
}

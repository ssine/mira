package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxProcessCount  = 128
	maxBufferedBytes = 1024 * 1024
)

type processParams struct {
	Action    string            `json:"action"`
	ProcessID string            `json:"processId"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	CWD       string            `json:"cwd"`
	Env       map[string]string `json:"env"`
	Cursor    int64             `json:"cursor"`
	Signal    string            `json:"signal"`
	System    bool              `json:"system"`
}

type outputChunk struct {
	Cursor int64  `json:"cursor"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

type outputBuffer struct {
	mu         sync.Mutex
	chunks     []outputChunk
	nextCursor int64
	size       int
}

type streamWriter struct {
	buffer *outputBuffer
	stream string
}

func (writer streamWriter) Write(value []byte) (int, error) {
	writer.buffer.push(writer.stream, strings.ToValidUTF8(string(value), "�"))
	return len(value), nil
}

func (buffer *outputBuffer) push(stream string, text string) {
	if text == "" {
		return
	}
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	chunk := outputChunk{Cursor: buffer.nextCursor, Stream: stream, Text: text}
	buffer.nextCursor += int64(len(text))
	buffer.size += len(text)
	buffer.chunks = append(buffer.chunks, chunk)
	for buffer.size > maxBufferedBytes && len(buffer.chunks) > 1 {
		buffer.size -= len(buffer.chunks[0].Text)
		buffer.chunks = buffer.chunks[1:]
	}
}

func (buffer *outputBuffer) read(cursor int64) map[string]any {
	if cursor < 0 {
		cursor = 0
	}
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	firstCursor := buffer.nextCursor
	if len(buffer.chunks) > 0 {
		firstCursor = buffer.chunks[0].Cursor
	}
	effective := cursor
	if effective < firstCursor {
		effective = firstCursor
	}
	chunks := make([]outputChunk, 0, len(buffer.chunks))
	for _, chunk := range buffer.chunks {
		end := chunk.Cursor + int64(len(chunk.Text))
		if end <= effective {
			continue
		}
		skip := effective - chunk.Cursor
		if skip < 0 {
			skip = 0
		}
		text := chunk.Text
		if skip > 0 && skip < int64(len(text)) {
			text = text[skip:]
		}
		chunks = append(chunks, outputChunk{
			Cursor: chunk.Cursor + skip,
			Stream: chunk.Stream,
			Text:   text,
		})
	}
	return map[string]any{
		"cursor": buffer.nextCursor, "lostOutput": cursor < firstCursor, "chunks": chunks,
	}
}

type managedProcess struct {
	mu        sync.Mutex
	command   *exec.Cmd
	name      string
	args      []string
	cwd       string
	startedAt time.Time
	running   bool
	exitCode  *int
	signal    string
	output    outputBuffer
}

func randomID() (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func (runtime *capabilityRuntime) process(ctx context.Context, params processParams) (any, error) {
	switch params.Action {
	case "list":
		if params.System {
			output, err := commandOutput(ctx, "ps", "-A", "-o", "PID,PPID,USER,STAT,NAME,ARGS")
			if err != nil {
				return nil, err
			}
			return map[string]any{"format": "android-ps", "output": output}, nil
		}
		return runtime.listManagedProcesses(params.Cursor), nil
	case "start":
		return runtime.startProcess(params)
	case "poll":
		process, err := runtime.managedProcess(params.ProcessID)
		if err != nil {
			return nil, err
		}
		return process.view(params.ProcessID, params.Cursor), nil
	case "signal":
		return runtime.signalProcess(params)
	default:
		return nil, fmt.Errorf("unsupported process action: %s", params.Action)
	}
}

func (runtime *capabilityRuntime) cleanProcessSlots() error {
	runtime.processMu.Lock()
	defer runtime.processMu.Unlock()
	if len(runtime.processes) < maxProcessCount {
		return nil
	}
	for id, process := range runtime.processes {
		process.mu.Lock()
		running := process.running
		process.mu.Unlock()
		if !running {
			delete(runtime.processes, id)
		}
		if len(runtime.processes) < maxProcessCount {
			return nil
		}
	}
	return fmt.Errorf("process session limit of %d reached", maxProcessCount)
}

func validateProcessParams(params processParams) error {
	if params.Command == "" || len(params.Command) > 4096 || strings.ContainsRune(params.Command, 0) {
		return fmt.Errorf("command must contain between 1 and 4096 bytes")
	}
	if len(params.Args) > 128 {
		return fmt.Errorf("args must contain at most 128 strings")
	}
	for _, value := range params.Args {
		if len(value) > 32768 || strings.ContainsRune(value, 0) {
			return fmt.Errorf("process argument is invalid")
		}
	}
	if len(params.Env) > 100 {
		return fmt.Errorf("env must contain at most 100 values")
	}
	for name, value := range params.Env {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, 0) {
			return fmt.Errorf("process environment contains an invalid name or value")
		}
	}
	return nil
}

func (runtime *capabilityRuntime) startProcess(params processParams) (any, error) {
	if err := validateProcessParams(params); err != nil {
		return nil, err
	}
	if err := runtime.cleanProcessSlots(); err != nil {
		return nil, err
	}
	cwd := params.CWD
	if cwd == "" {
		cwd = runtime.roots[0]
	}
	resolvedCWD, err := runtime.authorize(cwd, true)
	if err != nil {
		return nil, err
	}
	command := exec.Command(params.Command, params.Args...)
	command.Dir = resolvedCWD
	command.Env = os.Environ()
	for name, value := range params.Env {
		command.Env = append(command.Env, name+"="+value)
	}
	process := &managedProcess{
		command: command, name: params.Command, args: append([]string(nil), params.Args...),
		cwd: resolvedCWD, startedAt: time.Now().UTC(), running: true,
	}
	command.Stdout = streamWriter{buffer: &process.output, stream: "stdout"}
	command.Stderr = streamWriter{buffer: &process.output, stream: "stderr"}
	if err := command.Start(); err != nil {
		return nil, err
	}
	id, err := randomID()
	if err != nil {
		_ = command.Process.Kill()
		return nil, err
	}
	runtime.processMu.Lock()
	runtime.processes[id] = process
	runtime.processMu.Unlock()
	go process.wait()
	return process.view(id, 0), nil
}

func (process *managedProcess) wait() {
	err := process.command.Wait()
	process.mu.Lock()
	defer process.mu.Unlock()
	process.running = false
	exitCode := process.command.ProcessState.ExitCode()
	process.exitCode = &exitCode
	if exitError, ok := err.(*exec.ExitError); ok {
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			process.signal = status.Signal().String()
		}
	} else if err != nil {
		process.output.push("error", err.Error())
	}
}

func (process *managedProcess) view(id string, cursor int64) map[string]any {
	process.mu.Lock()
	running := process.running
	exitCode := process.exitCode
	signal := process.signal
	pid := 0
	if process.command.Process != nil {
		pid = process.command.Process.Pid
	}
	name := process.name
	args := append([]string(nil), process.args...)
	cwd := process.cwd
	startedAt := process.startedAt
	process.mu.Unlock()
	return map[string]any{
		"processId": id, "pid": pid, "command": name, "args": args, "cwd": cwd,
		"startedAt": startedAt.Format(time.RFC3339Nano), "exitCode": exitCode,
		"signal": signal, "running": running, "output": process.output.read(cursor),
	}
}

func (runtime *capabilityRuntime) managedProcess(id string) (*managedProcess, error) {
	runtime.processMu.Lock()
	defer runtime.processMu.Unlock()
	process := runtime.processes[id]
	if process == nil {
		return nil, fmt.Errorf("managed Android process not found")
	}
	return process, nil
}

func (runtime *capabilityRuntime) listManagedProcesses(cursor int64) map[string]any {
	runtime.processMu.Lock()
	ids := make([]string, 0, len(runtime.processes))
	for id := range runtime.processes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	processes := make([]*managedProcess, 0, len(ids))
	for _, id := range ids {
		processes = append(processes, runtime.processes[id])
	}
	runtime.processMu.Unlock()
	views := make([]map[string]any, 0, len(ids))
	for index, process := range processes {
		views = append(views, process.view(ids[index], cursor))
	}
	return map[string]any{"processes": views}
}

func (runtime *capabilityRuntime) signalProcess(params processParams) (any, error) {
	process, err := runtime.managedProcess(params.ProcessID)
	if err != nil {
		return nil, err
	}
	signalName := params.Signal
	if signalName == "" {
		signalName = "SIGTERM"
	}
	var signal syscall.Signal
	switch signalName {
	case "SIGINT":
		signal = syscall.SIGINT
	case "SIGTERM":
		signal = syscall.SIGTERM
	case "SIGKILL":
		signal = syscall.SIGKILL
	default:
		return nil, fmt.Errorf("invalid signal: %s", signalName)
	}
	process.mu.Lock()
	running := process.running
	pid := process.command.Process.Pid
	process.mu.Unlock()
	if !running {
		return map[string]any{
			"processId": params.ProcessID, "pid": pid, "signal": signalName, "accepted": false,
		}, nil
	}
	if err := process.command.Process.Signal(signal); err != nil {
		return nil, err
	}
	return map[string]any{
		"processId": params.ProcessID, "pid": pid, "signal": signalName, "accepted": true,
	}, nil
}

//go:build windows

package node

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pty "github.com/aymanbagabas/go-pty"
)

func ptyBackendName() string { return "windows-conpty" }

func startPTYProcess(commandName string, args []string, cwd string, rows, cols int, output *outputBuffer) (*ptyProcessHandle, error) {
	commandPath := commandName
	if !filepath.IsAbs(commandPath) {
		if strings.ContainsAny(commandPath, `/\\`) {
			commandPath = filepath.Join(cwd, commandPath)
		} else {
			var err error
			commandPath, err = exec.LookPath(commandName)
			if err != nil {
				return nil, fmt.Errorf("find Windows PTY command: %w", err)
			}
		}
	}
	terminal, err := pty.New()
	if err != nil {
		return nil, fmt.Errorf("create Windows ConPTY: %w", err)
	}
	var closeOnce sync.Once
	var closing atomic.Bool
	var lifecycleMu sync.Mutex
	closeTerminal := func() error {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		var closeErr error
		closeOnce.Do(func() {
			closing.Store(true)
			closeErr = terminal.Close()
		})
		return closeErr
	}
	if err := terminal.Resize(cols, rows); err != nil {
		_ = closeTerminal()
		return nil, fmt.Errorf("set initial Windows ConPTY size: %w", err)
	}
	command := terminal.Command(commandPath, args...)
	command.Dir = cwd
	command.Env = append(os.Environ(), "TERM=xterm-256color", "LINES="+strconv.Itoa(rows), "COLUMNS="+strconv.Itoa(cols))
	if err := command.Start(); err != nil {
		_ = closeTerminal()
		return nil, fmt.Errorf("start command in Windows ConPTY: %w", err)
	}
	drained := make(chan struct{})
	go func() {
		_, copyErr := io.Copy(streamWriter{buffer: output, stream: "stdout"}, terminal)
		if copyErr != nil && !closing.Load() {
			output.push("error", copyErr.Error())
		}
		close(drained)
	}()
	return &ptyProcessHandle{
		process: command.Process,
		stdin:   terminal,
		backend: "windows-conpty",
		wait: func() (int, string, error) {
			err := command.Wait()
			exitCode := -1
			if command.ProcessState != nil {
				exitCode = command.ProcessState.ExitCode()
			}
			_ = closeTerminal()
			select {
			case <-drained:
			case <-time.After(2 * time.Second):
			}
			return exitCode, processExitSignal(err), err
		},
		terminate: closeTerminal,
		resize: func(cols, rows int) error {
			lifecycleMu.Lock()
			defer lifecycleMu.Unlock()
			if closing.Load() {
				return os.ErrClosed
			}
			if err := terminal.Resize(cols, rows); err != nil {
				return fmt.Errorf("resize Windows ConPTY: %w", err)
			}
			return nil
		},
	}, nil
}

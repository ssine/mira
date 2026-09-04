//go:build !windows

package node

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func ptyBackendName() string {
	if runtime.GOOS == "android" {
		return "unsupported"
	}
	return "util-linux-script"
}

func startPTYProcess(commandName string, args []string, cwd string, rows, cols int, output *outputBuffer) (*ptyProcessHandle, error) {
	parts := append([]string{commandName}, args...)
	for index := range parts {
		parts[index] = shellQuote(parts[index])
	}
	command := exec.Command("script", "-qefc", strings.Join(parts, " "), "/dev/null")
	command.Dir = cwd
	command.Env = append(os.Environ(), "TERM=xterm-256color", "LINES="+strconv.Itoa(rows), "COLUMNS="+strconv.Itoa(cols))
	command.Stdout = streamWriter{buffer: output, stream: "stdout"}
	command.Stderr = streamWriter{buffer: output, stream: "stderr"}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	return &ptyProcessHandle{
		process: command.Process,
		stdin:   stdin,
		backend: "util-linux-script",
		wait: func() (int, string, error) {
			err := command.Wait()
			_ = stdin.Close()
			return command.ProcessState.ExitCode(), processExitSignal(err), err
		},
		terminate: func() error { return terminateProcess(command.Process, "SIGTERM") },
	}, nil
}

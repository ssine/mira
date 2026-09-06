package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

type ExecLauncher struct {
	Stdout io.Writer
	Stderr io.Writer
	Env    []string
}

type commandProcess struct {
	command *exec.Cmd
	done    chan error
	once    sync.Once

	// Tests may shorten the post-kill reap allowance. Production uses the
	// bounded default so a broken child cannot hold update/rollback forever.
	reapTimeout time.Duration
}

const defaultProcessReapTimeout = 2 * time.Second

func (launcher ExecLauncher) Start(ctx context.Context, spec ProcessSpec) (Process, error) {
	command := exec.CommandContext(ctx, spec.Executable, spec.Args...)
	command.Stdout, command.Stderr = launcher.Stdout, launcher.Stderr
	if command.Stdout == nil {
		command.Stdout = os.Stdout
	}
	if command.Stderr == nil {
		command.Stderr = os.Stderr
	}
	command.Env = launcher.Env
	if command.Env == nil {
		command.Env = os.Environ()
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &commandProcess{command: command, done: make(chan error, 1)}
	go func() { process.done <- command.Wait(); close(process.done) }()
	return process, nil
}

func (process *commandProcess) Done() <-chan error { return process.done }

func (process *commandProcess) Stop(ctx context.Context) error {
	var signalError error
	process.once.Do(func() {
		if process.command.Process == nil {
			return
		}
		if runtime.GOOS == "windows" {
			// Windows does not deliver os.Interrupt to arbitrary detached child
			// processes. Terminating the worker leaves PostgreSQL to roll back any
			// open transaction while the Supervisor itself stays in control.
			signalError = process.command.Process.Kill()
		} else {
			signalError = process.command.Process.Signal(os.Interrupt)
		}
	})
	select {
	case <-process.done:
		return ignoreFinishedSignal(signalError)
	case <-ctx.Done():
		if process.command.Process != nil {
			_ = process.command.Process.Kill()
		}
		reapTimeout := process.reapTimeout
		if reapTimeout <= 0 {
			reapTimeout = defaultProcessReapTimeout
		}
		reapTimer := time.NewTimer(reapTimeout)
		defer reapTimer.Stop()
		select {
		case <-process.done:
			return errors.Join(ignoreFinishedSignal(signalError), ctx.Err())
		case <-reapTimer.C:
			return errors.Join(ignoreFinishedSignal(signalError), ctx.Err(), fmt.Errorf("process did not exit within %s after kill", reapTimeout))
		}
	}
}

func ignoreFinishedSignal(err error) error {
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

type CandidateValidator struct {
	Timeout       time.Duration
	SelfCheckArgs []string
}

func (validator CandidateValidator) Validate(ctx context.Context, candidate Candidate) error {
	timeout := validator.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	checkContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := exec.CommandContext(checkContext, candidate.Executable, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("candidate --version: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if !strings.Contains(string(output), candidate.Version) {
		return fmt.Errorf("candidate reports a different version: %s", strings.TrimSpace(string(output)))
	}
	if len(validator.SelfCheckArgs) > 0 {
		output, err = exec.CommandContext(checkContext, candidate.Executable, validator.SelfCheckArgs...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("candidate Supervisor self-check: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

type HTTPHealthChecker struct {
	ServerURL         string
	Client            *http.Client
	Timeout           time.Duration
	NodeStabilization time.Duration
}

func (checker HTTPHealthChecker) WaitHealthy(ctx context.Context, role WorkerRole, spec ProcessSpec, process Process) error {
	if role == NodeWorker {
		delay := checker.NodeStabilization
		if delay <= 0 {
			delay = 750 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case err := <-process.Done():
			return fmt.Errorf("node-worker exited during startup: %w", err)
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	endpoint := strings.TrimRight(checker.ServerURL, "/") + "/healthz"
	client := checker.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	timeout := checker.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	healthContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, _ := http.NewRequestWithContext(healthContext, http.MethodGet, endpoint, nil)
		response, err := client.Do(request)
		if err == nil {
			var health struct {
				Version string `json:"version"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&health)
			response.Body.Close()
			if response.StatusCode == 200 && decodeErr == nil && health.Version == spec.Version {
				select {
				case processErr := <-process.Done():
					if processErr == nil {
						return fmt.Errorf("server-worker exited despite a healthy endpoint")
					}
					return fmt.Errorf("server-worker exited despite a healthy endpoint: %w", processErr)
				default:
					return nil
				}
			}
		}
		select {
		case processErr := <-process.Done():
			return fmt.Errorf("server-worker exited before healthy: %w", processErr)
		case <-healthContext.Done():
			return fmt.Errorf("server health check: %w", healthContext.Err())
		case <-ticker.C:
		}
	}
}

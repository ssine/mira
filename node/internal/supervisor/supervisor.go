// Package supervisor keeps Mira's worker processes alive and applies a release
// update without relying on the Mira Server, Mira Node, or an SSH connection.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	ErrAlreadyRunning       = errors.New("a Mira Supervisor already owns this state directory")
	ErrNotRunning           = errors.New("Mira Supervisor is not running")
	ErrServiceOwnerConflict = errors.New("Mira service owner conflict")
)

// RollbackError reports an activation failure after the previous release was
// successfully restored. It intentionally implements the small interface used
// by the local Supervisor API without importing that package.
type RollbackError struct{ Err error }

func (failure RollbackError) Error() string          { return failure.Err.Error() }
func (failure RollbackError) Unwrap() error          { return failure.Err }
func (failure RollbackError) RollbackComplete() bool { return true }

type ServiceOwner string

const (
	ServiceOwnerNix  ServiceOwner = "nix"
	ServiceOwnerMira ServiceOwner = "mira"
)

func (owner ServiceOwner) valid() bool {
	return owner == ServiceOwnerNix || owner == ServiceOwnerMira
}

type WorkerRole string

const (
	NodeWorker   WorkerRole = "node-worker"
	ServerWorker WorkerRole = "server-worker"
)

type WorkerConfig struct {
	Args []string
}

type ProcessSpec struct {
	Role       WorkerRole
	Version    string
	Executable string
	Args       []string
}

// Process represents one child. Stop must attempt graceful termination and
// return only after the process has exited or the supplied context expires.
type Process interface {
	Done() <-chan error
	Stop(context.Context) error
}

type Launcher interface {
	Start(context.Context, ProcessSpec) (Process, error)
}

// Stager writes an immutable release below destination and returns the path of
// its mira executable. Supervisor rejects paths outside destination.
type Stager interface {
	Stage(ctx context.Context, version, destination string) (executable string, err error)
}

type Validator interface {
	Validate(context.Context, Candidate) error
}

type HealthChecker interface {
	WaitHealthy(context.Context, WorkerRole, ProcessSpec, Process) error
}

type Candidate struct {
	Version    string
	Directory  string
	Executable string
}

type HandoffFunc func(context.Context, Candidate) error

type Config struct {
	StateDir     string
	ServiceOwner ServiceOwner

	Node   WorkerConfig
	Server *WorkerConfig

	Launcher      Launcher
	Stager        Stager
	Validator     Validator
	HealthChecker HealthChecker
	Handoff       HandoffFunc

	GracePeriod   time.Duration
	RestartDelay  time.Duration
	UpdateTimeout time.Duration
}

type runningWorker struct {
	spec       ProcessSpec
	process    Process
	generation uint64
}

type workerExit struct {
	role       WorkerRole
	generation uint64
}

type Supervisor struct {
	config Config
	layout Layout

	opMu sync.Mutex
	mu   sync.Mutex

	lock       *instanceLock
	runContext context.Context
	cancel     context.CancelFunc
	workers    map[WorkerRole]runningWorker
	exits      chan workerExit
	nextID     uint64
	running    bool
	stopping   bool
}

func New(configuration Config) (*Supervisor, error) {
	if configuration.Launcher == nil {
		return nil, fmt.Errorf("supervisor requires a process launcher")
	}
	if !configuration.ServiceOwner.valid() {
		return nil, fmt.Errorf("supervisor service owner must be %q or %q", ServiceOwnerNix, ServiceOwnerMira)
	}
	if configuration.Stager == nil {
		return nil, fmt.Errorf("supervisor requires a release stager")
	}
	if configuration.Validator == nil {
		return nil, fmt.Errorf("supervisor requires a candidate validator")
	}
	if configuration.Server != nil && configuration.HealthChecker == nil {
		return nil, fmt.Errorf("server-worker requires a health checker")
	}
	layout, err := NewLayout(configuration.StateDir)
	if err != nil {
		return nil, err
	}
	if configuration.GracePeriod <= 0 {
		configuration.GracePeriod = 10 * time.Second
	}
	if configuration.RestartDelay <= 0 {
		configuration.RestartDelay = time.Second
	}
	if configuration.UpdateTimeout <= 0 {
		configuration.UpdateTimeout = 2 * time.Minute
	}
	return &Supervisor{
		config:  configuration,
		layout:  layout,
		workers: make(map[WorkerRole]runningWorker),
		exits:   make(chan workerExit, 16),
	}, nil
}

func (supervisor *Supervisor) Layout() Layout { return supervisor.layout }

func (supervisor *Supervisor) State() (State, error) { return supervisor.layout.ReadState() }

func (supervisor *Supervisor) IsRunning() bool {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return supervisor.running
}

// Start acquires the per-state-directory lock and starts the current release.
func (supervisor *Supervisor) Start(ctx context.Context) error {
	supervisor.opMu.Lock()
	defer supervisor.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := supervisor.layout.Prepare(); err != nil {
		return err
	}
	supervisor.mu.Lock()
	if supervisor.running {
		supervisor.mu.Unlock()
		return nil
	}
	supervisor.mu.Unlock()

	lock, err := acquireInstanceLock(supervisor.layout.Lock)
	if err != nil {
		return err
	}
	if err := supervisor.layout.EnsureServiceOwner(supervisor.config.ServiceOwner); err != nil {
		lock.Close()
		return err
	}
	version, err := supervisor.layout.CurrentVersion()
	if err != nil {
		lock.Close()
		return fmt.Errorf("read current Mira version: %w", err)
	}
	candidate, err := supervisor.installedCandidate(version)
	if err != nil {
		lock.Close()
		return err
	}
	// Worker lifetime belongs to Supervisor, not to the request or terminal that
	// happened to start it. Stop performs graceful termination before cancelling
	// this internal context.
	runContext, cancel := context.WithCancel(context.Background())
	supervisor.mu.Lock()
	supervisor.lock = lock
	supervisor.runContext = runContext
	supervisor.cancel = cancel
	supervisor.running = true
	supervisor.stopping = false
	supervisor.mu.Unlock()

	started := make([]WorkerRole, 0, 2)
	for _, role := range supervisor.roles() {
		if err := supervisor.startWorker(runContext, ctx, role, candidate); err != nil {
			supervisor.stopRoles(context.Background(), reverseRoles(started))
			supervisor.finishStopped()
			return fmt.Errorf("start %s: %w", role, err)
		}
		started = append(started, role)
	}
	return nil
}

// Run starts the workers, restarts unexpected exits, and gracefully stops when
// ctx is cancelled.
func (supervisor *Supervisor) Run(ctx context.Context) error {
	if err := supervisor.Start(ctx); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			stopContext, cancel := context.WithTimeout(context.Background(), supervisor.config.GracePeriod)
			err := supervisor.Stop(stopContext)
			cancel()
			if err != nil {
				return err
			}
			return ctx.Err()
		case event := <-supervisor.exits:
			if !supervisor.exitIsCurrent(event) {
				continue
			}
			for ctx.Err() == nil {
				if !waitContext(ctx, supervisor.config.RestartDelay) {
					break
				}
				if err := supervisor.Restart(ctx, event.role); err == nil {
					break
				}
			}
		}
	}
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (supervisor *Supervisor) exitIsCurrent(event workerExit) bool {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	worker, found := supervisor.workers[event.role]
	if !found || supervisor.stopping || worker.generation != event.generation {
		return false
	}
	delete(supervisor.workers, event.role)
	return true
}

// Restart gracefully replaces one worker with the release selected by current.
func (supervisor *Supervisor) Restart(ctx context.Context, role WorkerRole) error {
	supervisor.opMu.Lock()
	defer supervisor.opMu.Unlock()
	if !supervisor.roleEnabled(role) {
		return fmt.Errorf("worker role %q is not enabled", role)
	}
	if !supervisor.IsRunning() {
		return ErrNotRunning
	}
	version, err := supervisor.layout.CurrentVersion()
	if err != nil {
		return err
	}
	candidate, err := supervisor.installedCandidate(version)
	if err != nil {
		return err
	}
	if old, found := supervisor.detachWorker(role); found {
		if err := supervisor.stopProcess(ctx, old.process); err != nil {
			supervisor.attachWorker(old)
			return fmt.Errorf("stop %s: %w", role, err)
		}
	}
	return supervisor.startWorker(supervisor.workerContext(), ctx, role, candidate)
}

// ApplyUpdate performs the complete worker update under the old Supervisor.
// No handoff is attempted until staging, validation, worker health checks and
// the atomic current-pointer replacement have all succeeded.
func (supervisor *Supervisor) ApplyUpdate(ctx context.Context, version string) (Candidate, error) {
	maintenance, err := AcquireMaintenanceLock(ctx, supervisor.layout.StateDir)
	if err != nil {
		return Candidate{}, err
	}
	defer maintenance.Close()
	supervisor.opMu.Lock()
	defer supervisor.opMu.Unlock()
	if !supervisor.IsRunning() {
		return Candidate{}, ErrNotRunning
	}
	oldVersion, err := supervisor.layout.CurrentVersion()
	if err != nil {
		return Candidate{}, err
	}
	if oldVersion == version {
		candidate, err := supervisor.installedCandidate(version)
		if err != nil {
			return Candidate{}, err
		}
		if err := supervisor.config.Validator.Validate(ctx, candidate); err != nil {
			return Candidate{}, fmt.Errorf("validate Mira %s: %w", version, err)
		}
		// current may already name the requested release because a previous owner
		// died after committing the pointer but before recording install state or
		// rewiring the Windows service. Finalization is deliberately idempotent.
		if supervisor.config.Handoff != nil {
			finalizeContext, cancelFinalize := context.WithTimeout(context.WithoutCancel(ctx), supervisor.config.UpdateTimeout)
			defer cancelFinalize()
			if err := supervisor.config.Handoff(finalizeContext, candidate); err != nil {
				return Candidate{}, fmt.Errorf("finalize current Mira %s: %w", version, err)
			}
		}
		return candidate, nil
	}
	destination, err := supervisor.layout.VersionDir(version)
	if err != nil {
		return Candidate{}, err
	}
	candidate, err := supervisor.prepareCandidate(ctx, version, destination)
	if err != nil {
		return Candidate{}, err
	}
	if err := supervisor.layout.writePointer(candidatePointer, version); err != nil {
		return Candidate{}, err
	}
	defer supervisor.layout.removePointer(candidatePointer)

	oldCandidate, err := supervisor.installedCandidate(oldVersion)
	if err != nil {
		return Candidate{}, err
	}

	// From this point onward an initiating CLI or SSH disconnect must not
	// interrupt the transaction. The still-running old Supervisor owns this
	// bounded context and either completes activation or restores the old workers.
	switchContext, cancelSwitch := context.WithTimeout(context.WithoutCancel(ctx), supervisor.config.UpdateTimeout)
	defer cancelSwitch()
	switched := make([]WorkerRole, 0, 2)
	for _, role := range supervisor.roles() {
		if err := supervisor.switchWorker(switchContext, role, candidate); err != nil {
			rollbackErr := supervisor.rollbackWorkers(append(switched, role), oldCandidate)
			if rollbackErr != nil {
				return Candidate{}, errors.Join(fmt.Errorf("activate Mira %s %s: %w", version, role, err), fmt.Errorf("restore Mira %s: %w", oldVersion, rollbackErr))
			}
			return Candidate{}, RollbackError{Err: fmt.Errorf("activate Mira %s %s: %w", version, role, err)}
		}
		switched = append(switched, role)
	}
	if err := supervisor.layout.writePointer(previousPointer, oldVersion); err != nil {
		rollbackErr := supervisor.rollbackWorkers(switched, oldCandidate)
		failure := errors.Join(err, rollbackErr)
		if rollbackErr == nil {
			return Candidate{}, RollbackError{Err: failure}
		}
		return Candidate{}, failure
	}
	if err := supervisor.layout.writePointer(currentPointer, version); err != nil {
		rollbackErr := supervisor.rollbackWorkers(switched, oldCandidate)
		failure := errors.Join(err, rollbackErr)
		if rollbackErr == nil {
			return Candidate{}, RollbackError{Err: failure}
		}
		return Candidate{}, failure
	}
	if supervisor.config.Handoff != nil {
		if err := supervisor.config.Handoff(switchContext, candidate); err != nil {
			pointerErr := supervisor.layout.writePointer(currentPointer, oldVersion)
			rollbackErr := supervisor.rollbackWorkers(switched, oldCandidate)
			failure := errors.Join(fmt.Errorf("request Supervisor handoff: %w", err), pointerErr, rollbackErr)
			if pointerErr == nil && rollbackErr == nil {
				return Candidate{}, RollbackError{Err: failure}
			}
			return Candidate{}, failure
		}
	}
	return candidate, nil
}

// prepareCandidate never exposes a partially extracted release under its final
// version name. A failed download/extract is removed, so retrying the same
// release remains possible without manual cleanup.
func (supervisor *Supervisor) prepareCandidate(ctx context.Context, version, destination string) (Candidate, error) {
	if info, err := os.Stat(destination); err == nil {
		if !info.IsDir() {
			return Candidate{}, fmt.Errorf("Mira version %s path is not a directory", version)
		}
		candidate, err := supervisor.installedCandidate(version)
		if err != nil {
			return Candidate{}, fmt.Errorf("existing Mira %s release is incomplete: %w", version, err)
		}
		if err := supervisor.config.Validator.Validate(ctx, candidate); err != nil {
			return Candidate{}, fmt.Errorf("validate Mira %s: %w", version, err)
		}
		return candidate, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Candidate{}, err
	}
	if err := supervisor.layout.Prepare(); err != nil {
		return Candidate{}, err
	}
	staging, err := os.MkdirTemp(supervisor.layout.VersionsDir, ".staging-"+version+"-")
	if err != nil {
		return Candidate{}, fmt.Errorf("create candidate staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	executable, err := supervisor.config.Stager.Stage(ctx, version, staging)
	if err != nil {
		return Candidate{}, fmt.Errorf("stage Mira %s: %w", version, err)
	}
	executable = filepath.Clean(executable)
	if !filepath.IsAbs(executable) || !pathInside(staging, executable) {
		return Candidate{}, fmt.Errorf("stager returned an executable outside the candidate directory")
	}
	if info, err := os.Stat(executable); err != nil {
		return Candidate{}, fmt.Errorf("stat candidate executable: %w", err)
	} else if info.IsDir() {
		return Candidate{}, fmt.Errorf("candidate executable is a directory")
	}
	staged, err := supervisor.safeCandidate(version, staging, executable)
	if err != nil {
		return Candidate{}, err
	}
	if err := supervisor.config.Validator.Validate(ctx, staged); err != nil {
		return Candidate{}, fmt.Errorf("validate Mira %s: %w", version, err)
	}
	// EvalSymlinks may canonicalize a Windows runner path (for example drive or
	// directory casing). Compute the relocation from the equally canonicalized
	// candidate directory instead of mixing it with the original temp path.
	relative, err := filepath.Rel(staged.Directory, staged.Executable)
	if err != nil || relative == "." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Candidate{}, fmt.Errorf("candidate executable cannot be relocated safely")
	}
	if err := os.Rename(staging, destination); err != nil {
		return Candidate{}, fmt.Errorf("publish staged Mira %s: %w", version, err)
	}
	return supervisor.safeCandidate(version, destination, filepath.Join(destination, relative))
}

func (supervisor *Supervisor) switchWorker(ctx context.Context, role WorkerRole, candidate Candidate) error {
	old, found := supervisor.detachWorker(role)
	if !found {
		return fmt.Errorf("%s is not running", role)
	}
	if err := supervisor.stopProcess(ctx, old.process); err != nil {
		supervisor.attachWorker(old)
		return fmt.Errorf("stop old worker: %w", err)
	}
	if err := supervisor.startWorker(supervisor.workerContext(), ctx, role, candidate); err != nil {
		return err
	}
	return nil
}

func (supervisor *Supervisor) rollbackWorkers(roles []WorkerRole, old Candidate) error {
	ctx, cancel := context.WithTimeout(context.Background(), supervisor.config.UpdateTimeout)
	defer cancel()
	var result error
	seen := make(map[WorkerRole]bool)
	for index := len(roles) - 1; index >= 0; index-- {
		role := roles[index]
		if seen[role] {
			continue
		}
		seen[role] = true
		if current, found := supervisor.detachWorker(role); found {
			if err := supervisor.stopProcess(ctx, current.process); err != nil {
				result = errors.Join(result, fmt.Errorf("stop candidate %s: %w", role, err))
				supervisor.attachWorker(current)
				continue
			}
		}
		if err := supervisor.startWorker(supervisor.workerContext(), ctx, role, old); err != nil {
			result = errors.Join(result, fmt.Errorf("restart previous %s: %w", role, err))
		}
	}
	return result
}

func (supervisor *Supervisor) installedCandidate(version string) (Candidate, error) {
	directory, err := supervisor.layout.VersionDir(version)
	if err != nil {
		return Candidate{}, err
	}
	name := "mira"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(directory, name)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return supervisor.safeCandidate(version, directory, path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Candidate{}, fmt.Errorf("inspect Mira version %s: %w", version, err)
	}
	return Candidate{}, fmt.Errorf("Mira version %s has no canonical %s executable", version, name)
}

func (supervisor *Supervisor) safeCandidate(version, directory, executable string) (Candidate, error) {
	resolvedVersions, err := filepath.EvalSymlinks(supervisor.layout.VersionsDir)
	if err != nil {
		return Candidate{}, err
	}
	resolvedDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return Candidate{}, err
	}
	if !pathInside(resolvedVersions, resolvedDirectory) {
		return Candidate{}, fmt.Errorf("Mira version %s directory resolves outside the versions directory", version)
	}
	resolvedExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return Candidate{}, err
	}
	if !pathInside(resolvedDirectory, resolvedExecutable) {
		return Candidate{}, fmt.Errorf("Mira version %s executable resolves outside its version directory", version)
	}
	relative, err := filepath.Rel(filepath.Clean(directory), filepath.Clean(executable))
	if err != nil || relative == "." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Candidate{}, fmt.Errorf("Mira version %s executable is outside its version directory", version)
	}
	return Candidate{Version: version, Directory: resolvedDirectory, Executable: filepath.Join(resolvedDirectory, relative)}, nil
}

func (supervisor *Supervisor) roles() []WorkerRole {
	roles := []WorkerRole{NodeWorker}
	if supervisor.config.Server != nil {
		roles = append(roles, ServerWorker)
	}
	return roles
}

func reverseRoles(roles []WorkerRole) []WorkerRole {
	result := make([]WorkerRole, len(roles))
	for index := range roles {
		result[len(roles)-1-index] = roles[index]
	}
	return result
}

func (supervisor *Supervisor) roleEnabled(role WorkerRole) bool {
	return role == NodeWorker || (role == ServerWorker && supervisor.config.Server != nil)
}

func (supervisor *Supervisor) workerSpec(role WorkerRole, candidate Candidate) ProcessSpec {
	configuration := supervisor.config.Node
	if role == ServerWorker {
		configuration = *supervisor.config.Server
	}
	arguments := append([]string(nil), configuration.Args...)
	return ProcessSpec{Role: role, Version: candidate.Version, Executable: candidate.Executable, Args: arguments}
}

func (supervisor *Supervisor) startWorker(processContext, healthContext context.Context, role WorkerRole, candidate Candidate) error {
	spec := supervisor.workerSpec(role, candidate)
	process, err := supervisor.config.Launcher.Start(processContext, spec)
	if err != nil {
		return err
	}
	if supervisor.config.HealthChecker != nil {
		if err := supervisor.config.HealthChecker.WaitHealthy(healthContext, role, spec, process); err != nil {
			stopContext, cancel := context.WithTimeout(context.Background(), supervisor.config.GracePeriod)
			stopErr := process.Stop(stopContext)
			cancel()
			return errors.Join(err, stopErr)
		}
	}
	supervisor.mu.Lock()
	supervisor.nextID++
	worker := runningWorker{spec: spec, process: process, generation: supervisor.nextID}
	supervisor.workers[role] = worker
	supervisor.mu.Unlock()
	go func() {
		<-process.Done()
		supervisor.exits <- workerExit{role: role, generation: worker.generation}
	}()
	return nil
}

func (supervisor *Supervisor) detachWorker(role WorkerRole) (runningWorker, bool) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	worker, found := supervisor.workers[role]
	if found {
		delete(supervisor.workers, role)
	}
	return worker, found
}

func (supervisor *Supervisor) attachWorker(worker runningWorker) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	supervisor.workers[worker.spec.Role] = worker
}

func (supervisor *Supervisor) workerContext() context.Context {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return supervisor.runContext
}

func (supervisor *Supervisor) stopProcess(ctx context.Context, process Process) error {
	stopContext, cancel := context.WithTimeout(ctx, supervisor.config.GracePeriod)
	defer cancel()
	return process.Stop(stopContext)
}

func (supervisor *Supervisor) stopRoles(ctx context.Context, roles []WorkerRole) error {
	var result error
	for _, role := range roles {
		worker, found := supervisor.detachWorker(role)
		if !found {
			continue
		}
		if err := supervisor.stopProcess(ctx, worker.process); err != nil {
			result = errors.Join(result, fmt.Errorf("stop %s: %w", role, err))
		}
	}
	return result
}

// Stop gracefully terminates server-worker before node-worker, then releases
// the single-instance lock.
func (supervisor *Supervisor) Stop(ctx context.Context) error {
	supervisor.opMu.Lock()
	defer supervisor.opMu.Unlock()
	supervisor.mu.Lock()
	if !supervisor.running {
		supervisor.mu.Unlock()
		return nil
	}
	supervisor.stopping = true
	cancel := supervisor.cancel
	supervisor.mu.Unlock()
	err := supervisor.stopRoles(ctx, reverseRoles(supervisor.roles()))
	if cancel != nil {
		cancel()
	}
	lockErr := supervisor.finishStopped()
	return errors.Join(err, lockErr)
}

func (supervisor *Supervisor) finishStopped() error {
	supervisor.mu.Lock()
	lock := supervisor.lock
	cancel := supervisor.cancel
	supervisor.lock = nil
	supervisor.runContext = nil
	supervisor.cancel = nil
	supervisor.running = false
	supervisor.stopping = false
	supervisor.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if lock != nil {
		return lock.Close()
	}
	return nil
}

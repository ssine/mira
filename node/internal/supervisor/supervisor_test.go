package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (log *eventLog) add(format string, arguments ...any) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.events = append(log.events, fmt.Sprintf(format, arguments...))
}

func (log *eventLog) snapshot() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.events...)
}

type fakeProcess struct {
	spec ProcessSpec
	log  *eventLog
	done chan error
	once sync.Once
}

func (process *fakeProcess) Done() <-chan error { return process.done }

func (process *fakeProcess) Stop(context.Context) error {
	process.once.Do(func() {
		process.log.add("stop:%s:%s", process.spec.Role, process.spec.Version)
		close(process.done)
	})
	return nil
}

type fakeLauncher struct {
	log *eventLog
}

func (launcher *fakeLauncher) Start(_ context.Context, spec ProcessSpec) (Process, error) {
	launcher.log.add("start:%s:%s:%s", spec.Role, spec.Version, strings.Join(spec.Args, " "))
	return &fakeProcess{spec: spec, log: launcher.log, done: make(chan error)}, nil
}

type fakeStager struct {
	log *eventLog
}

func (stager *fakeStager) Stage(_ context.Context, version, destination string) (string, error) {
	stager.log.add("stage:%s", version)
	executable := filepath.Join(destination, "mira")
	if err := os.WriteFile(executable, []byte("candidate "+version), 0700); err != nil {
		return "", err
	}
	return executable, nil
}

type retryStager struct{ calls int }

func (stager *retryStager) Stage(_ context.Context, version, destination string) (string, error) {
	stager.calls++
	if err := os.WriteFile(filepath.Join(destination, "partial"), []byte("partial"), 0600); err != nil {
		return "", err
	}
	if stager.calls == 1 {
		return "", errors.New("interrupted download")
	}
	if err := os.Remove(filepath.Join(destination, "partial")); err != nil {
		return "", err
	}
	executable := filepath.Join(destination, "mira")
	return executable, os.WriteFile(executable, []byte("candidate "+version), 0700)
}

type fakeValidator struct {
	log *eventLog
}

func (validator *fakeValidator) Validate(_ context.Context, candidate Candidate) error {
	validator.log.add("validate:%s", candidate.Version)
	return nil
}

type fakeHealth struct {
	log         *eventLog
	failRole    WorkerRole
	failVersion string
	onFailure   func()
}

func (health *fakeHealth) WaitHealthy(_ context.Context, role WorkerRole, spec ProcessSpec, _ Process) error {
	health.log.add("health:%s:%s", role, spec.Version)
	if role == health.failRole && spec.Version == health.failVersion {
		if health.onFailure != nil {
			health.onFailure()
		}
		return errors.New("unhealthy candidate")
	}
	return nil
}

func bootstrapVersion(t *testing.T, layout Layout, version string) {
	t.Helper()
	directory, err := layout.VersionDir(version)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "mira"), []byte("release "+version), 0700); err != nil {
		t.Fatal(err)
	}
	if err := layout.InitializeCurrent(version); err != nil {
		t.Fatal(err)
	}
}

func testConfig(stateDir string, log *eventLog, health HealthChecker) Config {
	return Config{
		StateDir:      stateDir,
		ServiceOwner:  ServiceOwnerMira,
		Node:          WorkerConfig{Args: []string{"node-worker"}},
		Server:        &WorkerConfig{Args: []string{"server-worker", "--listen", "127.0.0.1:8787"}},
		Launcher:      &fakeLauncher{log: log},
		Stager:        &fakeStager{log: log},
		Validator:     &fakeValidator{log: log},
		HealthChecker: health,
		GracePeriod:   time.Second,
		UpdateTimeout: time.Second,
	}
}

func assertOrder(t *testing.T, events []string, ordered ...string) {
	t.Helper()
	previous := -1
	for _, wanted := range ordered {
		position := -1
		for index := previous + 1; index < len(events); index++ {
			if events[index] == wanted {
				position = index
				break
			}
		}
		if position <= previous {
			t.Fatalf("event %q is not in order %v; events: %v", wanted, ordered, events)
		}
		previous = position
	}
}

func workerVersions(supervisor *Supervisor) map[WorkerRole]string {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	result := make(map[WorkerRole]string, len(supervisor.workers))
	for role, worker := range supervisor.workers {
		result[role] = worker.spec.Version
	}
	return result
}

func TestApplyUpdateSwitchesServerThenCommitsAndOffersHandoff(t *testing.T) {
	stateDir := t.TempDir()
	layout, err := NewLayout(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapVersion(t, layout, "1.0.0")
	log := &eventLog{}
	health := &fakeHealth{log: log}
	configuration := testConfig(stateDir, log, health)
	var supervisor *Supervisor
	handoffCalled := false
	configuration.Handoff = func(_ context.Context, candidate Candidate) error {
		handoffCalled = true
		log.add("handoff:%s", candidate.Version)
		if !supervisor.IsRunning() {
			t.Error("old Supervisor exited before handoff")
		}
		current, err := layout.CurrentVersion()
		if err != nil || current != candidate.Version {
			t.Errorf("handoff preceded current commit: current=%q err=%v", current, err)
		}
		return nil
	}
	supervisor, err = New(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Stop(context.Background()) })

	if _, err := supervisor.ApplyUpdate(context.Background(), "2.0.0"); err != nil {
		t.Fatal(err)
	}
	if !handoffCalled || !supervisor.IsRunning() {
		t.Fatalf("handoff=%v running=%v", handoffCalled, supervisor.IsRunning())
	}
	current, err := layout.CurrentVersion()
	if err != nil || current != "2.0.0" {
		t.Fatalf("current=%q err=%v", current, err)
	}
	previous, err := layout.PreviousVersion()
	if err != nil || previous != "1.0.0" {
		t.Fatalf("previous=%q err=%v", previous, err)
	}
	if _, err := layout.CandidateVersion(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate pointer remains: %v", err)
	}
	state, err := supervisor.State()
	if err != nil || state.ServiceOwner != ServiceOwnerMira || state.Current != "2.0.0" || state.Previous != "1.0.0" || state.Candidate != "" {
		t.Fatalf("persisted state=%+v err=%v", state, err)
	}
	if actual := workerVersions(supervisor); !reflect.DeepEqual(actual, map[WorkerRole]string{NodeWorker: "2.0.0", ServerWorker: "2.0.0"}) {
		t.Fatalf("worker versions: %v", actual)
	}
	events := log.snapshot()
	assertOrder(t, events,
		"stage:2.0.0",
		"validate:2.0.0",
		"stop:node-worker:1.0.0",
		"start:node-worker:2.0.0:node-worker",
		"stop:server-worker:1.0.0",
		"start:server-worker:2.0.0:server-worker --listen 127.0.0.1:8787",
		"health:server-worker:2.0.0",
		"handoff:2.0.0",
	)
}

func TestApplyUpdateFinalizesCurrentVersionAfterInterruptedHandoff(t *testing.T) {
	stateDir := t.TempDir()
	layout, err := NewLayout(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapVersion(t, layout, "2.0.0")
	log := &eventLog{}
	configuration := testConfig(stateDir, log, &fakeHealth{log: log})
	handoffCalled := false
	configuration.Handoff = func(_ context.Context, candidate Candidate) error {
		handoffCalled = true
		if candidate.Version != "2.0.0" {
			t.Fatalf("finalized candidate = %+v", candidate)
		}
		return nil
	}
	manager, err := New(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })

	if _, err := manager.ApplyUpdate(context.Background(), "2.0.0"); err != nil {
		t.Fatal(err)
	}
	if !handoffCalled {
		t.Fatal("same-version update skipped idempotent handoff finalization")
	}
}

func TestApplyUpdateRemovesPartialStageAndCanRetrySameVersion(t *testing.T) {
	stateDir := t.TempDir()
	layout, err := NewLayout(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapVersion(t, layout, "1.0.0")
	log := &eventLog{}
	configuration := testConfig(stateDir, log, &fakeHealth{log: log})
	stager := &retryStager{}
	configuration.Stager = stager
	supervisor, err := New(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Stop(context.Background()) })
	if _, err := supervisor.ApplyUpdate(context.Background(), "2.0.0"); err == nil {
		t.Fatal("interrupted stage unexpectedly succeeded")
	}
	destination, _ := layout.VersionDir("2.0.0")
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial final release remains: %v", err)
	}
	entries, err := os.ReadDir(layout.VersionsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".staging-2.0.0-") {
			t.Fatalf("partial staging directory remains: %s", entry.Name())
		}
	}
	if _, err := supervisor.ApplyUpdate(context.Background(), "2.0.0"); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if stager.calls != 2 {
		t.Fatalf("stage calls=%d, want 2", stager.calls)
	}
}

func TestRestartGracefullyReplacesOneWorker(t *testing.T) {
	stateDir := t.TempDir()
	layout, err := NewLayout(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapVersion(t, layout, "1.0.0")
	log := &eventLog{}
	health := &fakeHealth{log: log}
	supervisor, err := New(testConfig(stateDir, log, health))
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Stop(context.Background()) })

	if err := supervisor.Restart(context.Background(), NodeWorker); err != nil {
		t.Fatal(err)
	}
	events := log.snapshot()
	assertOrder(t, events,
		"stop:node-worker:1.0.0",
		"start:node-worker:1.0.0:node-worker",
		"health:node-worker:1.0.0",
	)
	if versions := workerVersions(supervisor); versions[ServerWorker] != "1.0.0" {
		t.Fatalf("restarting Node disturbed Server: %v", versions)
	}
}

func TestEnsureWindowsReleaseAliasesCreatesServiceAndCLIRoles(t *testing.T) {
	directory := t.TempDir()
	image := filepath.Join(directory, "mira-node.exe")
	if err := os.WriteFile(image, []byte("single-image"), 0755); err != nil {
		t.Fatal(err)
	}
	executable, err := ensureWindowsReleaseAliases(directory)
	if err != nil {
		t.Fatal(err)
	}
	if executable != filepath.Join(directory, "mira.exe") {
		t.Fatalf("service executable = %q", executable)
	}
	imageInfo, err := os.Stat(image)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mira.exe", "ssh.exe", "sftp-server.exe", "ssh-pkcs11-helper.exe"} {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil || !os.SameFile(imageInfo, info) {
			t.Fatalf("role %s is not the release image: %v", name, err)
		}
	}
}

func TestEnsureWindowsReleaseAliasesAcceptsCanonicalImage(t *testing.T) {
	directory := t.TempDir()
	image := filepath.Join(directory, "mira.exe")
	if err := os.WriteFile(image, []byte("single-image"), 0755); err != nil {
		t.Fatal(err)
	}
	executable, err := ensureWindowsReleaseAliases(directory)
	if err != nil {
		t.Fatal(err)
	}
	if executable != image {
		t.Fatalf("service executable = %q", executable)
	}
	if _, err := os.Stat(filepath.Join(directory, "mira-node.exe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical release created a legacy Mira role: %v", err)
	}
	imageInfo, err := os.Stat(image)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ssh.exe", "sftp-server.exe", "ssh-pkcs11-helper.exe"} {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil || !os.SameFile(imageInfo, info) {
			t.Fatalf("role %s is not the release image: %v", name, err)
		}
	}
}

func TestInstalledCandidateRejectsLegacyOnlyMiraRole(t *testing.T) {
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Prepare(); err != nil {
		t.Fatal(err)
	}
	directory, err := layout.VersionDir("2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	name := "mira-node"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte("legacy"), 0700); err != nil {
		t.Fatal(err)
	}
	supervisor := &Supervisor{layout: layout}
	if _, err := supervisor.installedCandidate("2.0.0"); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("legacy-only candidate error = %v", err)
	}
}

func TestServerHealthDoesNotAcceptAnotherHealthyProcess(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(map[string]any{"version": "1.0.0"})
	}))
	defer health.Close()
	done := make(chan error, 1)
	done <- errors.New("candidate failed to bind")
	close(done)
	process := &fakeProcess{done: done, log: &eventLog{}}
	checker := HTTPHealthChecker{ServerURL: health.URL, Timeout: time.Second}
	err := checker.WaitHealthy(context.Background(), ServerWorker, ProcessSpec{Version: "2.0.0"}, process)
	if err == nil || !strings.Contains(err.Error(), "candidate failed to bind") {
		t.Fatalf("unexpected health result: %v", err)
	}
}

func TestServerHealthRequiresCandidateVersion(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(response).Encode(map[string]any{"version": "2.0.0"})
	}))
	defer health.Close()
	process := &fakeProcess{done: make(chan error), log: &eventLog{}}
	checker := HTTPHealthChecker{ServerURL: health.URL, Timeout: time.Second}
	if err := checker.WaitHealthy(context.Background(), ServerWorker, ProcessSpec{Version: "2.0.0"}, process); err != nil {
		t.Fatalf("candidate version was not accepted: %v", err)
	}
}

func TestCommandProcessStopBoundsWaitAfterKill(t *testing.T) {
	process := &commandProcess{
		command:     &exec.Cmd{},
		done:        make(chan error),
		reapTimeout: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := process.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("unexpected bounded stop error: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("Stop waited beyond hard reap bound: %s", elapsed)
	}
}

func TestApplyUpdateRestoresOldWorkersWhenServerHealthFailsAfterRequestDisconnect(t *testing.T) {
	stateDir := t.TempDir()
	layout, err := NewLayout(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapVersion(t, layout, "1.0.0")
	log := &eventLog{}
	requestContext, cancelRequest := context.WithCancel(context.Background())
	health := &fakeHealth{log: log, failRole: ServerWorker, failVersion: "2.0.0", onFailure: cancelRequest}
	configuration := testConfig(stateDir, log, health)
	handoffCalled := false
	configuration.Handoff = func(context.Context, Candidate) error {
		handoffCalled = true
		return nil
	}
	supervisor, err := New(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Stop(context.Background()) })

	if _, err := supervisor.ApplyUpdate(requestContext, "2.0.0"); err == nil || !strings.Contains(err.Error(), "unhealthy candidate") {
		t.Fatalf("unexpected update error: %v", err)
	}
	if requestContext.Err() == nil {
		t.Fatal("test did not disconnect the initiating request")
	}
	if handoffCalled || !supervisor.IsRunning() {
		t.Fatalf("handoff=%v running=%v", handoffCalled, supervisor.IsRunning())
	}
	current, err := layout.CurrentVersion()
	if err != nil || current != "1.0.0" {
		t.Fatalf("current=%q err=%v", current, err)
	}
	if _, err := layout.PreviousVersion(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous pointer changed on failure: %v", err)
	}
	if _, err := layout.CandidateVersion(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate pointer remains: %v", err)
	}
	if actual := workerVersions(supervisor); !reflect.DeepEqual(actual, map[WorkerRole]string{NodeWorker: "1.0.0", ServerWorker: "1.0.0"}) {
		t.Fatalf("worker versions after rollback: %v", actual)
	}
	events := log.snapshot()
	assertOrder(t, events,
		"stop:server-worker:1.0.0",
		"start:server-worker:2.0.0:server-worker --listen 127.0.0.1:8787",
		"health:server-worker:2.0.0",
		"stop:server-worker:2.0.0",
		"start:server-worker:1.0.0:server-worker --listen 127.0.0.1:8787",
		"health:server-worker:1.0.0",
	)
}

func TestSingleInstanceAndServiceOwnerAreEnforced(t *testing.T) {
	stateDir := t.TempDir()
	layout, err := NewLayout(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapVersion(t, layout, "1.0.0")
	log := &eventLog{}
	health := &fakeHealth{log: log}
	first, err := New(testConfig(stateDir, log, health))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := New(testConfig(stateDir, log, health))
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Start(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Supervisor error: %v", err)
	}
	if err := first.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	nixConfiguration := testConfig(stateDir, log, health)
	nixConfiguration.ServiceOwner = ServiceOwnerNix
	nixSupervisor, err := New(nixConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	if err := nixSupervisor.Start(context.Background()); !errors.Is(err, ErrServiceOwnerConflict) {
		t.Fatalf("conflicting service owner error: %v", err)
	}
	owner, err := os.ReadFile(layout.ServiceOwnerFile)
	if err != nil || string(owner) != "mira\n" {
		t.Fatalf("persisted service owner=%q err=%v", owner, err)
	}
}

func TestLayoutRejectsFilesystemRoot(t *testing.T) {
	root := string(filepath.Separator)
	if volume := filepath.VolumeName(t.TempDir()); volume != "" {
		root = volume + string(filepath.Separator)
	}
	if _, err := NewLayout(root); err == nil {
		t.Fatal("filesystem root accepted as Supervisor state")
	}
}

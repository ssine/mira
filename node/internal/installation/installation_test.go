package installation

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ssine/mira/node/internal/supervisor"
)

type testFileSystem struct {
	OSFileSystem
	osRelease      []byte
	openWrtRelease bool
	procdHost      bool

	mu      sync.Mutex
	writes  []string
	removes []string
}

type executableTestFileInfo struct {
	name string
}

func (info executableTestFileInfo) Name() string  { return info.name }
func (executableTestFileInfo) Size() int64        { return 0 }
func (executableTestFileInfo) Mode() os.FileMode  { return 0755 }
func (executableTestFileInfo) ModTime() time.Time { return time.Time{} }
func (executableTestFileInfo) IsDir() bool        { return false }
func (executableTestFileInfo) Sys() any           { return nil }

func (files *testFileSystem) ReadFile(path string) ([]byte, error) {
	if path == "/etc/os-release" && files.osRelease != nil {
		return append([]byte(nil), files.osRelease...), nil
	}
	return files.OSFileSystem.ReadFile(path)
}

func (files *testFileSystem) Stat(path string) (os.FileInfo, error) {
	available := false
	switch path {
	case "/etc/openwrt_release":
		available = files.openWrtRelease
	case "/etc/rc.common", "/sbin/procd":
		available = files.procdHost
	default:
		return files.OSFileSystem.Stat(path)
	}
	if !available {
		return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
	}
	return executableTestFileInfo{name: filepath.Base(path)}, nil
}

func (files *testFileSystem) AtomicWrite(path string, content []byte, mode os.FileMode) error {
	files.mu.Lock()
	files.writes = append(files.writes, path)
	files.mu.Unlock()
	return files.OSFileSystem.AtomicWrite(path, content, mode)
}

func (files *testFileSystem) Remove(path string) error {
	files.mu.Lock()
	files.removes = append(files.removes, path)
	files.mu.Unlock()
	return files.OSFileSystem.Remove(path)
}

func (files *testFileSystem) writeCount() int {
	files.mu.Lock()
	defer files.mu.Unlock()
	return len(files.writes)
}

type testRunner struct {
	mu       sync.Mutex
	commands []Command
	output   string
	err      error
	handle   func(Command) (string, error)
}

func (runner *testRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	runner.mu.Lock()
	command := Command{Name: name, Args: append([]string(nil), args...)}
	runner.commands = append(runner.commands, command)
	handle := runner.handle
	runner.mu.Unlock()
	if handle != nil {
		return handle(command)
	}
	return runner.output, runner.err
}

func (runner *testRunner) snapshot() []Command {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return append([]Command(nil), runner.commands...)
}

func TestNixPlanOnlyGeneratesReviewableSnippet(t *testing.T) {
	stateDir := t.TempDir()
	files := &testFileSystem{osRelease: []byte("ID=nixos\n")}
	runner := &testRunner{}
	if _, err := BuildPlan(PlanOptions{StateDir: stateDir, Version: "1.2.3", Platform: "linux"}, files); !errors.Is(err, ErrOwnerChoiceNeeded) {
		t.Fatalf("NixOS did not require an explicit owner choice: %v", err)
	}
	plan, err := BuildPlan(PlanOptions{StateDir: stateDir, Version: "1.2.3", Platform: "linux", ServiceOwner: ServiceOwnerNix, Role: RoleServer}, files)
	if err != nil {
		t.Fatal(err)
	}
	if plan.State.ServiceOwner != ServiceOwnerNix || len(plan.Commands) != 0 || len(plan.Files) != 1 {
		t.Fatalf("unexpected Nix plan: %+v", plan)
	}
	if !strings.Contains(plan.NixSnippet, "${miraStateDir}/current/mira supervisor") ||
		!strings.Contains(plan.NixSnippet, "Mira never runs nixos-rebuild") || !strings.Contains(plan.NixSnippet, "--server") {
		t.Fatalf("Nix snippet does not expose reviewed Supervisor wiring:\n%s", plan.NixSnippet)
	}

	dependencies := Dependencies{Files: files, Runner: runner}
	report, err := Install(context.Background(), plan, dependencies, ApplyOptions{DryRun: true})
	if err != nil || report.Applied || files.writeCount() != 0 || len(runner.snapshot()) != 0 {
		t.Fatalf("dry run had side effects: report=%+v err=%v writes=%d commands=%v", report, err, files.writeCount(), runner.snapshot())
	}
	report, err = Install(context.Background(), plan, dependencies, ApplyOptions{})
	if err != nil || !report.Applied {
		t.Fatalf("install report=%+v err=%v", report, err)
	}
	if commands := runner.snapshot(); len(commands) != 0 {
		t.Fatalf("Nix install invoked system service commands: %v", commands)
	}
	state, err := LoadState(files, stateDir)
	if err != nil || state.ServiceOwner != ServiceOwnerNix {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	uninstall, err := Uninstall(context.Background(), stateDir, ServiceOwnerNix, dependencies, ApplyOptions{})
	if !errors.Is(err, ErrNixManualRemoval) || !uninstall.ManualActionRequired || len(runner.snapshot()) != 0 {
		t.Fatalf("Nix uninstall changed services: report=%+v err=%v commands=%v", uninstall, err, runner.snapshot())
	}
}

func TestNixOSAllowsExplicitMiraOwnership(t *testing.T) {
	stateDir := t.TempDir()
	unitPath := filepath.Join(t.TempDir(), "mira.service")
	files := &testFileSystem{osRelease: []byte("ID=nixos\n")}
	plan, err := BuildPlan(PlanOptions{StateDir: stateDir, Version: "1.2.3", Platform: "linux", ServiceOwner: ServiceOwnerMira, SystemdUnitPath: unitPath}, files)
	if err != nil {
		t.Fatal(err)
	}
	if plan.State.ServiceOwner != ServiceOwnerMira || plan.DetectedNixOS != true {
		t.Fatalf("unexpected explicit Mira-owned plan: %+v", plan)
	}
}

func TestOpenWrtAutoSelectsProcdAndSupportsLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("procd integration uses Unix filesystem semantics")
	}
	root := t.TempDir()
	stateDir := filepath.Join(root, "state user's")
	initPath := filepath.Join(root, "init.d", "mira")
	files := &testFileSystem{
		osRelease: []byte("ID=openwrt\n"), openWrtRelease: true, procdHost: true,
	}
	plan, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira,
		Role: RoleNode, ProcdInitPath: initPath,
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	if plan.State.ServiceManager != ServiceManagerProcd || plan.State.ServiceScope != ScopeSystem || plan.State.ServiceName != "mira" {
		t.Fatalf("unexpected procd plan: %+v", plan.State)
	}
	definition := plan.State.ServiceDefinition
	for _, expected := range []string{
		"#!/bin/sh /etc/rc.common", "USE_PROCD=1", "current/mira' supervisor",
		"--service-owner mira", "procd_set_param respawn 3600 3 0", "procd_set_param term_timeout 30",
		`state user'"'"'s`,
	} {
		if !strings.Contains(definition, expected) {
			t.Errorf("procd definition omitted %q:\n%s", expected, definition)
		}
	}
	wantedInstall := []Command{
		{Name: initPath, Args: []string{"enable"}},
		{Name: initPath, Args: []string{"start"}},
	}
	if !reflect.DeepEqual(plan.Commands, wantedInstall) {
		t.Fatalf("procd install commands=%v want=%v", plan.Commands, wantedInstall)
	}

	runner := &testRunner{}
	dependencies := Dependencies{Files: files, Runner: runner}
	if err := os.MkdirAll(filepath.Join(stateDir, "versions", "1.0.0"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", "1.0.0"), filepath.Join(stateDir, "current")); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), plan, dependencies, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(initPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("procd init mode=%v", info.Mode())
	}
	if report := Doctor(context.Background(), stateDir, dependencies); !report.Healthy {
		t.Fatalf("fresh procd install is unhealthy: %+v", report)
	}

	if err := os.WriteFile(initPath, []byte("drifted\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(initPath, 0644); err != nil {
		t.Fatal(err)
	}
	report := Doctor(context.Background(), stateDir, dependencies)
	codes := map[string]bool{}
	for _, finding := range report.Findings {
		codes[finding.Code] = true
	}
	if !codes["service_definition_drift"] || !codes["service_not_executable"] {
		t.Fatalf("procd doctor missed definition/mode drift: %+v", report)
	}
	baseline := len(runner.snapshot())
	if _, err := Repair(context.Background(), plan, dependencies, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	repairCommands := runner.snapshot()[baseline:]
	wantedRepair := []Command{
		{Name: initPath, Args: []string{"enable"}},
		{Name: initPath, Args: []string{"restart"}},
	}
	if !reflect.DeepEqual(repairCommands, wantedRepair) {
		t.Fatalf("procd repair commands=%v want=%v", repairCommands, wantedRepair)
	}
	if info, err := os.Stat(initPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0755 {
		t.Fatalf("repair did not restore executable mode: %v", info.Mode())
	}

	baseline = len(runner.snapshot())
	if _, err := Uninstall(context.Background(), stateDir, ServiceOwnerMira, dependencies, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	uninstallCommands := runner.snapshot()[baseline:]
	wantedUninstall := []Command{
		{Name: initPath, Args: []string{"disable"}},
		{Name: initPath, Args: []string{"stop"}},
	}
	if !reflect.DeepEqual(uninstallCommands, wantedUninstall) {
		t.Fatalf("procd uninstall commands=%v want=%v", uninstallCommands, wantedUninstall)
	}
	for _, path := range []string{initPath, filepath.Join(stateDir, "install-state.json"), filepath.Join(stateDir, "service-owner")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("uninstall retained %s: %v", path, err)
		}
	}
}

func TestProcdDetectionOverrideAndConstraints(t *testing.T) {
	stateDir := t.TempDir()
	files := &testFileSystem{osRelease: []byte("ID=openwrt\n"), openWrtRelease: true, procdHost: true}

	systemd, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira,
		Role: RoleNode, ServiceManager: ServiceManagerSystemd, SystemdUnitPath: filepath.Join(t.TempDir(), "mira.service"),
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	if systemd.State.ServiceManager != ServiceManagerSystemd || systemd.State.ServiceScope != ScopeUser {
		t.Fatalf("explicit systemd override was not preserved: %+v", systemd.State)
	}

	for name, options := range map[string]PlanOptions{
		"server role": {
			StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira,
			Role: RoleServer, ServiceManager: ServiceManagerProcd,
		},
		"user scope": {
			StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira,
			Role: RoleNode, ServiceManager: ServiceManagerProcd, ServiceScope: ScopeUser,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildPlan(options, files); err == nil {
				t.Fatalf("invalid procd plan unexpectedly succeeded: %+v", options)
			}
		})
	}

	missingProcd := &testFileSystem{osRelease: []byte("ID=openwrt\n"), openWrtRelease: true}
	if _, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira, Role: RoleNode,
	}, missingProcd); err == nil || !strings.Contains(err.Error(), "/etc/rc.common") {
		t.Fatalf("OpenWrt without procd prerequisites was not rejected: %v", err)
	}
}

func TestSchemaOneLinuxStateWithoutManagerInfersSystemd(t *testing.T) {
	stateDir := t.TempDir()
	unitPath := filepath.Join(t.TempDir(), "mira.service")
	files := &testFileSystem{osRelease: []byte("ID=debian\n")}
	plan, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira,
		Role: RoleNode, ServiceManager: ServiceManagerSystemd, SystemdUnitPath: unitPath,
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	content, err := encodeState(plan.State)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.Replace(string(content), "  \"serviceManager\": \"systemd\",\n", "", 1))
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := files.AtomicWrite(filepath.Join(stateDir, "install-state.json"), content, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(files, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ServiceManager != ServiceManagerSystemd {
		t.Fatalf("legacy manager=%q want systemd", loaded.ServiceManager)
	}
}

func TestProcdDoctorReportsDisabledAndInactive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("procd integration uses Unix filesystem semantics")
	}
	stateDir := t.TempDir()
	initPath := filepath.Join(t.TempDir(), "mira")
	files := &testFileSystem{osRelease: []byte("ID=openwrt\n"), procdHost: true}
	plan, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira,
		Role: RoleNode, ServiceManager: ServiceManagerProcd, ProcdInitPath: initPath,
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	runner := &testRunner{}
	dependencies := Dependencies{Files: files, Runner: runner}
	if _, err := Install(context.Background(), plan, dependencies, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	runner.handle = func(command Command) (string, error) {
		if command.Name == initPath && (slices.Contains(command.Args, "enabled") || slices.Contains(command.Args, "running")) {
			return "", errors.New("not active")
		}
		return "", nil
	}
	report := Doctor(context.Background(), stateDir, dependencies)
	codes := map[string]bool{}
	for _, finding := range report.Findings {
		codes[finding.Code] = true
	}
	if !codes["service_autostart_disabled"] || !codes["service_inactive"] {
		t.Fatalf("procd status drift not reported: %+v", report)
	}
}

func TestProcdInstallRefusesSymlinkService(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("procd integration uses Unix filesystem semantics")
	}
	stateDir := t.TempDir()
	serviceDir := t.TempDir()
	initPath := filepath.Join(serviceDir, "mira")
	target := filepath.Join(serviceDir, "other")
	files := &testFileSystem{osRelease: []byte("ID=openwrt\n"), procdHost: true}
	plan, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira,
		Role: RoleNode, ServiceManager: ServiceManagerProcd, ProcdInitPath: initPath,
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(plan.State.ServiceDefinition), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, initPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), plan, Dependencies{Files: files, Runner: &testRunner{}}, ApplyOptions{}); !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("symlink service collision error=%v", err)
	}
	if targetValue, err := os.Readlink(initPath); err != nil || targetValue != target {
		t.Fatalf("symlink service was replaced: target=%q err=%v", targetValue, err)
	}
}

func TestOwnershipConflictStopsInstallRepairAndUninstallBeforeSideEffects(t *testing.T) {
	stateDir := t.TempDir()
	unitPath := filepath.Join(t.TempDir(), "mira.service")
	files := &testFileSystem{osRelease: []byte("ID=debian\n")}
	runner := &testRunner{}
	miraPlan, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira, SystemdUnitPath: unitPath,
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), miraPlan, Dependencies{Files: files, Runner: runner}, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	baselineWrites := files.writeCount()
	baselineCommands := len(runner.snapshot())

	files.osRelease = []byte("ID=nixos\n")
	nixPlan, err := BuildPlan(PlanOptions{StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerNix}, files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), nixPlan, Dependencies{Files: files, Runner: runner}, ApplyOptions{}); !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("conflicting install error: %v", err)
	}
	if _, err := Repair(context.Background(), nixPlan, Dependencies{Files: files, Runner: runner}, ApplyOptions{}); !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("conflicting repair error: %v", err)
	}
	if _, err := Uninstall(context.Background(), stateDir, ServiceOwnerNix, Dependencies{Files: files, Runner: runner}, ApplyOptions{}); !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("conflicting uninstall error: %v", err)
	}
	if files.writeCount() != baselineWrites || len(runner.snapshot()) != baselineCommands {
		t.Fatalf("ownership conflict caused side effects: writes %d->%d commands %d->%d", baselineWrites, files.writeCount(), baselineCommands, len(runner.snapshot()))
	}
}

func TestLinuxPlanInstallsSupervisorUnitAndDoctorReportsDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux systemd integration uses Unix filesystem semantics")
	}
	stateDir := t.TempDir()
	unitPath := filepath.Join(t.TempDir(), "mira.service")
	files := &testFileSystem{osRelease: []byte("ID=debian\n")}
	runner := &testRunner{}
	versionDir := filepath.Join(stateDir, "versions", "1.0.0")
	if err := os.MkdirAll(versionDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, "mira"), []byte("release"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", "1.0.0"), filepath.Join(stateDir, "current")); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira, ServiceScope: ScopeSystem, SystemdUnitPath: unitPath,
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.State.ServiceDefinition, filepath.Join(stateDir, "current", "mira")+`" supervisor`) {
		t.Fatalf("systemd unit does not start current/mira:\n%s", plan.State.ServiceDefinition)
	}
	dependencies := Dependencies{Files: files, Runner: runner}
	if _, err := Install(context.Background(), plan, dependencies, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	wantedCommands := []Command{
		{Name: "systemctl", Args: []string{"show", "mira.service", "--property=FragmentPath", "--value"}},
		{Name: "systemctl", Args: []string{"show", "mira.service", "--property=DropInPaths", "--value"}},
		{Name: "systemctl", Args: []string{"daemon-reload"}},
		{Name: "systemctl", Args: []string{"enable", "--now", "mira.service"}},
	}
	if commands := runner.snapshot(); !reflect.DeepEqual(commands, wantedCommands) {
		t.Fatalf("systemd commands=%v want=%v", commands, wantedCommands)
	}
	if report := Doctor(context.Background(), stateDir, dependencies); !report.Healthy {
		t.Fatalf("fresh install is unhealthy: %+v", report)
	}

	if err := os.WriteFile(unitPath, []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(stateDir, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", "2.0.0"), filepath.Join(stateDir, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "service-owner"), []byte("nix\n"), 0600); err != nil {
		t.Fatal(err)
	}
	report := Doctor(context.Background(), stateDir, dependencies)
	if report.Healthy {
		t.Fatalf("drift was not detected: %+v", report)
	}
	codes := make(map[string]bool)
	for _, finding := range report.Findings {
		codes[finding.Code] = true
	}
	for _, code := range []string{"service_owner_drift", "version_drift", "service_definition_drift"} {
		if !codes[code] {
			t.Errorf("missing %s in %+v", code, report.Findings)
		}
	}
}

func TestWindowsPlanRegistersVersionedSupervisorService(t *testing.T) {
	stateDir := t.TempDir()
	files := &testFileSystem{osRelease: []byte("ID=debian\n")}
	runner := &testRunner{}
	executable := filepath.Join(stateDir, "versions", "1.0.0", "mira.exe")
	plan, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "windows", ServiceOwner: ServiceOwnerMira, WindowsExecutable: executable,
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Commands) != 4 || plan.Commands[0].Name != "sc.exe" || plan.Commands[0].Args[0] != "create" ||
		plan.Commands[2].Args[0] != "failureflag" || !strings.Contains(plan.State.ServiceDefinition, "mira.exe") ||
		!strings.Contains(plan.State.ServiceDefinition, " supervisor ") || !strings.Contains(plan.State.ServiceDefinition, `--windows-service-name "Mira"`) {
		t.Fatalf("unexpected Windows service plan: %+v", plan)
	}
	if _, err := Install(context.Background(), plan, Dependencies{Files: files, Runner: runner}, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if commands := runner.snapshot(); len(commands) != 5 || commands[0].Name != "powershell.exe" || commands[1].Name != "sc.exe" {
		t.Fatalf("Windows service was not registered: %v", commands)
	}
}

func TestWindowsPartialInstallReconfiguresRunningServiceWithoutStartingTwice(t *testing.T) {
	stateDir := t.TempDir()
	files := &testFileSystem{osRelease: []byte("ID=debian\n")}
	plan, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "windows", ServiceOwner: ServiceOwnerMira,
		WindowsExecutable: filepath.Join(stateDir, "versions", "1.0.0", "mira.exe"),
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	runner := &testRunner{handle: func(command Command) (string, error) {
		if command.Name == "powershell.exe" {
			return plan.State.ServiceDefinition, nil
		}
		if command.Name == "sc.exe" && command.Args[0] == "query" {
			return "STATE : 4 RUNNING", nil
		}
		return "", nil
	}}
	if _, err := Install(context.Background(), plan, Dependencies{Files: files, Runner: runner}, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	commands := runner.snapshot()
	for _, command := range commands {
		if command.Name == "sc.exe" && command.Args[0] == "start" {
			t.Fatalf("running partial service was started twice: %v", commands)
		}
	}
	if len(commands) < 3 || commands[2].Name != "sc.exe" || commands[2].Args[0] != "config" {
		t.Fatalf("partial service was not reconfigured: %v", commands)
	}
}

func TestWindowsPartialInstallWaitsForStartPending(t *testing.T) {
	stateDir := t.TempDir()
	files := &testFileSystem{osRelease: []byte("ID=debian\n")}
	plan, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "windows", ServiceOwner: ServiceOwnerMira,
		WindowsExecutable: filepath.Join(stateDir, "versions", "1.0.0", "mira.exe"),
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	queries := 0
	runner := &testRunner{handle: func(command Command) (string, error) {
		if command.Name == "powershell.exe" {
			return plan.State.ServiceDefinition, nil
		}
		if command.Name == "sc.exe" && command.Args[0] == "query" {
			queries++
			if queries == 1 {
				return "STATE : 2 START_PENDING", nil
			}
			return "STATE : 4 RUNNING", nil
		}
		return "", nil
	}}
	if _, err := Install(context.Background(), plan, Dependencies{Files: files, Runner: runner}, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if queries != 2 {
		t.Fatalf("service state queries = %d, want 2", queries)
	}
	for _, command := range runner.snapshot() {
		if command.Name == "sc.exe" && command.Args[0] == "start" {
			t.Fatalf("START_PENDING partial service was started twice: %v", runner.snapshot())
		}
	}
}

func TestWindowsServiceStateWaitsForStopPending(t *testing.T) {
	queries := 0
	runner := &testRunner{handle: func(command Command) (string, error) {
		queries++
		if queries == 1 {
			return "STATE : 3 STOP_PENDING", nil
		}
		return "STATE : 1 STOPPED", nil
	}}
	state, err := waitWindowsServiceStable(context.Background(), runner, "Mira")
	if err != nil {
		t.Fatal(err)
	}
	if state != windowsServiceStopped || queries != 2 {
		t.Fatalf("stable state=%d queries=%d", state, queries)
	}
}

func TestWindowsDoctorAndRepairCoverHandoffRecoveryPolicy(t *testing.T) {
	stateDir := t.TempDir()
	files := &testFileSystem{osRelease: []byte("ID=debian\n")}
	plan, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "windows", ServiceOwner: ServiceOwnerMira,
		WindowsExecutable: filepath.Join(stateDir, "versions", "1.0.0", "mira.exe"),
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	stateContent, err := encodeState(plan.State)
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string][]byte{
		filepath.Join(stateDir, "install-state.json"): stateContent,
		filepath.Join(stateDir, "service-owner"):      []byte("mira\n"),
		filepath.Join(stateDir, "current"):            []byte("1.0.0\n"),
	} {
		if err := files.AtomicWrite(path, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	policyRepaired := false
	recoveryActions := make([]byte, 44)
	binary.LittleEndian.PutUint32(recoveryActions[0:4], 86400)
	binary.LittleEndian.PutUint32(recoveryActions[12:16], 3)
	binary.LittleEndian.PutUint32(recoveryActions[16:20], 20)
	for index := 0; index < 3; index++ {
		binary.LittleEndian.PutUint32(recoveryActions[20+index*8:24+index*8], 1)
		binary.LittleEndian.PutUint32(recoveryActions[24+index*8:28+index*8], 3000)
	}
	recoveryEncoded := base64.StdEncoding.EncodeToString(recoveryActions)
	runner := &testRunner{handle: func(command Command) (string, error) {
		if command.Name == "powershell.exe" {
			if strings.Contains(command.Args[len(command.Args)-1], "ConvertTo-Json") {
				if policyRepaired {
					return fmt.Sprintf(`{"state":"Running","startMode":"Auto","failureFlag":1,"failureActions":%q}`, recoveryEncoded), nil
				}
				return `{"state":"Stopped","startMode":"Manual","failureFlag":0,"failureActions":""}`, nil
			}
			return plan.State.ServiceDefinition, nil
		}
		if command.Name == "sc.exe" {
			switch command.Args[0] {
			case "query":
				return "STATE : 4 RUNNING", nil
			case "failureflag":
				policyRepaired = true
			}
		}
		return "", nil
	}}
	dependencies := Dependencies{Files: files, Runner: runner}
	report := Doctor(context.Background(), stateDir, dependencies)
	if report.Healthy {
		t.Fatalf("Windows policy drift was not detected: %+v", report)
	}
	codes := map[string]bool{}
	for _, finding := range report.Findings {
		codes[finding.Code] = true
	}
	for _, code := range []string{"service_inactive", "service_start_mode_drift", "service_recovery_drift"} {
		if !codes[code] {
			t.Errorf("missing %s in %+v", code, report.Findings)
		}
	}
	if _, err := Repair(context.Background(), plan, dependencies, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if report := Doctor(context.Background(), stateDir, dependencies); !report.Healthy {
		t.Fatalf("Windows policy remained unhealthy after repair: %+v", report)
	}
}

func TestInstallLockHonorsContext(t *testing.T) {
	stateDir := t.TempDir()
	first, err := acquireInstallLock(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := acquireInstallLock(ctx, stateDir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended install lock error = %v", err)
	}
}

func TestUninstallWaitsForSupervisorUpdateMaintenanceLock(t *testing.T) {
	stateDir := t.TempDir()
	unitPath := filepath.Join(t.TempDir(), "mira.service")
	files := &testFileSystem{osRelease: []byte("ID=debian\n")}
	runner := &testRunner{}
	versionDir := filepath.Join(stateDir, "versions", "1.0.0")
	if err := os.MkdirAll(versionDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, "mira"), []byte("release"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", "1.0.0"), filepath.Join(stateDir, "current")); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira,
		ServiceScope: ScopeSystem, SystemdUnitPath: unitPath,
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	dependencies := Dependencies{Files: files, Runner: runner}
	if _, err := Install(context.Background(), plan, dependencies, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	baseline := len(runner.snapshot())
	updateLock, err := supervisor.AcquireMaintenanceLock(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Uninstall(context.Background(), stateDir, ServiceOwnerMira, dependencies, ApplyOptions{})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if commands := runner.snapshot(); len(commands) != baseline {
		updateLock.Close()
		t.Fatalf("uninstall interleaved with active update maintenance: %v", commands[baseline:])
	}
	if err := updateLock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("uninstall did not continue after update maintenance completed")
	}
	commands := runner.snapshot()
	if len(commands) <= baseline || !slices.Contains(commands[len(commands)-2].Args, "disable") {
		t.Fatalf("uninstall did not synchronously stop the service after update: %v", commands[baseline:])
	}
}

func TestWindowsRecoveryActionsValidation(t *testing.T) {
	value := make([]byte, 44)
	binary.LittleEndian.PutUint32(value[0:4], 86400)
	binary.LittleEndian.PutUint32(value[12:16], 3)
	binary.LittleEndian.PutUint32(value[16:20], 20)
	for index := 0; index < 3; index++ {
		binary.LittleEndian.PutUint32(value[20+index*8:24+index*8], 1)
		binary.LittleEndian.PutUint32(value[24+index*8:28+index*8], 3000)
	}
	encoded := base64.StdEncoding.EncodeToString(value)
	if !validWindowsFailureActions(encoded) {
		t.Fatal("rejected the configured restart actions")
	}
	value[24] = 0
	if validWindowsFailureActions(base64.StdEncoding.EncodeToString(value)) {
		t.Fatal("accepted a changed restart delay")
	}
}

func TestRepairRestoresRecordedLinuxServiceDefinition(t *testing.T) {
	stateDir := t.TempDir()
	unitPath := filepath.Join(t.TempDir(), "mira.service")
	files := &testFileSystem{osRelease: []byte("ID=debian\n")}
	runner := &testRunner{}
	plan, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira, SystemdUnitPath: unitPath,
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	dependencies := Dependencies{Files: files, Runner: runner}
	if _, err := Install(context.Background(), plan, dependencies, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("drifted\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Repair(context.Background(), plan, dependencies, ApplyOptions{}); err != nil {
		t.Fatalf("repair rejected recorded service drift: %v", err)
	}
	content, err := os.ReadFile(unitPath)
	if err != nil || string(content) != plan.State.ServiceDefinition {
		t.Fatalf("repair did not restore unit: %q err=%v", content, err)
	}
}

func TestFreshInstallRefusesExistingServiceFromAnotherStateDirectory(t *testing.T) {
	t.Run("linux", func(t *testing.T) {
		stateDir := t.TempDir()
		unitPath := filepath.Join(t.TempDir(), "mira.service")
		files := &testFileSystem{osRelease: []byte("ID=debian\n")}
		if err := os.WriteFile(unitPath, []byte("another installation\n"), 0644); err != nil {
			t.Fatal(err)
		}
		plan, err := BuildPlan(PlanOptions{
			StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira, SystemdUnitPath: unitPath,
		}, files)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Install(context.Background(), plan, Dependencies{Files: files, Runner: &testRunner{}}, ApplyOptions{}); !errors.Is(err, ErrOwnershipConflict) {
			t.Fatalf("collision error = %v", err)
		}
		content, _ := os.ReadFile(unitPath)
		if string(content) != "another installation\n" {
			t.Fatalf("existing service was overwritten: %q", content)
		}
	})

	t.Run("systemd loaded unit", func(t *testing.T) {
		stateDir := t.TempDir()
		unitPath := filepath.Join(t.TempDir(), "mira.service")
		files := &testFileSystem{osRelease: []byte("ID=debian\n")}
		plan, err := BuildPlan(PlanOptions{
			StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira, SystemdUnitPath: unitPath,
		}, files)
		if err != nil {
			t.Fatal(err)
		}
		runner := &testRunner{handle: func(command Command) (string, error) {
			if command.Name == "systemctl" && slices.Contains(command.Args, "--property=FragmentPath") {
				return "/usr/lib/systemd/system/mira.service\n", nil
			}
			return "", nil
		}}
		if _, err := Install(context.Background(), plan, Dependencies{Files: files, Runner: runner}, ApplyOptions{}); !errors.Is(err, ErrOwnershipConflict) {
			t.Fatalf("loaded unit collision error = %v", err)
		}
		if files.writeCount() != 0 {
			t.Fatalf("loaded unit collision wrote %d files", files.writeCount())
		}
	})

	t.Run("windows", func(t *testing.T) {
		stateDir := t.TempDir()
		files := &testFileSystem{osRelease: []byte("ID=debian\n")}
		plan, err := BuildPlan(PlanOptions{
			StateDir: stateDir, Version: "1.0.0", Platform: "windows", ServiceOwner: ServiceOwnerMira,
			WindowsExecutable: filepath.Join(stateDir, "versions", "1.0.0", "mira.exe"),
		}, files)
		if err != nil {
			t.Fatal(err)
		}
		runner := &testRunner{output: `"C:\\another\\mira.exe" supervisor --state-dir "C:\\another"`}
		if _, err := Install(context.Background(), plan, Dependencies{Files: files, Runner: runner}, ApplyOptions{}); !errors.Is(err, ErrOwnershipConflict) {
			t.Fatalf("collision error = %v", err)
		}
		if files.writeCount() != 0 || len(runner.snapshot()) != 1 {
			t.Fatalf("collision caused side effects: writes=%d commands=%v", files.writeCount(), runner.snapshot())
		}
	})
}

func TestRecordCurrentVersionPreservesOwnerAndServiceDefinition(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux service state uses Unix symlink semantics")
	}
	stateDir := t.TempDir()
	unitPath := filepath.Join(t.TempDir(), "mira.service")
	files := &testFileSystem{osRelease: []byte("ID=debian\n")}
	plan, err := BuildPlan(PlanOptions{
		StateDir: stateDir, Version: "1.0.0", Platform: "linux", ServiceOwner: ServiceOwnerMira,
		SystemdUnitPath: unitPath, Role: RoleServer,
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.0.0", "2.0.0"} {
		if err := os.MkdirAll(filepath.Join(stateDir, "versions", version), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("versions", "1.0.0"), filepath.Join(stateDir, "current")); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), plan, Dependencies{Files: files, Runner: &testRunner{}}, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(stateDir, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", "2.0.0"), filepath.Join(stateDir, "current")); err != nil {
		t.Fatal(err)
	}
	if err := RecordCurrentVersion(context.Background(), stateDir, ServiceOwnerNix, "2.0.0"); !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("wrong owner updated state: %v", err)
	}
	if err := RecordCurrentVersion(context.Background(), stateDir, ServiceOwnerMira, "1.0.0"); err == nil {
		t.Fatal("recorded a release not selected by current")
	}
	if err := RecordCurrentVersion(context.Background(), stateDir, ServiceOwnerMira, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(nil, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != "2.0.0" || state.ServiceOwner != ServiceOwnerMira || state.Role != RoleServer || state.ServiceDefinition != plan.State.ServiceDefinition {
		t.Fatalf("unexpected recorded state: %+v", state)
	}
}

package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ssine/mira/node/internal/installation"
)

func promptServiceOwner(stdin io.Reader, stdout io.Writer) (installation.ServiceOwner, error) {
	file, ok := stdin.(*os.File)
	if !ok {
		return "", installation.ErrOwnerChoiceNeeded
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return "", installation.ErrOwnerChoiceNeeded
	}
	fmt.Fprintln(stdout, "检测到 NixOS，请选择 Supervisor 的系统服务管理方式：")
	fmt.Fprintln(stdout, "  1. 由 Nix 管理（推荐）")
	fmt.Fprintln(stdout, "  2. 由 Mira 直接管理 systemd")
	fmt.Fprint(stdout, "请选择 [1/2]: ")
	line, err := bufio.NewReader(file).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "1", "nix":
		return installation.ServiceOwnerNix, nil
	case "2", "mira":
		return installation.ServiceOwnerMira, nil
	default:
		return "", fmt.Errorf("请选择 1（nix）或 2（mira）")
	}
}

func runSystemInstall(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) (any, error) {
	defaultState, err := defaultSupervisorStateDir()
	if err != nil {
		return nil, err
	}
	set := flagSet("install")
	stateDir := set.String("state-dir", defaultState, "Mira state directory")
	role := set.String("role", installation.RoleNode, "node or server")
	ownerValue := set.String("service-owner", "", "nix or mira")
	serviceManager := set.String("service-manager", "", "auto, systemd, or procd")
	serviceScope := set.String("service-scope", "", "user or system")
	systemdUnit := set.String("systemd-unit", "", "systemd unit path")
	nixSnippet := set.String("nix-snippet", "", "generated Nix module path")
	dryRun := set.Bool("dry-run", false, "show the installation without applying it")
	serverURL := set.String("server-url", "", "configure this Node for a Mira Server")
	if err := set.Parse(args); err != nil {
		return nil, err
	}
	if set.NArg() != 0 {
		return nil, fmt.Errorf("unexpected install arguments: %s", strings.Join(set.Args(), " "))
	}
	owner := installation.ServiceOwner(*ownerValue)
	detectedNixOS, err := installation.DetectNixOS(nil)
	if err != nil {
		return nil, err
	}
	if owner == "" && detectedNixOS {
		owner, err = promptServiceOwner(stdin, stdout)
		if err != nil {
			return nil, fmt.Errorf("%w; pass --service-owner nix or --service-owner mira", err)
		}
	}
	plan, err := installation.BuildPlan(installation.PlanOptions{
		StateDir: *stateDir, Version: Version, Platform: runtime.GOOS, ServiceOwner: owner, Role: *role,
		ServiceManager: installation.ServiceManager(*serviceManager), ServiceScope: *serviceScope,
		SystemdUnitPath: *systemdUnit, NixSnippetPath: *nixSnippet,
	}, nil)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"status": "planned", "stateDir": plan.StateDir, "version": Version, "role": plan.State.Role,
		"serviceOwner": plan.State.ServiceOwner, "serviceManager": plan.State.ServiceManager,
		"serviceScope": plan.State.ServiceScope, "servicePath": plan.State.ServicePath, "dryRun": *dryRun,
	}
	if *dryRun {
		if plan.State.ServiceOwner == installation.ServiceOwnerNix {
			result["nixModule"] = plan.NixSnippet
		}
		return result, nil
	}
	if plan.State.ServiceManager == installation.ServiceManagerProcd && os.Geteuid() != 0 {
		return nil, fmt.Errorf("procd service installation requires root")
	}
	if existing, err := installation.LoadState(nil, plan.StateDir); err == nil {
		unchanged := existing.Version == plan.State.Version && existing.Role == plan.State.Role &&
			existing.ServiceOwner == plan.State.ServiceOwner && existing.ServiceManager == plan.State.ServiceManager && existing.ServiceScope == plan.State.ServiceScope &&
			existing.Platform == plan.State.Platform && existing.ServiceName == plan.State.ServiceName &&
			existing.ServicePath == plan.State.ServicePath && existing.ServiceDefinitionSHA256 == plan.State.ServiceDefinitionSHA256
		if !unchanged {
			return nil, fmt.Errorf("Mira is already installed in %s; use `mira update` for versions or an explicit ownership migration for service changes", plan.StateDir)
		}
		executable, executableErr := installationCurrentExecutable(plan.StateDir)
		if executableErr != nil {
			return nil, executableErr
		}
		result["status"] = "already_installed"
		result["executable"] = executable
		result["applied"] = false
		return result, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect existing Mira installation: %w", err)
	}
	if *serverURL == "" && *role == installation.RoleServer {
		*serverURL = "http://127.0.0.1:8787"
	}
	if *serverURL == "" {
		return nil, fmt.Errorf("--server-url is required when installing a Node")
	}
	nodeConfig := filepath.Join(plan.StateDir, "node.json")
	nodeIdentity := filepath.Join(plan.StateDir, "node-identity.json")
	installedExecutable := ""
	report, err := installation.InstallPrepared(ctx, plan, installation.Dependencies{}, installation.ApplyOptions{}, func() error {
		if _, err := runSetup([]string{"--server", *serverURL, "--config", nodeConfig, "--identity", nodeIdentity}); err != nil {
			return fmt.Errorf("configure Node: %w", err)
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		installedExecutable, err = installation.BootstrapRelease(executable, plan.StateDir, Version)
		return err
	})
	if err != nil {
		return nil, err
	}
	result["executable"] = installedExecutable
	result["applied"] = report.Applied
	if plan.State.ServiceOwner == installation.ServiceOwnerNix {
		result["status"] = "configuration_generated"
		result["next"] = "Import the generated module into your NixOS configuration and run your normal reviewed deployment workflow."
	} else {
		result["status"] = "installed"
	}
	return result, nil
}

func runDoctor(args []string) (any, error) {
	defaultState, err := defaultSupervisorStateDir()
	if err != nil {
		return nil, err
	}
	set := flagSet("doctor")
	stateDir := set.String("state-dir", defaultState, "Mira state directory")
	if err := set.Parse(args); err != nil {
		return nil, err
	}
	if set.NArg() != 0 {
		return nil, fmt.Errorf("unexpected doctor arguments")
	}
	return installation.Doctor(context.Background(), *stateDir, installation.Dependencies{}), nil
}

func runRepair(ctx context.Context, args []string) (any, error) {
	defaultState, err := defaultSupervisorStateDir()
	if err != nil {
		return nil, err
	}
	set := flagSet("repair")
	stateDir := set.String("state-dir", defaultState, "Mira state directory")
	dryRun := set.Bool("dry-run", false, "show changes without applying them")
	if err := set.Parse(args); err != nil {
		return nil, err
	}
	state, err := installation.LoadState(nil, *stateDir)
	if err != nil {
		return nil, err
	}
	if state.ServiceManager == installation.ServiceManagerProcd && os.Geteuid() != 0 && !*dryRun {
		return nil, fmt.Errorf("repairing a procd service requires root")
	}
	options := installation.PlanOptions{StateDir: *stateDir, Version: state.Version, Platform: state.Platform, ServiceOwner: state.ServiceOwner, ServiceManager: state.ServiceManager, Role: state.Role, ServiceScope: state.ServiceScope}
	if state.ServiceOwner == installation.ServiceOwnerNix {
		options.NixSnippetPath = state.ServicePath
	} else if state.Platform == "linux" && state.ServiceManager == installation.ServiceManagerSystemd {
		options.SystemdUnitPath = state.ServicePath
	} else if state.Platform == "linux" {
		options.ProcdInitPath = state.ServicePath
	} else {
		options.WindowsServiceName = state.ServiceName
		executable, executableErr := installationCurrentExecutable(*stateDir)
		if executableErr != nil {
			return nil, executableErr
		}
		options.WindowsExecutable = executable
	}
	plan, err := installation.BuildPlan(options, nil)
	if err != nil {
		return nil, err
	}
	report, err := installation.Repair(ctx, plan, installation.Dependencies{}, installation.ApplyOptions{DryRun: *dryRun})
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "repaired", "applied": report.Applied, "serviceOwner": state.ServiceOwner, "serviceManager": state.ServiceManager, "dryRun": *dryRun}, nil
}

func installationCurrentExecutable(stateDir string) (string, error) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		return filepath.Join(stateDir, "current", "mira"), nil
	}
	// Windows service definitions are reconstructed from the recorded version;
	// the ownership validator still checks the installed service before changes.
	state, err := installation.LoadState(nil, stateDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "versions", state.Version, "mira.exe"), nil
}

func runUninstall(ctx context.Context, args []string) (any, error) {
	defaultState, err := defaultSupervisorStateDir()
	if err != nil {
		return nil, err
	}
	set := flagSet("uninstall")
	stateDir := set.String("state-dir", defaultState, "Mira state directory")
	dryRun := set.Bool("dry-run", false, "show changes without applying them")
	if err := set.Parse(args); err != nil {
		return nil, err
	}
	state, err := installation.LoadState(nil, *stateDir)
	if err != nil {
		return nil, err
	}
	if state.ServiceManager == installation.ServiceManagerProcd && os.Geteuid() != 0 && !*dryRun {
		return nil, fmt.Errorf("uninstalling a procd service requires root")
	}
	report, err := installation.Uninstall(ctx, *stateDir, state.ServiceOwner, installation.Dependencies{}, installation.ApplyOptions{DryRun: *dryRun})
	if errors.Is(err, installation.ErrNixManualRemoval) {
		return map[string]any{"status": "manual_action_required", "serviceOwner": state.ServiceOwner, "instructions": report.Instructions}, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "uninstalled", "applied": report.Applied, "serviceOwner": state.ServiceOwner, "serviceManager": state.ServiceManager, "dryRun": *dryRun}, nil
}

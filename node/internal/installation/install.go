package installation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ssine/mira/node/internal/supervisor"
)

func Install(ctx context.Context, plan InstallPlan, dependencies Dependencies, options ApplyOptions) (ApplyReport, error) {
	return InstallPrepared(ctx, plan, dependencies, options, nil)
}

// InstallPrepared holds the ownership lock across release preparation, service
// registration and state persistence. This prevents two installers from each
// switching current before only one discovers the ownership conflict.
func InstallPrepared(ctx context.Context, plan InstallPlan, dependencies Dependencies, options ApplyOptions, prepare func() error) (ApplyReport, error) {
	return installPrepared(ctx, plan, dependencies, options, prepare, false)
}

func installPrepared(ctx context.Context, plan InstallPlan, dependencies Dependencies, options ApplyOptions, prepare func() error, allowServiceDrift bool) (ApplyReport, error) {
	dependencies = withDefaults(dependencies)
	if err := validatePlan(plan); err != nil {
		return ApplyReport{Plan: plan}, err
	}
	if options.DryRun {
		if _, _, err := inspectInstall(ctx, plan, dependencies, allowServiceDrift); err != nil {
			return ApplyReport{Plan: plan}, err
		}
		return ApplyReport{Plan: plan}, nil
	}
	if err := dependencies.Files.MkdirAll(plan.StateDir, 0700); err != nil {
		return ApplyReport{Plan: plan}, err
	}
	maintenance, err := supervisor.AcquireMaintenanceLock(ctx, plan.StateDir)
	if err != nil {
		return ApplyReport{Plan: plan}, err
	}
	defer maintenance.Close()
	lock, err := acquireInstallLock(ctx, plan.StateDir)
	if err != nil {
		return ApplyReport{Plan: plan}, err
	}
	defer lock.Close()
	_, serviceExists, err := inspectInstall(ctx, plan, dependencies, allowServiceDrift)
	if err != nil {
		return ApplyReport{Plan: plan}, err
	}
	if err := dependencies.Files.AtomicWrite(plan.OwnerPath, []byte(string(plan.State.ServiceOwner)+"\n"), 0600); err != nil {
		return ApplyReport{Plan: plan}, fmt.Errorf("persist service owner: %w", err)
	}
	if prepare != nil {
		if err := prepare(); err != nil {
			return ApplyReport{Plan: plan}, err
		}
	}
	for _, file := range plan.Files {
		if err := dependencies.Files.MkdirAll(filepath.Dir(file.Path), file.DirMode); err != nil {
			return ApplyReport{Plan: plan}, err
		}
		if err := dependencies.Files.AtomicWrite(file.Path, file.Content, file.Mode); err != nil {
			return ApplyReport{Plan: plan}, err
		}
	}
	// Nix ownership is intentionally command-free. The user reviews and imports
	// the generated module; Mira never invokes nixos-rebuild or systemctl here.
	if plan.State.ServiceOwner != ServiceOwnerNix {
		commands := plan.Commands
		if serviceExists && plan.State.Platform == "windows" && len(commands) > 0 {
			commands = cloneCommands(commands)
			commands[0].Args[0] = "config"
			state, queryErr := waitWindowsServiceStable(ctx, dependencies.Runner, plan.State.ServiceName)
			if queryErr != nil {
				return ApplyReport{Plan: plan}, fmt.Errorf("inspect Windows service state: %w", queryErr)
			}
			if len(commands) > 1 && commands[len(commands)-1].Args[0] == "start" {
				switch state {
				case windowsServiceRunning:
					commands = commands[:len(commands)-1]
				case windowsServicePaused:
					commands[len(commands)-1] = Command{Name: "sc.exe", Args: []string{"continue", plan.State.ServiceName}}
				}
			}
		}
		for _, command := range commands {
			if _, err := dependencies.Runner.Run(ctx, command.Name, command.Args...); err != nil {
				return ApplyReport{Plan: plan}, fmt.Errorf("install service: %w", err)
			}
		}
	}
	content, err := encodeState(plan.State)
	if err != nil {
		return ApplyReport{Plan: plan}, err
	}
	if err := dependencies.Files.AtomicWrite(plan.StatePath, content, 0600); err != nil {
		return ApplyReport{Plan: plan}, err
	}
	return ApplyReport{Plan: plan, Applied: true}, nil
}

func inspectInstall(ctx context.Context, plan InstallPlan, dependencies Dependencies, allowServiceDrift bool) (InstallState, bool, error) {
	existingState, err := requireOwner(dependencies.Files, plan.StateDir, plan.State.ServiceOwner, true)
	if err != nil {
		return InstallState{}, false, err
	}
	if existingState.SchemaVersion != 0 && existingState != plan.State {
		return InstallState{}, false, fmt.Errorf("%w: an existing installation may only change version through mira update or service settings through mira repair", ErrOwnershipConflict)
	}
	if plan.State.ServiceOwner != ServiceOwnerMira {
		return existingState, false, nil
	}
	definition, definitionErr := currentServiceDefinition(ctx, dependencies, plan.State)
	switch {
	case definitionErr == nil:
		if definition != plan.State.ServiceDefinition {
			if allowServiceDrift && existingState.SchemaVersion != 0 {
				return existingState, true, nil
			}
			return InstallState{}, false, fmt.Errorf("%w: service %s already belongs to a different Mira state directory or installation", ErrOwnershipConflict, plan.State.ServiceName)
		}
		return existingState, true, nil
	case errors.Is(definitionErr, os.ErrNotExist):
		if existingState.SchemaVersion != 0 {
			if allowServiceDrift {
				return existingState, false, nil
			}
			return InstallState{}, false, fmt.Errorf("installed service %s is missing; run mira repair", plan.State.ServiceName)
		}
		return existingState, false, nil
	default:
		return InstallState{}, false, fmt.Errorf("inspect existing service: %w", definitionErr)
	}
}

func cloneCommands(source []Command) []Command {
	result := make([]Command, len(source))
	for index, command := range source {
		result[index] = Command{Name: command.Name, Args: append([]string(nil), command.Args...)}
	}
	return result
}

func validatePlan(plan InstallPlan) error {
	if err := validateState(plan.State); err != nil {
		return err
	}
	if !safeAbsolutePath(plan.StateDir) || plan.StatePath != filepath.Join(plan.StateDir, "install-state.json") || plan.OwnerPath != filepath.Join(plan.StateDir, "service-owner") {
		return fmt.Errorf("invalid install plan state paths")
	}
	if plan.State.ServiceOwner == ServiceOwnerNix {
		if len(plan.Commands) != 0 || len(plan.Files) != 1 || !pathInside(plan.StateDir, plan.Files[0].Path) ||
			plan.Files[0].Path != plan.State.ServicePath || string(plan.Files[0].Content) != plan.State.ServiceDefinition {
			return fmt.Errorf("Nix install plan may only generate one reviewable snippet inside the state directory")
		}
	} else if plan.State.Platform == "linux" {
		if len(plan.Files) != 1 || plan.Files[0].Path != plan.State.ServicePath || string(plan.Files[0].Content) != plan.State.ServiceDefinition {
			return fmt.Errorf("Linux install plan service unit does not match install state")
		}
	} else if len(plan.Files) != 0 {
		return fmt.Errorf("Windows install plan cannot contain service files")
	}
	return nil
}

// Repair reapplies the reviewed plan only after verifying that the existing
// installation has the same owner.
func Repair(ctx context.Context, plan InstallPlan, dependencies Dependencies, options ApplyOptions) (ApplyReport, error) {
	dependencies = withDefaults(dependencies)
	if _, err := requireOwner(dependencies.Files, plan.StateDir, plan.State.ServiceOwner, false); err != nil {
		return ApplyReport{Plan: plan}, err
	}
	return installPrepared(ctx, plan, dependencies, options, nil, true)
}

// Uninstall removes Mira-managed services. Nix-owned service removal is always
// a reviewed configuration change and therefore returns instructions without
// modifying files or invoking the service manager.
func Uninstall(ctx context.Context, stateDir string, owner ServiceOwner, dependencies Dependencies, options ApplyOptions) (UninstallReport, error) {
	dependencies = withDefaults(dependencies)
	state, err := requireOwner(dependencies.Files, stateDir, owner, false)
	if err != nil {
		return UninstallReport{}, err
	}
	maintenance, err := supervisor.AcquireMaintenanceLock(ctx, stateDir)
	if err != nil {
		return UninstallReport{}, err
	}
	defer maintenance.Close()
	lock, err := acquireInstallLock(ctx, stateDir)
	if err != nil {
		return UninstallReport{}, err
	}
	defer lock.Close()
	state, err = requireOwner(dependencies.Files, stateDir, owner, false)
	if err != nil {
		return UninstallReport{}, err
	}
	if owner == ServiceOwnerNix {
		return UninstallReport{
			ManualActionRequired: true,
			Instructions:         "Remove the generated Mira module import from your NixOS configuration, review the change, and run your normal deployment workflow.",
		}, ErrNixManualRemoval
	}
	definition, err := currentServiceDefinition(ctx, dependencies, state)
	if err != nil {
		return UninstallReport{}, fmt.Errorf("verify installed service before removal: %w", err)
	}
	digest := sha256.Sum256([]byte(definition))
	if hex.EncodeToString(digest[:]) != state.ServiceDefinitionSHA256 {
		return UninstallReport{}, fmt.Errorf("refusing to remove a service definition that drifted from Mira install state")
	}
	commands := uninstallCommands(state)
	if options.DryRun {
		return UninstallReport{Commands: commands}, nil
	}
	for _, command := range commands {
		if _, err := dependencies.Runner.Run(ctx, command.Name, command.Args...); err != nil {
			return UninstallReport{Commands: commands}, err
		}
	}
	if state.ServicePath != "" {
		if err := dependencies.Files.Remove(state.ServicePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return UninstallReport{Commands: commands}, err
		}
		if state.Platform == "linux" {
			arguments := []string{}
			if state.ServiceScope == ScopeUser {
				arguments = append(arguments, "--user")
			}
			if _, err := dependencies.Runner.Run(ctx, "systemctl", append(arguments, "daemon-reload")...); err != nil {
				return UninstallReport{Commands: commands}, err
			}
		}
	}
	for _, path := range []string{filepath.Join(stateDir, "install-state.json"), filepath.Join(stateDir, "service-owner")} {
		if err := dependencies.Files.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return UninstallReport{Commands: commands}, err
		}
	}
	return UninstallReport{Applied: true, Commands: commands}, nil
}

func uninstallCommands(state InstallState) []Command {
	if state.Platform == "windows" {
		return []Command{
			{Name: "sc.exe", Args: []string{"stop", state.ServiceName}},
			{Name: "sc.exe", Args: []string{"delete", state.ServiceName}},
		}
	}
	arguments := []string{}
	if state.ServiceScope == ScopeUser {
		arguments = append(arguments, "--user")
	}
	return []Command{{Name: "systemctl", Args: append(arguments, "disable", "--now", state.ServiceName)}}
}

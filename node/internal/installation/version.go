package installation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ssine/mira/node/internal/supervisor"
)

// RecordCurrentVersion is called by the long-lived Supervisor after the
// current pointer has committed. Its caller must already hold the state
// directory's maintenance lock. It never changes service ownership and refuses
// to record a version the pointer does not select.
func RecordCurrentVersion(ctx context.Context, stateDir string, owner ServiceOwner, version string) error {
	files := OSFileSystem{}
	layout, err := supervisor.NewLayout(stateDir)
	if err != nil {
		return err
	}
	lock, err := acquireInstallLock(ctx, layout.StateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err := requireOwner(files, layout.StateDir, owner, false)
	if err != nil {
		return err
	}
	current, err := layout.CurrentVersion()
	if err != nil {
		return err
	}
	if current != version {
		return fmt.Errorf("current Mira release is %s, not %s", current, version)
	}
	previousDefinition := state.ServiceDefinition
	if state.Platform == "windows" {
		executable := filepath.Join(layout.VersionsDir, version, "mira.exe")
		definition, definitionErr := windowsCommandLine(executable, layout.StateDir, state.Role, state.ServiceName)
		if definitionErr != nil {
			return definitionErr
		}
		commands := []Command{{Name: "sc.exe", Args: []string{"config", state.ServiceName, "binPath=", definition, "start=", "auto"}}}
		commands = append(commands, windowsServicePolicyCommands(state.ServiceName)...)
		configured := false
		for _, command := range commands {
			if _, err := (ExecRunner{}).Run(ctx, command.Name, command.Args...); err != nil {
				if configured {
					if rollbackErr := restoreWindowsServiceDefinition(state.ServiceName, previousDefinition); rollbackErr != nil {
						return fmt.Errorf("prepare Windows service handoff: %w; restore Windows service: %v", err, rollbackErr)
					}
				}
				return fmt.Errorf("prepare Windows service handoff: %w", err)
			}
			configured = true
		}
		state.ServiceDefinition = definition
		digest := sha256.Sum256([]byte(definition))
		state.ServiceDefinitionSHA256 = hex.EncodeToString(digest[:])
	}
	state.Version = version
	if err := validateState(state); err != nil {
		return err
	}
	content, err := encodeState(state)
	if err != nil {
		return err
	}
	if err := files.AtomicWrite(filepath.Join(layout.StateDir, "install-state.json"), content, 0600); err != nil {
		if state.Platform == "windows" {
			rollbackErr := restoreWindowsServiceDefinition(state.ServiceName, previousDefinition)
			if rollbackErr != nil {
				return fmt.Errorf("record current version: %w; restore Windows service: %v", err, rollbackErr)
			}
		}
		return err
	}
	return nil
}

func restoreWindowsServiceDefinition(serviceName, definition string) error {
	rollbackContext, cancelRollback := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelRollback()
	_, err := (ExecRunner{}).Run(rollbackContext, "sc.exe", "config", serviceName, "binPath=", definition, "start=", "auto")
	return err
}

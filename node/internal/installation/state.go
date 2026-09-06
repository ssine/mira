package installation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ssine/mira/node/internal/supervisor"
)

func LoadState(files FileSystem, stateDir string) (InstallState, error) {
	if files == nil {
		files = OSFileSystem{}
	}
	layout, err := supervisor.NewLayout(stateDir)
	if err != nil {
		return InstallState{}, err
	}
	content, err := files.ReadFile(filepath.Join(layout.StateDir, "install-state.json"))
	if err != nil {
		return InstallState{}, err
	}
	var state InstallState
	if err := json.Unmarshal(content, &state); err != nil {
		return InstallState{}, fmt.Errorf("decode Mira install state: %w", err)
	}
	// Schema 1 originally predated an explicit Linux service-manager field.
	// Such installations were all systemd-owned, so normalize them in memory
	// before comparisons, repair planning, and the next state write.
	if state.Platform == "linux" && state.ServiceManager == "" {
		state.ServiceManager = ServiceManagerSystemd
	}
	if err := validateState(state); err != nil {
		return InstallState{}, err
	}
	if state.ServiceOwner == ServiceOwnerNix && !pathInside(layout.StateDir, state.ServicePath) {
		return InstallState{}, fmt.Errorf("Nix service snippet escapes the Mira state directory")
	}
	return state, nil
}

func validateState(state InstallState) error {
	if state.SchemaVersion != installStateSchema {
		return fmt.Errorf("unsupported Mira install state schema %d", state.SchemaVersion)
	}
	if state.ServiceOwner != ServiceOwnerNix && state.ServiceOwner != ServiceOwnerMira {
		return fmt.Errorf("invalid install state service owner %q", state.ServiceOwner)
	}
	if state.Role != RoleNode && state.Role != RoleServer {
		return fmt.Errorf("invalid install state role %q", state.Role)
	}
	if state.ServiceScope != ScopeUser && state.ServiceScope != ScopeSystem {
		return fmt.Errorf("invalid install state service scope %q", state.ServiceScope)
	}
	if state.Platform != "linux" && state.Platform != "windows" {
		return fmt.Errorf("invalid install state platform %q", state.Platform)
	}
	if state.Platform == "linux" && state.ServiceManager != ServiceManagerSystemd && state.ServiceManager != ServiceManagerProcd {
		return fmt.Errorf("invalid Linux install state service manager %q", state.ServiceManager)
	}
	if state.Platform == "windows" && state.ServiceManager != "" {
		return fmt.Errorf("invalid Windows install state service manager %q", state.ServiceManager)
	}
	if state.ServiceOwner == ServiceOwnerNix && state.Platform != "linux" {
		return fmt.Errorf("Nix service owner is only valid on Linux")
	}
	if (state.ServiceOwner == ServiceOwnerNix || state.Platform == "windows") && state.ServiceScope != ScopeSystem {
		return fmt.Errorf("invalid user-scoped install state")
	}
	if state.ServiceManager == ServiceManagerProcd && (state.ServiceOwner != ServiceOwnerMira || state.Role != RoleNode || state.ServiceScope != ScopeSystem) {
		return fmt.Errorf("invalid procd install state")
	}
	if !serviceNamePattern.MatchString(strings.TrimSuffix(state.ServiceName, ".service")) {
		return fmt.Errorf("invalid install state service name %q", state.ServiceName)
	}
	if state.Platform == "linux" && !safeAbsolutePath(state.ServicePath) {
		return fmt.Errorf("invalid install state service path")
	}
	if state.ServiceDefinition == "" {
		return fmt.Errorf("install state service definition is empty")
	}
	if !versionPattern.MatchString(state.Version) {
		return fmt.Errorf("invalid install state version %q", state.Version)
	}
	digest := sha256.Sum256([]byte(state.ServiceDefinition))
	if !strings.EqualFold(state.ServiceDefinitionSHA256, hex.EncodeToString(digest[:])) {
		return fmt.Errorf("install state service definition checksum mismatch")
	}
	return nil
}

func requireOwner(files FileSystem, stateDir string, owner ServiceOwner, allowMissing bool) (InstallState, error) {
	state, err := LoadState(files, stateDir)
	if err == nil {
		if state.ServiceOwner != owner {
			return InstallState{}, fmt.Errorf("%w: installed owner is %s, requested %s", ErrOwnershipConflict, state.ServiceOwner, owner)
		}
	} else if !allowMissing || !errors.Is(err, os.ErrNotExist) {
		return InstallState{}, err
	}
	marker, markerErr := files.ReadFile(filepath.Join(stateDir, "service-owner"))
	if markerErr == nil {
		persisted := ServiceOwner(strings.TrimSpace(string(marker)))
		if persisted != owner {
			return InstallState{}, fmt.Errorf("%w: Supervisor owner is %s, requested %s", ErrOwnershipConflict, persisted, owner)
		}
	} else if !allowMissing || !errors.Is(markerErr, os.ErrNotExist) {
		return InstallState{}, markerErr
	}
	return state, nil
}

func encodeState(state InstallState) ([]byte, error) {
	content, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

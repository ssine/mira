package supervisorapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ssine/mira/node/internal/supervisor"
)

type paths struct {
	stateDir string
	endpoint string
	token    string
	status   string
}

func controlPaths(stateDir string) (paths, error) {
	layout, err := supervisor.NewLayout(stateDir)
	if err != nil {
		return paths{}, err
	}
	return paths{
		stateDir: layout.StateDir,
		endpoint: filepath.Join(layout.StateDir, EndpointFileName),
		token:    filepath.Join(layout.StateDir, TokenFileName),
		status:   filepath.Join(layout.StateDir, StatusFileName),
	}, nil
}

func atomicWrite(path string, content []byte) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	if directoryHandle, err := os.Open(directory); err == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return nil
}

func writeStatus(path string, status OperationStatus) error {
	content, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(content, '\n'))
}

func readStatus(path string) (OperationStatus, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return OperationStatus{}, err
	}
	var status OperationStatus
	if err := json.Unmarshal(content, &status); err != nil {
		return OperationStatus{}, fmt.Errorf("decode Supervisor update status: %w", err)
	}
	if len(content) > 1024*1024 || len(status.OperationID) != 32 || !isLowerHex(status.OperationID) || !validVersion(status.Version) ||
		!validPhase(status.Phase) || status.CreatedAt.IsZero() || status.UpdatedAt.IsZero() ||
		(status.Phase.terminal() && status.FinishedAt == nil) || (!status.Phase.terminal() && status.FinishedAt != nil) {
		return OperationStatus{}, fmt.Errorf("invalid Supervisor update status")
	}
	return status, nil
}

func isLowerHex(value string) bool {
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validPhase(phase Phase) bool {
	switch phase {
	case PhaseStaging, PhaseSwitching, PhaseSucceeded, PhaseRolledBack, PhaseFailed:
		return true
	default:
		return false
	}
}

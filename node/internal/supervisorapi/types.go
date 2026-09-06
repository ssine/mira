// Package supervisorapi exposes a token-authenticated, loopback-only control
// plane for the local Mira Supervisor.
package supervisorapi

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/ssine/mira/node/internal/supervisor"
)

const (
	EndpointFileName = "supervisor-api.endpoint"
	TokenFileName    = "supervisor-api.token"
	StatusFileName   = "supervisor-update-status.json"
)

type Phase string

const (
	PhaseStaging    Phase = "staging"
	PhaseSwitching  Phase = "switching"
	PhaseSucceeded  Phase = "succeeded"
	PhaseRolledBack Phase = "rolled_back"
	PhaseFailed     Phase = "failed"
)

func (phase Phase) terminal() bool {
	return phase == PhaseSucceeded || phase == PhaseRolledBack || phase == PhaseFailed
}

type OperationStatus struct {
	OperationID string     `json:"operationId"`
	Phase       Phase      `json:"phase"`
	Version     string     `json:"version"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
}

type Manager interface {
	ApplyUpdate(context.Context, string) (supervisor.Candidate, error)
}

// RolledBackError marks an update failure for which Manager restored the old
// workers. Other errors are recorded as failed because the API must not infer
// rollback from error text.
type RolledBackError interface {
	error
	RollbackComplete() bool
}

type rollbackError struct{ error }

func (rollbackError) RollbackComplete() bool { return true }
func (failure rollbackError) Unwrap() error  { return failure.error }

func NewRolledBackError(err error) error {
	if err == nil {
		return nil
	}
	return rollbackError{error: err}
}

type Config struct {
	StateDir string
	Manager  Manager

	Listen          func(network, address string) (net.Listener, error)
	Random          io.Reader
	Now             func() time.Time
	ClassifyFailure func(error) Phase
}

var (
	ErrUpdateInProgress = errors.New("a Mira update is already in progress")
	ErrNoOperation      = errors.New("no Mira update operation is available")
	ErrOperationChanged = errors.New("Mira update status belongs to another operation")
)

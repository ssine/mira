// Package installation plans and applies Mira installation ownership without
// coupling release updates to Mira Server or to an SSH connection.
package installation

import (
	"context"
	"errors"
	"io/fs"

	"github.com/ssine/mira/node/internal/supervisor"
)

type ServiceOwner = supervisor.ServiceOwner

const (
	ServiceOwnerNix  = supervisor.ServiceOwnerNix
	ServiceOwnerMira = supervisor.ServiceOwnerMira
)

var (
	ErrOwnershipConflict = errors.New("Mira installation ownership conflict")
	ErrNixManualRemoval  = errors.New("Nix-owned Mira must be removed through its reviewed Nix configuration")
	ErrOwnerChoiceNeeded = errors.New("NixOS requires an explicit service owner choice")
)

const installStateSchema = 1

type InstallState struct {
	SchemaVersion           int          `json:"schemaVersion"`
	ServiceOwner            ServiceOwner `json:"serviceOwner"`
	Role                    string       `json:"role"`
	ServiceScope            string       `json:"serviceScope"`
	Platform                string       `json:"platform"`
	Version                 string       `json:"version"`
	ServiceName             string       `json:"serviceName"`
	ServicePath             string       `json:"servicePath,omitempty"`
	ServiceDefinition       string       `json:"serviceDefinition"`
	ServiceDefinitionSHA256 string       `json:"serviceDefinitionSha256"`
}

type PlanOptions struct {
	StateDir     string
	Version      string
	Platform     string
	ServiceOwner ServiceOwner
	Role         string
	ServiceScope string

	SystemdUnitPath    string
	NixSnippetPath     string
	WindowsServiceName string
	WindowsExecutable  string
}

type PlannedFile struct {
	Path    string
	Content []byte
	Mode    fs.FileMode
	DirMode fs.FileMode
}

type Command struct {
	Name string
	Args []string
}

type InstallPlan struct {
	State         InstallState
	StateDir      string
	StatePath     string
	OwnerPath     string
	DetectedNixOS bool
	Files         []PlannedFile
	Commands      []Command
	NixSnippet    string
}

type ApplyOptions struct {
	DryRun bool
}

type ApplyReport struct {
	Plan    InstallPlan
	Applied bool
}

type FileSystem interface {
	ReadFile(path string) ([]byte, error)
	Readlink(path string) (string, error)
	Stat(path string) (fs.FileInfo, error)
	MkdirAll(path string, mode fs.FileMode) error
	AtomicWrite(path string, content []byte, mode fs.FileMode) error
	Remove(path string) error
}

type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type Dependencies struct {
	Files  FileSystem
	Runner Runner
}

type Finding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type DoctorReport struct {
	Healthy        bool         `json:"healthy"`
	State          InstallState `json:"state"`
	CurrentVersion string       `json:"currentVersion,omitempty"`
	Findings       []Finding    `json:"findings,omitempty"`
}

type UninstallReport struct {
	Applied              bool
	ManualActionRequired bool
	Instructions         string
	Commands             []Command
}

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	currentPointer   = "current"
	previousPointer  = "previous"
	candidatePointer = "candidate"
)

var versionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Layout owns the small amount of persistent state used by Supervisor. Pointer
// files contain version names, never arbitrary paths, so every resolved release
// remains below VersionsDir.
type Layout struct {
	StateDir         string
	VersionsDir      string
	Current          string
	Previous         string
	Candidate        string
	ServiceOwnerFile string
	Lock             string
	Maintenance      string
}

type State struct {
	ServiceOwner ServiceOwner
	Current      string
	Previous     string
	Candidate    string
}

// NewLayout deliberately requires a caller-selected, absolute, non-root state
// directory. The Supervisor must never guess a broad system path to mutate.
func NewLayout(stateDir string) (Layout, error) {
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return Layout{}, fmt.Errorf("supervisor state directory must be absolute")
	}
	clean := filepath.Clean(stateDir)
	volume := filepath.VolumeName(clean)
	root := string(filepath.Separator)
	if volume != "" {
		root = volume + string(filepath.Separator)
	}
	if samePath(clean, root) {
		return Layout{}, fmt.Errorf("supervisor state directory cannot be a filesystem root")
	}
	return Layout{
		StateDir:         clean,
		VersionsDir:      filepath.Join(clean, "versions"),
		Current:          filepath.Join(clean, currentPointer),
		Previous:         filepath.Join(clean, previousPointer),
		Candidate:        filepath.Join(clean, candidatePointer),
		ServiceOwnerFile: filepath.Join(clean, "service-owner"),
		Lock:             filepath.Join(clean, "supervisor.lock"),
		Maintenance:      filepath.Join(clean, "maintenance.lock"),
	}, nil
}

func samePath(left, right string) bool {
	if filepath.Separator == '\\' {
		return strings.EqualFold(left, right)
	}
	return left == right
}

// Prepare creates only the dedicated state and versions directories.
func (layout Layout) Prepare() error {
	if err := os.MkdirAll(layout.VersionsDir, 0700); err != nil {
		return fmt.Errorf("create supervisor state: %w", err)
	}
	return nil
}

// VersionDir returns the only directory into which a release may be staged.
func (layout Layout) VersionDir(version string) (string, error) {
	if !versionNamePattern.MatchString(version) || version == "." || version == ".." {
		return "", fmt.Errorf("invalid Mira version %q", version)
	}
	return filepath.Join(layout.VersionsDir, version), nil
}

// InitializeCurrent is intended for installers and first-run bootstrap. It does
// not create release contents; the referenced version directory must exist.
func (layout Layout) InitializeCurrent(version string) error {
	directory, err := layout.VersionDir(version)
	if err != nil {
		return err
	}
	if info, err := os.Stat(directory); err != nil {
		return fmt.Errorf("initialize current Mira version: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("Mira version path is not a directory: %s", directory)
	}
	return layout.writePointer(currentPointer, version)
}

// CurrentVersion resolves the current release pointer.
func (layout Layout) CurrentVersion() (string, error) {
	return layout.readPointer(currentPointer)
}

// CurrentExecutable returns the executable selected by current. On Unix the
// stable path is <state>/current/mira, suitable for Nix ExecStart. Windows uses
// a restricted pointer file and therefore returns the real version path; its
// installer/service handoff must never assume current/mira.exe is traversable.
func (layout Layout) CurrentExecutable() (string, error) {
	return currentExecutable(layout)
}

// PreviousVersion resolves the last successfully replaced release pointer.
func (layout Layout) PreviousVersion() (string, error) {
	return layout.readPointer(previousPointer)
}

// CandidateVersion resolves the release currently being validated.
func (layout Layout) CandidateVersion() (string, error) {
	return layout.readPointer(candidatePointer)
}

// EnsureServiceOwner records which installation system owns process startup.
// Nix and the Mira installer are mutually exclusive; changing ownership is an
// explicit installer migration, never a side effect of Supervisor startup.
func (layout Layout) EnsureServiceOwner(owner ServiceOwner) error {
	if !owner.valid() {
		return fmt.Errorf("invalid service owner %q", owner)
	}
	content, err := os.ReadFile(layout.ServiceOwnerFile)
	if err == nil {
		persisted := ServiceOwner(strings.TrimSpace(string(content)))
		if persisted != owner {
			return fmt.Errorf("%w: state is owned by %s, requested %s", ErrServiceOwnerConflict, persisted, owner)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return layout.writeStateFile(layout.ServiceOwnerFile, string(owner)+"\n")
}

// ReadState exposes the persisted update state to doctor and installer code.
// Missing previous/candidate references are represented by empty strings.
func (layout Layout) ReadState() (State, error) {
	content, err := os.ReadFile(layout.ServiceOwnerFile)
	if err != nil {
		return State{}, err
	}
	state := State{ServiceOwner: ServiceOwner(strings.TrimSpace(string(content)))}
	if !state.ServiceOwner.valid() {
		return State{}, fmt.Errorf("invalid persisted service owner %q", state.ServiceOwner)
	}
	if state.Current, err = layout.CurrentVersion(); err != nil {
		return State{}, err
	}
	if state.Previous, err = layout.PreviousVersion(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return State{}, err
	}
	if state.Candidate, err = layout.CandidateVersion(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return State{}, err
	}
	return state, nil
}

func (layout Layout) pointerPath(name string) string {
	switch name {
	case currentPointer:
		return layout.Current
	case previousPointer:
		return layout.Previous
	case candidatePointer:
		return layout.Candidate
	default:
		panic("unknown supervisor pointer")
	}
}

func (layout Layout) readPointer(name string) (string, error) {
	version, err := readVersionReference(layout, name)
	if err != nil {
		return "", err
	}
	if _, err := layout.VersionDir(version); err != nil {
		return "", fmt.Errorf("read %s version: %w", name, err)
	}
	return version, nil
}

func (layout Layout) writePointer(name, version string) error {
	if _, err := layout.VersionDir(version); err != nil {
		return err
	}
	if err := layout.Prepare(); err != nil {
		return err
	}
	return writeVersionReference(layout, name, version)
}

func (layout Layout) writeStateFile(target, content string) error {
	file, err := os.CreateTemp(layout.StateDir, "."+filepath.Base(target)+"-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.WriteString(content); err != nil {
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
	if err := os.Rename(temporary, target); err != nil {
		return fmt.Errorf("replace supervisor state %s: %w", filepath.Base(target), err)
	}
	return syncDirectory(layout.StateDir)
}

func (layout Layout) removePointer(name string) error {
	err := os.Remove(layout.pointerPath(name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(layout.StateDir)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		// Some platforms do not support syncing directory handles. Atomic rename
		// still provides the required runtime visibility there.
		return nil
	}
	return nil
}

func pathInside(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

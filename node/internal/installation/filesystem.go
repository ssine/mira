package installation

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

type OSFileSystem struct{}

func (OSFileSystem) ReadFile(path string) ([]byte, error)  { return os.ReadFile(path) }
func (OSFileSystem) Readlink(path string) (string, error)  { return os.Readlink(path) }
func (OSFileSystem) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }
func (OSFileSystem) MkdirAll(path string, mode fs.FileMode) error {
	return os.MkdirAll(path, mode)
}
func (OSFileSystem) Remove(path string) error { return os.Remove(path) }

func (OSFileSystem) AtomicWrite(path string, content []byte, mode fs.FileMode) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(mode); err != nil {
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

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	configureServiceEnvironment(command)
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s: %w", name, err)
	}
	return string(output), nil
}

func withDefaults(dependencies Dependencies) Dependencies {
	if dependencies.Files == nil {
		dependencies.Files = OSFileSystem{}
	}
	if dependencies.Runner == nil {
		dependencies.Runner = ExecRunner{}
	}
	return dependencies
}

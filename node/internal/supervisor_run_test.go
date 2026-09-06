package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledStateDirFollowsVersionedExecutable(t *testing.T) {
	stateDir := t.TempDir()
	versionDir := filepath.Join(stateDir, "versions", "1.2.3")
	if err := os.MkdirAll(versionDir, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(versionDir, "mira")
	if err := os.WriteFile(executable, []byte("image"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, found := installedStateDir(executable); found {
		t.Fatal("accepted a version directory without install state")
	}
	if err := os.WriteFile(filepath.Join(stateDir, "install-state.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(stateDir, "current")
	if err := os.Symlink(filepath.Join("versions", "1.2.3"), current); err != nil {
		t.Fatal(err)
	}
	if got, found := installedStateDir(filepath.Join(current, "mira")); !found || got != stateDir {
		t.Fatalf("installedStateDir = %q, %v; want %q, true", got, found, stateDir)
	}
}

package node

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSingleImageSSHLinksStayPrivateAndRejectConflicts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows linked-image acceptance covers hardlinks")
	}
	source := filepath.Join(t.TempDir(), "mira")
	if err := os.WriteFile(source, []byte("isolated image fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(t.TempDir(), "roles")
	dir, err := ensureSingleImageOpenSSHRoles(source, parent)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.Stat(filepath.Join(dir, "sshd"))
	if err != nil {
		t.Fatal(err)
	}
	image, _ := os.Stat(source)
	if !os.SameFile(image, actual) {
		t.Fatal("role is a second image")
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0700 {
		t.Fatal("roles directory is not private")
	}
	if again, err := ensureSingleImageOpenSSHRoles(source, parent); err != nil || again != dir {
		t.Fatal(again, err)
	}
	_ = os.Remove(filepath.Join(dir, "sftp"))
	if err := os.WriteFile(filepath.Join(dir, "sftp"), []byte("unrelated executable"), 0700); err != nil {
		t.Fatal(err)
	}
	// Simulate another process starting with a fresh local cache.
	singleImageRoles.Lock()
	singleImageRoles.self = ""
	singleImageRoles.Unlock()
	if _, err := ensureSingleImageOpenSSHRoles(source, parent); err == nil {
		t.Fatal("unrelated role was accepted or overwritten")
	}
}

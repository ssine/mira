package installation

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBootstrapReleaseIsIdempotentAndRejectsVersionCollision(t *testing.T) {
	stateDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("release-one"), 0755); err != nil {
		t.Fatal(err)
	}
	target, err := BootstrapRelease(source, stateDir, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	wantName := "mira"
	if runtime.GOOS == "windows" {
		wantName += ".exe"
	}
	if filepath.Base(target) != wantName {
		t.Fatalf("target=%s", target)
	}
	alias := filepath.Join(filepath.Dir(target), "sshd")
	if runtime.GOOS == "windows" {
		alias += ".exe"
	}
	targetInfo, targetErr := os.Stat(target)
	aliasInfo, aliasErr := os.Stat(alias)
	if targetErr != nil || aliasErr != nil || !os.SameFile(targetInfo, aliasInfo) {
		t.Fatalf("OpenSSH role does not reference the installed image: target=%v alias=%v", targetErr, aliasErr)
	}
	if _, err := BootstrapRelease(source, stateDir, "1.2.3"); err != nil {
		t.Fatalf("idempotent bootstrap: %v", err)
	}
	if err := os.WriteFile(source, []byte("different"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := BootstrapRelease(source, stateDir, "1.2.3"); err == nil {
		t.Fatal("accepted different contents for an existing version")
	}
}

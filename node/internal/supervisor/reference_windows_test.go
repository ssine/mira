//go:build windows

package supervisor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsCurrentExecutableIsExplicitVersionPath(t *testing.T) {
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory, err := layout.VersionDir("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "mira.exe"), []byte("release"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := layout.InitializeCurrent("1.0.0"); err != nil {
		t.Fatal(err)
	}
	executable, err := layout.CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	wanted := filepath.Join(layout.VersionsDir, "1.0.0", "mira.exe")
	if executable != wanted {
		t.Fatalf("Windows service wiring must use explicit version path: got %q want %q", executable, wanted)
	}
}

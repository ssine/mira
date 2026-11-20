package node

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInstalledIdentityFile(t *testing.T) {
	state := t.TempDir()
	identity := filepath.Join(state, "node-identity.json")
	binary := filepath.Join(state, "versions", "1.0.132", "mira")
	if err := os.MkdirAll(filepath.Dir(binary), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{binary, identity} {
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got := installedIdentityFile(binary); got != "" {
		t.Fatalf("without node.json: %q", got)
	}
	if err := os.WriteFile(filepath.Join(state, "node.json"), []byte(`{"identityFile":`+jsonString(identity)+`}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := installedIdentityFile(binary); got != identity {
		t.Fatalf("versioned image: %q", got)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Dir(binary), filepath.Join(state, "current")); err != nil {
			t.Fatal(err)
		}
		if got := installedIdentityFile(filepath.Join(state, "current", "mira")); got != identity {
			t.Fatalf("current pointer: %q", got)
		}
	}
	if got := installedIdentityFile(filepath.Join(t.TempDir(), "mira")); got != "" {
		t.Fatalf("not an installed image: %q", got)
	}
	if err := os.Remove(identity); err != nil {
		t.Fatal(err)
	}
	if got := installedIdentityFile(binary); got != "" {
		t.Fatalf("missing identity: %q", got)
	}
}

func jsonString(value string) string {
	encoded := []byte{'"'}
	for _, character := range value {
		if character == '\\' || character == '"' {
			encoded = append(encoded, '\\')
		}
		encoded = append(encoded, string(character)...)
	}
	return string(append(encoded, '"'))
}

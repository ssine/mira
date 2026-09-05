package node

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingLogBoundsAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.log")
	log, err := openRotatingLog(path, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	// One write exceeds the entire retention window; rotation must remain
	// bounded and retain the newest bytes without buffering the full stream.
	input := []byte("000000001111111122222222333333334444")
	if n, err := log.Write(input); err != nil || n != len(input) {
		t.Fatalf("write: %d %v", n, err)
	}
	log.Close()
	for name, want := range map[string]string{path: "4444", path + ".1": "33333333", path + ".2": "22222222"} {
		got, err := os.ReadFile(name)
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	log, err = openRotatingLog(path, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := log.Write([]byte("444455")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path + ".1")
	if !bytes.Equal(got, []byte("44444444")) {
		t.Fatalf("restart discarded prior log: %q", got)
	}
	files, _ := filepath.Glob(path + "*")
	if len(files) != 3 {
		t.Fatalf("unbounded backups: %v", files)
	}
}

func TestDesktopURLRemovesCredentials(t *testing.T) {
	got := desktopServerURL("https://user:secret@example.test/mira?token=secret#secret")
	if got != "https://example.test/mira" {
		t.Fatalf("unsafe desktop URL: %s", got)
	}
	for _, value := range []string{"file:///tmp/example", "javascript:alert(1)", "https://"} {
		if desktopServerURL(value) != "" {
			t.Fatalf("accepted %q", value)
		}
	}
}

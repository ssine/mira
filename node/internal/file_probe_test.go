package node

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func probeEntries(t *testing.T, r *capabilityRuntime, params fileParams) []map[string]any {
	t.Helper()
	params.Action = "probe"
	value, err := r.file(params)
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)["entries"].([]map[string]any)
}

func TestFileProbeResolvesHomeRelativeAndAbsoluteReferences(t *testing.T) {
	// Probe reports logical paths; start from a resolved root so a linked
	// temporary directory does not change the expected values.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := newCapabilityRuntime(config{AllowedRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	home := filepath.Join(root, "home")
	work := filepath.Join(root, "work")
	for _, directory := range []string{filepath.Join(home, "notes"), filepath.Join(work, "src"), filepath.Join(work, "~tilde")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{filepath.Join(home, "notes", "todo.md"), filepath.Join(work, "src", "app.js"), filepath.Join(work, "README.md")} {
		if err := os.WriteFile(file, []byte("content"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	entries := probeEntries(t, r, fileParams{Cwd: work, home: home, Paths: []string{
		"~/notes/todo.md", "~", "src/app.js", "./README.md", "../home/notes", filepath.Join(work, "src"),
		"missing.txt", "~tilde", "src/../src/app.js",
	}})
	expected := []struct {
		path, kind string
		exists     bool
	}{
		{filepath.Join(home, "notes", "todo.md"), "file", true},
		{home, "directory", true},
		{filepath.Join(work, "src", "app.js"), "file", true},
		{filepath.Join(work, "README.md"), "file", true},
		{filepath.Join(home, "notes"), "directory", true},
		{filepath.Join(work, "src"), "directory", true},
		{filepath.Join(work, "missing.txt"), "", false},
		{filepath.Join(work, "~tilde"), "directory", true},
		{filepath.Join(work, "src", "app.js"), "file", true},
	}
	if len(entries) != len(expected) {
		t.Fatalf("got %d entries", len(entries))
	}
	for index, want := range expected {
		entry := entries[index]
		if entry["exists"] != want.exists || entry["path"] != want.path || (want.exists && entry["type"] != want.kind) {
			t.Fatalf("entry %d: %#v, want %#v", index, entry, want)
		}
	}
	if entries[6]["reason"] != nil {
		t.Fatalf("a plain missing file needs no reason: %#v", entries[6])
	}
}

func TestFileProbeReportsUnresolvableReferencesWithoutFailingTheBatch(t *testing.T) {
	r, root := previewRuntime(t)
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "leak.txt")); err != nil {
			t.Fatal(err)
		}
	}
	entries := probeEntries(t, r, fileParams{Paths: []string{
		"relative.txt", "~/x", filepath.Join(outside, "secret.txt"), filepath.Join(root, "leak.txt"),
		"", strings.Repeat("a", maxProbePathBytes+1), filepath.Join(root, "inside.txt"),
	}})
	reasons := []any{"relative", "home_unavailable", "forbidden", "forbidden", "invalid", "invalid", nil}
	if runtime.GOOS == "windows" {
		reasons[3] = nil
	}
	for index, reason := range reasons {
		if entries[index]["reason"] != reason {
			t.Fatalf("entry %d: %#v, want reason %v", index, entries[index], reason)
		}
		if exists := entries[index]["exists"].(bool); exists != (index == 6) {
			t.Fatalf("entry %d existence: %#v", index, entries[index])
		}
	}
	// A reference outside the roots must not disclose whether it exists.
	if entries[2]["type"] != nil || entries[2]["size"] != nil {
		t.Fatalf("forbidden entry leaked metadata: %#v", entries[2])
	}
}

func TestFileProbeBoundsAndCwdValidation(t *testing.T) {
	r, root := previewRuntime(t)
	for _, params := range []fileParams{
		{Action: "probe"},
		{Action: "probe", Paths: make([]string, maxProbePaths+1)},
		{Action: "probe", Paths: []string{"a"}, Cwd: "relative/cwd"},
	} {
		if _, err := r.file(params); err == nil {
			t.Fatalf("probe accepted %#v", params)
		}
	}
	if _, err := r.file(fileParams{Action: "probe", Paths: make([]string, maxProbePaths), Cwd: root}); err != nil {
		t.Fatal(err)
	}
}

func TestFileRevealResolvesOnlyExistingAuthorizedTargets(t *testing.T) {
	r, root := previewRuntime(t)
	file := filepath.Join(root, "report.html")
	if err := os.WriteFile(file, []byte("<p>"), 0600); err != nil {
		t.Fatal(err)
	}
	// The file manager itself starts outside runtime.file; only the
	// authorization and classification it depends on are exercised here.
	value, err := r.file(fileParams{Action: "reveal", Path: file})
	if err != nil || value.(map[string]any)["type"] != "file" {
		t.Fatal(value, err)
	}
	value, err = r.file(fileParams{Action: "reveal", Path: root})
	if err != nil || value.(map[string]any)["type"] != "directory" {
		t.Fatal(value, err)
	}
	if _, err = r.file(fileParams{Action: "reveal", Path: filepath.Join(root, "missing")}); err == nil {
		t.Fatal("reveal accepted a missing target")
	}
	if _, err = r.file(fileParams{Action: "reveal", Path: filepath.Join(t.TempDir(), "outside")}); err == nil {
		t.Fatal("reveal accepted a target outside the roots")
	}
	if _, err = revealResolvedFile(nil, executionRequest{}, map[string]any{}); err == nil {
		t.Fatal("reveal accepted an empty target")
	}
}

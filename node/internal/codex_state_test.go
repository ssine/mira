package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCodexSQLiteStateIsSeparateAndStable(t *testing.T) {
	t.Setenv("CODEX_SQLITE_HOME", "")
	dir := t.TempDir()
	identity := filepath.Join(dir, "identity.json")
	binary := filepath.Join(dir, "runtime", "codex")
	first, err := codexSQLiteOverride(identity, binary, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err := strconv.Unquote(strings.TrimPrefix(first, "sqlite_home="))
	if err != nil || !filepath.IsAbs(state) || !strings.HasPrefix(state, filepath.Join(dir, "state", "codex")+string(filepath.Separator)) {
		t.Fatalf("unexpected state override %q: %v", first, err)
	}
	second, err := codexSQLiteOverride(identity, binary, []string{`approval_policy="never"`})
	if err != nil || second != first {
		t.Fatalf("state location changed: %q, %v", second, err)
	}
	candidate, err := codexSQLiteOverride(identity, filepath.Join(dir, "candidate", "codex"), nil)
	if err != nil || candidate == first {
		t.Fatalf("candidate must have independent state: %q, %v", candidate, err)
	}
}

func TestCodexSQLiteStatePreservesExplicitOverrides(t *testing.T) {
	t.Setenv("CODEX_SQLITE_HOME", "")
	for _, overrides := range [][]string{{`sqlite_home="custom"`}, {` sqlite_home = "custom"`}} {
		got, err := codexSQLiteOverride("", "", overrides)
		if err != nil || got != "" {
			t.Fatalf("explicit config replaced: %q, %v", got, err)
		}
	}
	t.Setenv("CODEX_SQLITE_HOME", t.TempDir())
	got, err := codexSQLiteOverride("", "", nil)
	if err != nil || got != "" {
		t.Fatalf("explicit environment replaced: %q, %v", got, err)
	}
}

func codexStateDir(t *testing.T, override string) string {
	t.Helper()
	state, err := strconv.Unquote(strings.TrimPrefix(override, "sqlite_home="))
	if err != nil {
		t.Fatalf("override %q: %v", override, err)
	}
	return state
}

func TestCodexSQLiteStateKeysExplicitBinariesByTheBinary(t *testing.T) {
	t.Setenv("CODEX_SQLITE_HOME", "")
	t.Setenv("MIRA_NODE_CODEX_CACHE", "")
	dir := t.TempDir()
	identity := filepath.Join(dir, "identity.json")
	var pinned runtimeIdentity
	if err := json.Unmarshal(codexRuntimeLockJSON, &pinned); err != nil {
		t.Fatal(err)
	}
	// The pinned managed package keeps its runtime-version layout.
	managed := filepath.Join(dir, "runtimes", "codex", pinned.Version, "linux-amd64", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(managed), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managed, []byte("managed"), 0o700); err != nil {
		t.Fatal(err)
	}
	override, err := codexSQLiteOverride(identity, managed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(filepath.Dir(codexStateDir(t, override))); got != pinned.Version {
		t.Fatalf("managed runtime state moved: %s", got)
	}
	// An explicit binary is keyed by itself, not by the Node pin, and stays stable.
	explicit := filepath.Join(dir, "libexec", "codex")
	if err := os.MkdirAll(filepath.Dir(explicit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(explicit, []byte("explicit-v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := codexAccountSQLiteOverride(identity, explicit, "21dfcf17-1486-4e89-922a-b67a4fd644d9", nil)
	if err != nil {
		t.Fatal(err)
	}
	state := codexStateDir(t, first)
	key := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(state))))
	if !strings.HasPrefix(key, "binary-") {
		t.Fatalf("explicit binary must not use the Node pin: %s", state)
	}
	again, _ := codexAccountSQLiteOverride(identity, explicit, "21dfcf17-1486-4e89-922a-b67a4fd644d9", nil)
	if again != first {
		t.Fatalf("explicit state is not stable: %q vs %q", again, first)
	}
	// Replacing the binary at the same path gets independent state.
	later := time.Now().Add(time.Minute)
	if err := os.WriteFile(explicit, []byte("explicit-v2-longer"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(explicit, later, later); err != nil {
		t.Fatal(err)
	}
	replaced, _ := codexAccountSQLiteOverride(identity, explicit, "21dfcf17-1486-4e89-922a-b67a4fd644d9", nil)
	if replaced == first {
		t.Fatal("a replaced binary must not reuse migration state")
	}
}

func TestCodexSQLiteStateAdoptsPinKeyedStateOfTheSameBinary(t *testing.T) {
	t.Setenv("CODEX_SQLITE_HOME", "")
	t.Setenv("MIRA_NODE_CODEX_CACHE", "")
	dir := t.TempDir()
	identity := filepath.Join(dir, "identity.json")
	explicit := filepath.Join(dir, "libexec", "codex")
	if err := os.MkdirAll(filepath.Dir(explicit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(explicit, []byte("explicit"), 0o700); err != nil {
		t.Fatal(err)
	}
	built := time.Now().Add(-time.Hour)
	if err := os.Chtimes(explicit, built, built); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(explicit))
	location := hex.EncodeToString(digest[:12])
	write := func(version, name, content string, modified time.Time) {
		t.Helper()
		path := filepath.Join(dir, "state", "codex", version, location, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	write("0.1.0-mira.1", "state_5.sqlite", "stale-before-binary", built.Add(-time.Hour))
	write("0.2.0-mira.1", "state_5.sqlite", "older-run", built.Add(10*time.Minute))
	write("0.3.0-mira.1", "state_5.sqlite", "newest-run", built.Add(20*time.Minute))
	write("0.3.0-mira.1", "state_5.sqlite-wal", "newest-wal", built.Add(20*time.Minute))
	override, err := codexSQLiteOverride(identity, explicit, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := codexStateDir(t, override)
	data, err := os.ReadFile(filepath.Join(state, "state_5.sqlite"))
	if err != nil || string(data) != "newest-run" {
		t.Fatalf("expected the newest qualifying state: %q %v", data, err)
	}
	if wal, err := os.ReadFile(filepath.Join(state, "state_5.sqlite-wal")); err != nil || string(wal) != "newest-wal" {
		t.Fatalf("WAL must be adopted with its database: %q %v", wal, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "codex", "0.3.0-mira.1", location, "state_5.sqlite")); err != nil {
		t.Fatalf("source must be kept for rollback: %v", err)
	}
	// Existing state is never overwritten.
	if err := os.WriteFile(filepath.Join(state, "state_5.sqlite"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := codexSQLiteOverride(identity, explicit, nil); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(state, "state_5.sqlite")); string(data) != "live" {
		t.Fatalf("live state overwritten: %q", data)
	}

	// State written only before the current binary existed is not reused.
	other := filepath.Join(dir, "libexec2", "codex")
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("other"), 0o700); err != nil {
		t.Fatal(err)
	}
	otherDigest := sha256.Sum256([]byte(other))
	stalePath := filepath.Join(dir, "state", "codex", "0.3.0-mira.1", hex.EncodeToString(otherDigest[:12]), "state_5.sqlite")
	if err := os.MkdirAll(filepath.Dir(stalePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stalePath, []byte("from-a-previous-build"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stalePath, old, old); err != nil {
		t.Fatal(err)
	}
	otherOverride, _ := codexSQLiteOverride(identity, other, nil)
	if _, err := os.Stat(filepath.Join(codexStateDir(t, otherOverride), "state_5.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("state older than the binary must not be adopted: %v", err)
	}
}

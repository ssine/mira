package node

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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

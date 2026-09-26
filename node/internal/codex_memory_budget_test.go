package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexMemoryBudgetFormula(t *testing.T) {
	for _, test := range []struct{ total, expected uint64 }{
		{2 << 30, (2 << 30) / 5}, {4 << 30, (4 << 30) / 5}, {(4 << 30) + 1, (4 << 30) / 5},
		{8 << 30, (4 << 30) / 5}, {16 << 30, (16 << 30) / 10}, {64 << 30, (64 << 30) / 10},
	} {
		if actual := (codexMemoryBudget{}).resolve(test.total); actual != test.expected {
			t.Errorf("total %d: got %d want %d", test.total, actual, test.expected)
		}
	}
}

func TestCodexMemoryBudgetParsing(t *testing.T) {
	for _, test := range []struct {
		raw      string
		expected uint64
	}{
		{"", (16 << 30) / 10}, {" auto ", (16 << 30) / 10}, {"2GiB", 2 << 30},
		{"512 MiB", 512 << 20}, {"1.5GB", 1500000000}, {"20%", (16 << 30) / 5}, {"12.5%", 2 << 30}, {"100%", 16 << 30},
	} {
		budget, err := parseCodexMemoryBudget(test.raw)
		if err != nil || budget.resolve(16<<30) != test.expected {
			t.Errorf("%q: got %+v, %v", test.raw, budget, err)
		}
	}
	for _, raw := range []string{"-1GiB", "0B", "0%", "101%", "NaN%", "InfGiB", "NaNB", "2", "huge", "1e30GB"} {
		if _, err := parseCodexMemoryBudget(raw); err == nil {
			t.Errorf("accepted invalid budget %q", raw)
		}
	}
}

func TestCodexMemoryBudgetConfigPrecedence(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "node.json")
	if err := os.WriteFile(path, []byte(`{"serverUrl":"http://127.0.0.1:8765","codexMemoryBudget":"512MiB"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIRA_NODE_IDENTITY_FILE", filepath.Join(directory, "identity.json"))
	t.Setenv("MIRA_NODE_CODEX_MEMORY_BUDGET", "")
	configuration, err := loadConfigArgs([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if configuration.CodexMemoryBudget.resolve(8<<30) != 512<<20 {
		t.Fatal("file budget ignored")
	}
	t.Setenv("MIRA_NODE_CODEX_MEMORY_BUDGET", "25%")
	configuration, err = loadConfigArgs([]string{"--config", path})
	if err != nil || configuration.CodexMemoryBudget.resolve(8<<30) != 2<<30 {
		t.Fatalf("environment override failed: %v", err)
	}
	t.Setenv("MIRA_NODE_CODEX_MEMORY_BUDGET", "invalid")
	if _, err := loadConfigArgs([]string{"--config", path}); err == nil {
		t.Fatal("invalid budget silently accepted")
	}
}

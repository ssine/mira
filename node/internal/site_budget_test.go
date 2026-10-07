package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSiteBudgetFileAndEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(path, []byte(`{"siteStreamBudget":256}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIRA_NODE_SITE_STREAM_BUDGET", "")
	cfg, err := loadConfigArgs([]string{"--config", path})
	if err != nil || cfg.SiteStreamBudget != 256 {
		t.Fatalf("file budget: %d, %v", cfg.SiteStreamBudget, err)
	}
	t.Setenv("MIRA_NODE_SITE_STREAM_BUDGET", "512")
	cfg, err = loadConfigArgs([]string{"--config", path})
	if err != nil || cfg.SiteStreamBudget != 512 {
		t.Fatalf("environment budget: %d, %v", cfg.SiteStreamBudget, err)
	}
	t.Setenv("MIRA_NODE_SITE_STREAM_BUDGET", "invalid")
	if _, err := loadConfigArgs([]string{"--config", path}); err == nil {
		t.Fatal("invalid budget accepted")
	}
}

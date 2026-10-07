package foundation

import "testing"

func TestSiteBudgetEnvironment(t *testing.T) {
	for _, raw := range []string{"", "256", "invalid", "0"} {
		cfg, err := ConfigFromLookup(func(name string) (string, bool) {
			if name == "MIRA_NODE_SITE_STREAM_BUDGET" {
				return raw, true
			}
			return "", false
		})
		if raw == "invalid" || raw == "0" {
			if err == nil {
				t.Fatalf("invalid budget %q accepted", raw)
			}
			continue
		}
		want := 128
		if raw == "256" {
			want = 256
		}
		if err != nil || cfg.SiteStreamBudget != want {
			t.Fatalf("budget %q: %d, %v", raw, cfg.SiteStreamBudget, err)
		}
	}
}

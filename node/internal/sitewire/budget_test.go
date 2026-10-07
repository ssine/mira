package sitewire

import "testing"

func TestStreamBudgetConfiguration(t *testing.T) {
	for raw, want := range map[string]int{"": 128, "1": 1, "256": 256, "65536": 65536} {
		got, err := ParseStreamBudget(raw)
		if err != nil || got != want {
			t.Fatalf("budget %q = %d, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"0", "-1", "65537", "invalid", "1.5"} {
		if _, err := ParseStreamBudget(raw); err == nil {
			t.Fatalf("invalid budget %q accepted", raw)
		}
	}
}

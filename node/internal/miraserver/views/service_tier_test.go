package views

import "testing"

func TestServiceTierSnapshots(t *testing.T) {
	for _, tc := range []struct {
		item map[string]any
		want string
	}{
		{record("event_msg", map[string]any{"type": "thread_settings_applied", "thread_settings": map[string]any{"service_tier": "priority"}}), "priority"},
		{record("event_msg", map[string]any{"type": "thread_settings_applied", "thread_settings": map[string]any{"service_tier": nil}}), "default"},
		{record("event_msg", map[string]any{"type": "thread_settings_applied", "thread_settings": map[string]any{"model": "fixture"}}), "default"},
		{record("turn_context", map[string]any{"model": "fixture"}), ""},
		{record("response_item", map[string]any{"service_tier": "priority"}), ""},
	} {
		got := serviceTierFromRecord(tc.item)
		if tc.want == "" {
			if got != nil {
				t.Fatalf("unexpected tier %s", *got)
			}
		} else if got == nil || *got != tc.want {
			t.Fatalf("tier %v, want %s", got, tc.want)
		}
	}
}

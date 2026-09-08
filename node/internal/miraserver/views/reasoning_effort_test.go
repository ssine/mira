package views

import "testing"

func TestReasoningEffortFromRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		item map[string]any
		want string
	}{
		{"turn context", record("turn_context", map[string]any{"effort": "xhigh"}), "xhigh"},
		{"applied settings", record("event_msg", map[string]any{"type": "thread_settings_applied", "thread_settings": map[string]any{"reasoning_effort": "ultra"}}), "ultra"},
		{"future value", record("turn_context", map[string]any{"effort": "future"}), "future"},
		{"legacy context", record("turn_context", map[string]any{"model": "test"}), ""},
		{"blank", record("turn_context", map[string]any{"effort": " "}), ""},
		{"unrelated payload", record("response_item", map[string]any{"effort": "low"}), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reasoningEffortFromRecord(tc.item)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got %q, want nil", *got)
				}
			} else if got == nil || *got != tc.want {
				t.Fatalf("got %v, want %q", got, tc.want)
			}
		})
	}
}

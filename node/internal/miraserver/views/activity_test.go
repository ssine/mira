package views

import (
	"reflect"
	"testing"
	"time"
)

func activityRow(sequence int64, payload map[string]any) ActivityRow {
	return ActivityRow{ItemSeq: &sequence, Payload: record("event_msg", payload)}
}

func TestProjectThreadActivityLifecycle(t *testing.T) {
	rows := []ActivityRow{activityRow(4, map[string]any{"type": "task_complete", "turn_id": "new", "completed_at": float64(1788602412)}), activityRow(3, map[string]any{"type": "task_started", "turn_id": "new", "started_at": float64(1788602400)}), activityRow(2, map[string]any{"type": "task_complete", "turn_id": "old"}), activityRow(1, map[string]any{"type": "task_started", "turn_id": "old"})}
	activity := ProjectThreadActivity(rows, true)
	if activity["state"] != "idle" || activity["turnId"] != "new" || activity["startedItemSeq"] != int64(3) || activity["startedAt"] != "2026-09-05T10:00:00.000Z" {
		t.Fatalf("activity: %#v", activity)
	}
	running := ProjectThreadActivity(rows[1:], true)
	if running["state"] != "running" || running["turnId"] != "new" {
		t.Fatalf("running: %#v", running)
	}
	unknown := ProjectThreadActivity(nil, false)
	if !reflect.DeepEqual(unknown, map[string]any{"state": "unknown", "turnId": nil, "reason": "history"}) {
		t.Fatalf("unknown: %#v", unknown)
	}
}

func TestProjectThreadActivityErrorsAndAborts(t *testing.T) {
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	rows := []ActivityRow{{ItemSeq: int64Pointer(3), Payload: record("event_msg", map[string]any{"type": "error", "turn_id": "turn", "codex_error_info": "other"}), CreatedAt: &now}, activityRow(2, map[string]any{"type": "task_started", "turn_id": "turn"})}
	if state := ProjectThreadActivity(rows, true)["state"]; state != "failed" {
		t.Fatalf("state %v", state)
	}
	rows[0].Payload = record("event_msg", map[string]any{"type": "error", "turn_id": "turn", "will_retry": true})
	if state := ProjectThreadActivity(rows, true)["state"]; state != "running" {
		t.Fatalf("retry state %v", state)
	}
	aborted := ProjectThreadActivity([]ActivityRow{activityRow(2, map[string]any{"type": "turn_aborted", "turn_id": "turn"}), activityRow(1, map[string]any{"type": "turn_started", "turn_id": "turn"})}, true)
	if aborted["state"] != "interrupted" {
		t.Fatalf("abort: %#v", aborted)
	}
}

func int64Pointer(value int64) *int64 { return &value }

func TestTokenUsageModelAndReadProjection(t *testing.T) {
	usage := NormalizeTokenUsage(map[string]any{"input_tokens": 10, "cached_input_tokens": 8, "output_tokens": 1})
	if !reflect.DeepEqual(usage, map[string]any{"inputTokens": int64(10), "cachedInputTokens": int64(8), "outputTokens": int64(1)}) {
		t.Fatalf("usage: %#v", usage)
	}
	partial := NormalizeTokenUsage(map[string]any{"input_tokens": "bad", "output_tokens": 1})
	if partial["inputTokens"] != nil || partial["cachedInputTokens"] != nil || partial["outputTokens"] != int64(1) {
		t.Fatalf("partial: %#v", partial)
	}
	model := "gpt-6-astra"
	if found := modelFromRecord(record("turn_context", map[string]any{"model": model})); found == nil || *found != model {
		t.Fatalf("model: %v", found)
	}
	if !VisibleAssistantUpdate(record("response_item", map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Done"}}})) {
		t.Fatal("assistant update not visible")
	}
	if VisibleAssistantUpdate(record("response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"text": "Request"}}})) {
		t.Fatal("user message is not assistant update")
	}
}

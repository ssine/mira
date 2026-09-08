package views

import (
	"reflect"
	"testing"
)

func record(recordType string, payload map[string]any) map[string]any {
	return map[string]any{"type": recordType, "payload": payload}
}

func TestProjectTranscriptSeparatesImageSnapshot(t *testing.T) {
	dataURL := "data:image/png;base64,iVBORw0KGgo="
	records := []map[string]any{
		record("event_msg", map[string]any{"type": "task_started", "turn_id": "image-turn"}),
		record("response_item", map[string]any{"type": "function_call", "name": "view_image", "call_id": "image-1", "arguments": `{"path":"/tmp/chart.png"}`}),
		record("event_msg", map[string]any{"type": "item_completed", "turn_id": "image-turn", "item": map[string]any{"type": "ImageView", "id": "image-1", "path": "file:///tmp/chart.png"}}),
		record("response_item", map[string]any{"type": "function_call_output", "call_id": "image-1", "output": []any{map[string]any{"type": "input_image", "image_url": dataURL}}}),
	}
	trace := ProjectCodexTranscript(records, ProjectionOptions{Fragments: true})
	if len(trace) != 2 || trace[0]["kind"] != "tool" || trace[0]["title"] != "查看图片" {
		t.Fatalf("unexpected trace: %#v", trace)
	}
	if trace[0]["images"] != nil || trace[1]["kind"] != "image" || trace[1]["sourceItemSeq"] != int64(4) || !reflect.DeepEqual(trace[1]["image"], map[string]any{"url": dataURL}) {
		t.Fatalf("image must belong to its canonical output record: %#v", trace)
	}

	if body := stringValue(trace[0]["body"]); contains(body, "base64") {
		t.Fatalf("image leaked into body")
	}
	tail := ProjectCodexTranscript(records[3:], ProjectionOptions{InitialTurnID: "image-turn", ItemOffset: 3, Fragments: true})
	if len(tail) != 2 || tail[0]["key"] != trace[0]["key"] || tail[1]["key"] != trace[1]["key"] {
		t.Fatalf("tail: %#v", tail)
	}
}

func TestProjectTranscriptMixedFormatsAndDeduplicatesNarrative(t *testing.T) {
	old, resumed := "old", "resumed"
	items := []map[string]any{
		record("event_msg", map[string]any{"type": "task_started", "turn_id": old}),
		record("event_msg", map[string]any{"type": "item_completed", "turn_id": old, "item": map[string]any{"id": "old-user", "type": "UserMessage", "content": []any{map[string]any{"type": "text", "text": "Old request"}}}}),
		record("event_msg", map[string]any{"type": "item_completed", "turn_id": old, "item": map[string]any{"id": "old-agent", "type": "AgentMessage", "content": []any{map[string]any{"type": "text", "text": "Old response"}}}}),
		record("event_msg", map[string]any{"type": "task_started", "turn_id": resumed}),
		record("response_item", map[string]any{"id": "environment", "role": "user", "type": "message", "content": []any{map[string]any{"text": "hidden"}}, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": resumed, "content_item_kinds": []any{"environment_context"}}}),
		record("response_item", map[string]any{"id": "new-user", "role": "user", "type": "message", "content": []any{map[string]any{"text": "New request"}}, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": resumed, "content_item_kinds": []any{"user.text"}}}),
		record("event_msg", map[string]any{"type": "user_message", "message": "New request"}),
		record("event_msg", map[string]any{"type": "agent_message", "phase": "final_answer", "message": "New response"}),
		record("response_item", map[string]any{"id": "new-agent", "role": "assistant", "type": "message", "phase": "final_answer", "content": []any{map[string]any{"text": "New response"}}, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": resumed}}),
	}
	trace := ProjectCodexTranscript(items, ProjectionOptions{})
	bodies := []string{}
	for _, entry := range trace {
		bodies = append(bodies, stringValue(entry["body"]))
	}
	if !reflect.DeepEqual(bodies, []string{"Old request", "Old response", "New request", "New response"}) {
		t.Fatalf("bodies: %#v", bodies)
	}
}

func TestProjectTranscriptTimingAndErrors(t *testing.T) {
	turnID := "turn"
	items := []map[string]any{
		{"timestamp": "2026-09-05T10:00:00.000Z", "type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": turnID}},
		{"timestamp": "2026-09-05T10:00:00.120Z", "type": "event_msg", "payload": map[string]any{"type": "user_message", "turn_id": turnID, "message": "Start"}},
		{"timestamp": "2026-09-05T10:00:06.250Z", "type": "event_msg", "payload": map[string]any{"type": "agent_message", "turn_id": turnID, "message": "Finished"}},
		{"timestamp": "2026-09-05T10:00:06.400Z", "type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": turnID}},
	}
	trace := ProjectCodexTranscript(items, ProjectionOptions{})
	if trace[0]["completedAt"] != "2026-09-05T10:00:00.120Z" || trace[0]["elapsedMs"] != int64(120) || trace[1]["turnElapsedMs"] != int64(6400) {
		t.Fatalf("timing: %#v", trace)
	}
	errorItems := []map[string]any{{"timestamp": "2026-09-05T11:00:00.000Z", "type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": turnID}}, {"timestamp": "2026-09-05T11:00:00.400Z", "type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": turnID, "error": map[string]any{"message": `{"error":{"message":"Upgrade Codex"}}`}}}}
	errors := ProjectCodexTranscript(errorItems, ProjectionOptions{})
	if len(errors) != 1 || errors[0]["kind"] != "error" || errors[0]["body"] != "Upgrade Codex" {
		t.Fatalf("error: %#v", errors)
	}
}

func TestPaginateTranscript(t *testing.T) {
	trace := make([]map[string]any, 125)
	for index := range trace {
		trace[index] = map[string]any{"key": index}
	}
	page := PaginateCodexTranscript(trace, nil, 60)
	items := page["trace"].([]map[string]any)
	if items[0]["key"] != 65 || page["nextCursor"] != "65" || page["totalTraceItems"] != 125 {
		t.Fatalf("page: %#v", page)
	}
}

func contains(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}

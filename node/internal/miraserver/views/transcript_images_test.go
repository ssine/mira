package views

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestHistoryImagesOnlyUsesTypedModelContent(t *testing.T) {
	image := map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"}
	content := []any{map[string]any{"type": "input_text", "text": "before"}, image, image}
	for _, kind := range []string{"function_call_output", "custom_tool_call_output", "message"} {
		payload := map[string]any{"type": kind, "role": "user", "output": content, "content": content}
		got := historyImages(record("response_item", payload))
		want := []historyImage{{1, image["image_url"].(string)}, {2, image["image_url"].(string)}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %#v", kind, got)
		}
	}
	raw, _ := json.Marshal(content)
	for _, input := range []map[string]any{
		record("event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "ImageView", "path": "/tmp/image.png", "output": content}}),
		record("response_item", map[string]any{"type": "function_call", "arguments": map[string]any{"path": "/tmp/image.png"}, "output": content}),
		record("response_item", map[string]any{"type": "function_call_output", "output": string(raw)}),
		record("response_item", map[string]any{"type": "function_call_output", "output": []any{map[string]any{"type": "input_text", "text": string(raw)}}}),
		record("response_item", map[string]any{"type": "message", "role": "developer", "content": content}),
		record("response_item", map[string]any{"type": "message", "role": "user", "content": content, "internal_chat_message_metadata_passthrough": map[string]any{"content_item_kinds": []any{"environment_context"}}}),
	} {
		if got := historyImages(input); len(got) != 0 {
			t.Fatalf("non-image content produced images: %#v", input)
		}
	}
}

func TestHistoryImageOrderSurvivesCallPairingAndSummary(t *testing.T) {
	output := func(id string) map[string]any {
		return record("response_item", map[string]any{"type": "function_call_output", "call_id": id, "output": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"}}})
	}
	records := []map[string]any{
		record("response_item", map[string]any{"type": "function_call", "call_id": "a", "name": "view_image"}),
		record("response_item", map[string]any{"type": "function_call", "call_id": "b", "name": "view_image"}),
		output("b"),
		record("response_item", map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Between pictures"}}}),
		output("a"),
	}
	trace := ProjectCodexTranscript(records, ProjectionOptions{Fragments: true})
	var keys []any
	for _, item := range trace {
		if item["kind"] == "image" {
			keys = append(keys, item["key"])
		}
	}
	if !reflect.DeepEqual(keys, []any{"history-3-image-0", "history-5-image-0"}) {
		t.Fatalf("order: %#v", trace)
	}
	referenceTranscriptImages(trace, "personal", "thread", 7)
	for _, loaded := range []bool{false, true} {
		summary := transcriptToolDetails(trace, "cursor", 60, loaded)
		encoded, _ := json.Marshal(summary)
		if contains(string(encoded), "base64") {
			t.Fatal("summary must contain references, not image bytes")
		}
		for _, item := range summary {
			if item["kind"] == "image" && !contains(stringValue(object(item["image"])["href"]), "generation=7") {
				t.Fatalf("missing generation reference: %#v", item)
			}
			if item["kind"] == "tool" && item["images"] != nil {
				t.Fatal("tool must not own images")
			}
		}
	}
}

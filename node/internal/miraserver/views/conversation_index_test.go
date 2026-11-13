package views

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// outlineSkeleton is the comparison form of a transcript without tool details:
// what the Web renders before a tool card is expanded.
func outlineSkeleton(trace []map[string]any) []map[string]any {
	referenceTranscriptImages(trace, "personal", "thread", 1)
	result := transcriptToolDetails(trace, "", 0, false)
	normalized := make([]map[string]any, len(result))
	for index, item := range result {
		encoded, _ := json.Marshal(item)
		var copy map[string]any
		_ = json.Unmarshal(encoded, &copy)
		delete(copy, "toolDetail")
		normalized[index] = copy
	}
	return normalized
}

// projectFromIndex runs the same projection over slim, sparse index records.
func projectFromIndex(raw []map[string]any, recordedAt map[int64]string) ([]map[string]any, []map[string]any) {
	carry := codexIndexCarry{}
	items, sequences := []map[string]any{}, []int64{}
	slimRecorded := map[int64]string{}
	for index, record := range raw {
		slim, _ := slimCodexRecord(record, &carry)
		if slim == nil {
			continue
		}
		// Round-trip like the database column does.
		encoded, err := json.Marshal(slim)
		if err != nil {
			panic(err)
		}
		decoded, err := decodeObject(encoded)
		if err != nil {
			panic(err)
		}
		sequence := int64(index + 1)
		items, sequences = append(items, decoded), append(sequences, sequence)
		if value, ok := recordedAt[sequence]; ok {
			slimRecorded[sequence] = value
		}
	}
	return ProjectCodexTranscript(items, ProjectionOptions{Fragments: true, RecordedAt: slimRecorded, Sequences: sequences}), items
}

func assertOutlineEquivalent(t *testing.T, raw []map[string]any, recordedAt map[int64]string) []map[string]any {
	t.Helper()
	normalizedRaw := make([]map[string]any, len(raw))
	for index, record := range raw {
		encoded, _ := json.Marshal(record)
		normalizedRaw[index], _ = decodeObject(encoded)
	}
	full := ProjectCodexTranscript(normalizedRaw, ProjectionOptions{Fragments: true, RecordedAt: recordedAt})
	indexed, _ := projectFromIndex(normalizedRaw, recordedAt)
	left, right := outlineSkeleton(full), outlineSkeleton(indexed)
	if !reflect.DeepEqual(left, right) {
		for index := 0; index < max(len(left), len(right)); index++ {
			var a, b map[string]any
			if index < len(left) {
				a = left[index]
			}
			if index < len(right) {
				b = right[index]
			}
			if !reflect.DeepEqual(a, b) {
				encodedA, _ := json.Marshal(a)
				encodedB, _ := json.Marshal(b)
				t.Fatalf("outline differs at %d (full %d, index %d)\nfull:  %.2000s\nindex: %.2000s", index, len(left), len(right), encodedA, encodedB)
			}
		}
	}
	return left
}

func TestConversationIndexOutlineMatchesFullProjection(t *testing.T) {
	bigOutput := strings.Repeat("output line\n", 50000)
	patch := "*** Begin Patch\n*** Update File: /repo/a.go\n@@\n-old\n+new\n*** Add File: /repo/b.go\n+package b\n*** End Patch\n"
	image := "data:image/png;base64," + strings.Repeat("iVBORw0KGgo", 2000)
	raw := []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": "thread", "instructions": strings.Repeat("x", 100000)}},
		{"type": "turn_context", "payload": map[string]any{"turn_id": "t1", "cwd": "/repo", "user_instructions": strings.Repeat("rules ", 10000), "model": "gpt"}},
		record("event_msg", map[string]any{"type": "task_started", "turn_id": "t1", "started_at": float64(1788602400)}),
		record("response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<environment_context>cwd</environment_context>"}},
			"internal_chat_message_metadata_passthrough": map[string]any{"content_item_kinds": []any{"environment_context"}}}),
		record("response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "修复 bug 并查看截图"}, map[string]any{"type": "input_image", "image_url": image}}}),
		record("event_msg", map[string]any{"type": "user_message", "turn_id": "t1", "message": "修复 bug 并查看截图", "images": []any{image}}),
		record("event_msg", map[string]any{"type": "token_count", "info": map[string]any{"last_token_usage": map[string]any{"input_tokens": float64(12000), "total_tokens": float64(12100)}}}),
		record("response_item", map[string]any{"type": "reasoning", "id": "rs1", "summary": []any{map[string]any{"type": "summary_text", "text": "**Planning** the fix"}}, "encrypted_content": strings.Repeat("e", 40000)}),
		record("event_msg", map[string]any{"type": "agent_reasoning", "turn_id": "t1", "text": "**Planning** the fix"}),
		record("response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": "c1", "arguments": `{"cmd":"go test ./...","workdir":"/repo"}`}),
		record("response_item", map[string]any{"type": "function_call_output", "call_id": "c1", "output": bigOutput}),
		record("response_item", map[string]any{"type": "custom_tool_call", "name": "apply_patch", "call_id": "c2", "input": patch}),
		record("response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "c2", "output": `{"output":"Success","metadata":{"exit_code":0}}`}),
		record("event_msg", map[string]any{"type": "item_completed", "turn_id": "t1", "item": map[string]any{"type": "mcpToolCall", "id": "m1", "server": "home_nodes", "tool": "file",
			"arguments": map[string]any{"path": "/x", "content": strings.Repeat("y", 20000)}, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": bigOutput}}}, "status": "completed"}}),
		record("event_msg", map[string]any{"type": "item_completed", "turn_id": "t1", "item": map[string]any{"type": "commandExecution", "id": "cmd2", "command": "ls -la", "cwd": "/repo",
			"aggregatedOutput": bigOutput, "exitCode": float64(0), "status": "completed", "durationMs": float64(1200)}}),
		record("response_item", map[string]any{"type": "function_call", "name": "view_image", "call_id": "c3", "arguments": `{"path":"/repo/shot.png"}`}),
		record("response_item", map[string]any{"type": "function_call_output", "call_id": "c3", "output": []any{map[string]any{"type": "input_image", "image_url": image}, map[string]any{"type": "input_text", "text": "image"}}}),
		record("event_msg", map[string]any{"type": "view_image_tool_call", "call_id": "c3", "path": "/repo/shot.png"}),
		record("response_item", map[string]any{"type": "web_search_call", "id": "ws1", "status": "completed", "action": map[string]any{"type": "search", "query": "pg_trgm"}}),
		record("response_item", map[string]any{"type": "function_call", "name": "orphan", "call_id": "c4", "arguments": "{}"}),
		record("response_item", map[string]any{"type": "function_call_output", "call_id": "missing", "output": "orphan output"}),
		record("response_item", map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "已修复。"}}}),
		record("event_msg", map[string]any{"type": "agent_message", "turn_id": "t1", "message": "已修复。", "phase": "final_answer"}),
		record("event_msg", map[string]any{"type": "task_complete", "turn_id": "t1", "completed_at": float64(1788602460), "last_agent_message": strings.Repeat("z", 50000), "duration_ms": float64(60000)}),
		{"type": "world_state", "payload": map[string]any{"blob": strings.Repeat("w", 10000)}},
		{"type": "turn_context", "payload": map[string]any{"turn_id": "t2", "cwd": "/repo"}},
		record("event_msg", map[string]any{"type": "task_started", "turn_id": "t2"}),
		record("event_msg", map[string]any{"type": "user_message", "turn_id": "t2", "message": "继续"}),
		record("event_msg", map[string]any{"type": "token_count", "info": map[string]any{"last_token_usage": map[string]any{"input_tokens": float64(250000), "total_tokens": float64(251000)}}}),
		{"type": "compacted", "payload": map[string]any{"message": "Summary of earlier work", "replacement_history": []any{strings.Repeat("h", 80000)}}},
		record("event_msg", map[string]any{"type": "token_count", "info": map[string]any{"last_token_usage": map[string]any{"input_tokens": float64(0), "total_tokens": float64(9000)}}}),
		record("event_msg", map[string]any{"type": "agent_message", "turn_id": "t2", "message": "Summary of earlier work"}),
		record("event_msg", map[string]any{"type": "turn_aborted", "turn_id": "t2", "error": map[string]any{"message": "interrupted"}}),
	}
	recordedAt := map[int64]string{}
	base := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	for index := range raw {
		recordedAt[int64(index+1)] = formatTime(base.Add(time.Duration(index) * time.Second))
	}
	skeleton := assertOutlineEquivalent(t, raw, recordedAt)
	kinds := map[string]int{}
	for _, item := range skeleton {
		kinds[stringValue(item["kind"])]++
	}
	if kinds["user"] != 2 || kinds["assistant"] < 1 || kinds["tool"] < 5 || kinds["image"] < 1 || kinds["compaction"] != 1 {
		t.Fatalf("fixture does not exercise the projection: %v", kinds)
	}
	for _, item := range skeleton {
		if item["kind"] == "compaction" && !strings.Contains(stringValue(item["body"]), "250,000") {
			t.Fatalf("compaction lost the preceding token counter: %v", item["body"])
		}
	}
	_, slim := projectFromIndex(raw, recordedAt)
	encoded, _ := json.Marshal(slim)
	if len(encoded) > 200*1024 {
		t.Fatalf("slim records retain bulky payloads: %d bytes", len(encoded))
	}
}

func TestConversationIndexMessagesAndCarry(t *testing.T) {
	carry := codexIndexCarry{}
	if slim, message := slimCodexRecord(record("event_msg", map[string]any{"type": "user_message", "message": "a\x00b"}), &carry); slim == nil || message == nil || message.role != "user" || message.body != "ab" {
		t.Fatalf("user message: %#v %#v", slim, message)
	}
	if _, message := slimCodexRecord(record("event_msg", map[string]any{"type": "agent_message", "message": "done"}), &carry); message == nil || message.role != "assistant" {
		t.Fatalf("agent message: %#v", message)
	}
	if slim, _ := slimCodexRecord(record("event_msg", map[string]any{"type": "token_count", "info": map[string]any{"last_token_usage": map[string]any{"input_tokens": float64(7), "total_tokens": float64(9)}}}), &carry); slim != nil || carry.LastInputTokens != 7 {
		t.Fatalf("token counters are carried, not stored: %#v %#v", slim, carry)
	}
	long := strings.Repeat("长", indexMessageLimit)
	if _, message := slimCodexRecord(record("event_msg", map[string]any{"type": "user_message", "message": long}), &carry); len(message.body) > indexMessageLimit+4 || !strings.HasSuffix(message.body, "…") {
		t.Fatalf("message body bound: %d", len(message.body))
	}
	if value := truncateStrings("你好世界", 4).(string); value != "你…" {
		t.Fatalf("truncation must keep UTF-8 boundaries: %q", value)
	}
}

func TestSearchTermsAndSnippets(t *testing.T) {
	terms := SearchTerms("  对话   搜索 对话 SEARCH search ")
	if fmt.Sprint(terms) != "[对话 搜索 SEARCH]" {
		t.Fatalf("terms: %v", terms)
	}
	if pattern := likePattern(`50%_a\b`); pattern != `%50\%\_a\\b%` {
		t.Fatalf("pattern: %s", pattern)
	}
	snippet := searchSnippet(strings.Repeat("前文", 60)+"\n这里是 Mira 的全文搜索功能"+strings.Repeat("后文", 60), []string{"全文搜索"})
	if !strings.Contains(snippet, "全文搜索") || !strings.HasPrefix(snippet, "…") || !strings.HasSuffix(snippet, "…") || strings.Contains(snippet, "\n") {
		t.Fatalf("snippet: %q", snippet)
	}
}

func TestCodexTimelineLane(t *testing.T) {
	at := func(seconds int) *time.Time {
		value := time.Date(2026, 11, 14, 10, 0, seconds, 0, time.UTC)
		return &value
	}
	records := []outlineRecord{
		{1, record("event_msg", map[string]any{"type": "task_started", "turn_id": "t1"}), at(0)},
		{2, record("event_msg", map[string]any{"type": "user_message", "turn_id": "t1", "message": "run tests"}), at(0)},
		{3, record("response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": "c1", "arguments": "{}"}), at(5)},
		{4, record("response_item", map[string]any{"type": "function_call", "name": "wait_agent", "call_id": "c2", "arguments": "{}"}), at(6)},
		{5, record("response_item", map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"}), at(20)},
		{6, record("event_msg", map[string]any{"type": "item_completed", "turn_id": "t1", "item": map[string]any{"type": "mcpToolCall", "id": "m1", "tool": "file", "durationMs": float64(3000), "status": "completed"}}), at(30)},
		{7, record("event_msg", map[string]any{"type": "agent_message", "turn_id": "t1", "message": "done"}), at(40)},
		{8, record("event_msg", map[string]any{"type": "task_complete", "turn_id": "t1"}), at(41)},
		{9, record("event_msg", map[string]any{"type": "task_started", "turn_id": "t2"}), at(3600)},
		{10, record("response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": "c3", "arguments": "{}"}), at(3605)},
	}
	turns, tools, messages := codexTimelineLane(records)
	if len(turns) != 2 || turns[0].Open || turns[0].End-turns[0].Start != 41000 || !turns[1].Open || turns[1].End != at(3605).UnixMilli() {
		t.Fatalf("turns: %#v", turns)
	}
	if len(tools) != 4 || tools[0].End-tools[0].Start != 15000 || tools[1].Kind != "wait" || tools[1].End != at(41).UnixMilli() ||
		tools[2].End-tools[2].Start != 3000 || tools[3].End != tools[3].Start {
		t.Fatalf("tools: %#v", tools)
	}
	if len(messages) != 2 || messages[0].Role != "user" || messages[0].Seq != 2 || messages[1].Role != "assistant" {
		t.Fatalf("messages: %#v", messages)
	}
}

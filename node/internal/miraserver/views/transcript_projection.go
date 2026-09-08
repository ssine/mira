package views

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

func isoTimestamp(value any, numericScale int64) string {
	if numeric, ok := number(value); ok {
		return formatTime(time.UnixMilli(int64(numeric * float64(numericScale))))
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return ""
	}
	return formatTime(parsed)
}

func recordTimestamp(record map[string]any) string {
	payload := object(record["payload"])
	for _, candidate := range []struct {
		value any
		scale int64
	}{
		{payload["completed_at_ms"], 1}, {payload["completed_at"], 1000},
		{object(payload["item"])["completedAt"], 1}, {first(record["timestamp"], payload["timestamp"]), 1},
	} {
		if result := isoTimestamp(candidate.value, candidate.scale); result != "" {
			return result
		}
	}
	return ""
}

func turnStartedAt(record map[string]any) string {
	payload := object(record["payload"])
	if result := isoTimestamp(payload["started_at"], 1000); result != "" {
		return result
	}
	if result := isoTimestamp(payload["started_at_ms"], 1); result != "" {
		return result
	}
	if includes(activityStarts, stringValue(payload["type"])) {
		return recordTimestamp(record)
	}
	return ""
}

func elapsedMilliseconds(startedAt, completedAt string) any {
	start, startOK := parseISO(startedAt)
	end, endOK := parseISO(completedAt)
	if !startOK || !endOK || end.Before(start) {
		return nil
	}
	return end.Sub(start).Milliseconds()
}

func reasoningParts(item map[string]any) []string {
	parts := first(item["summary"], item["summary_text"], item["text"])
	values := array(parts)
	if values == nil {
		values = []any{parts}
	}
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = valueText(value)
	}
	return result
}

func reasoningText(item map[string]any) string {
	parts := []string{}
	for _, part := range reasoningParts(item) {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func visibleResponseMessage(payload map[string]any) bool {
	role := stringValue(payload["role"])
	if role != "user" && role != "assistant" {
		return false
	}
	if role == "assistant" {
		return true
	}
	kinds := array(object(payload["internal_chat_message_metadata_passthrough"])["content_item_kinds"])
	if len(kinds) == 0 {
		return true
	}
	for _, kind := range kinds {
		if strings.HasPrefix(fmt.Sprint(kind), "user.") {
			return true
		}
	}
	return false
}

func projectedMaterializedItem(item map[string]any, itemSeq int64, index int, turnID string) map[string]any {
	if item == nil {
		return nil
	}
	id := item["id"]
	keyPart := fmt.Sprint(index)
	if id != nil {
		keyPart = fmt.Sprint(id)
	}
	var turn any
	if turnID != "" {
		turn = turnID
	}
	base := map[string]any{"key": fmt.Sprintf("history-%d-%s", itemSeq, keyPart), "turnId": turn, "sourceItemSeq": itemSeq, "itemId": id, "status": first(item["status"], "")}
	if tool := ToolItemView(item); tool != nil {
		for key, value := range tool {
			base[key] = value
		}
		base["body"] = boundedText(tool["body"])
		return base
	}
	typeName := normalizedType(item["type"])
	switch typeName {
	case "usermessage":
		base["kind"], base["title"], base["markdown"], base["body"] = "user", "你", true, boundedText(valueText(item["content"]))
	case "agentmessage":
		base["kind"], base["title"], base["markdown"], base["body"], base["phase"] = "assistant", "Codex", true, boundedText(valueText(first(item["content"], item["text"]))), item["phase"]
	case "reasoning":
		parts := reasoningParts(item)
		summaries := make([]any, len(parts))
		for index, part := range parts {
			summaries[index] = boundedText(part)
		}
		base["kind"], base["title"], base["markdown"], base["body"], base["summaryParts"] = "reasoning", "推理摘要", true, boundedText(reasoningText(item)), summaries
	case "plan":
		base["kind"], base["title"], base["markdown"], base["body"] = "reasoning", "计划", true, boundedText(valueText(first(item["text"], item["plan"], item["content"])))
	case "contextcompaction":
		base["kind"], base["title"], base["markdown"], base["body"] = "compaction", "上下文自动压缩", false, "较早的上下文已自动压缩。"
	case "websearch", "imageview", "imagegeneration", "subagentactivity", "functioncalloutput":
		base["kind"], base["title"], base["markdown"], base["body"] = "tool", normalizedToolTitle(item), false, boundedText(printable(item))
	default:
		return nil
	}
	return base
}

type responseTool struct {
	callID           string
	isCall, isOutput bool
	input, output    any
	entry            map[string]any
}

func responseToolCall(payload map[string]any, itemSeq int64, index int, turnID string) *responseTool {
	typeName := stringValue(payload["type"])
	isCall := includes([]string{"custom_tool_call", "function_call", "local_shell_call", "tool_search_call", "web_search_call"}, typeName)
	isOutput := includes([]string{"custom_tool_call_output", "function_call_output", "tool_search_output"}, typeName)
	if !isCall && !isOutput {
		return nil
	}
	callID := stringValue(first(payload["call_id"], payload["id"]))
	if callID == "" {
		callID = fmt.Sprintf("response-%d-%d", itemSeq, index)
	}
	var turn any
	turnPart := "unscoped"
	if turnID != "" {
		turn, turnPart = turnID, turnID
	}
	title := "工具输出"
	status := any("完成")
	if isCall {
		title = normalizedToolTitle(payload)
		status = first(payload["status"], "运行")
	}
	return &responseTool{callID: callID, isCall: isCall, isOutput: isOutput,
		input: first(payload["input"], payload["arguments"], payload["command"], payload["query"]), output: first(payload["output"], payload["result"]),
		entry: map[string]any{"key": "history-tool-" + turnPart + "-" + callID, "turnId": turn, "sourceItemSeq": itemSeq, "itemId": callID,
			"kind": "tool", "title": title, "markdown": false, "status": status, "body": ""}}
}

func callKey(turnID, id string) string {
	payload, _ := json.Marshal([]any{func() any {
		if turnID == "" {
			return nil
		}
		return turnID
	}(), id})
	return string(payload)
}

type turnTiming struct {
	startedAt, completedAt       string
	startedApproximate, finished bool
	elapsed                      any
}
type toolState struct {
	entry, materialized, payload map[string]any
	input, output                any
	hasInput, hasOutput          bool
}

// ProjectCodexTranscript builds the stable Web trace without mutating records.
func ProjectCodexTranscript(items []map[string]any, options ProjectionOptions) []map[string]any {
	records := items
	responseCalls := map[string]bool{}
	materializedTools := map[string]map[string]any{}
	turnTimings := map[string]*turnTiming{}
	scannedTurnID := options.InitialTurnID
	if scannedTurnID != "" && options.InitialTurnStartedAt != "" {
		turnTimings[scannedTurnID] = &turnTiming{startedAt: options.InitialTurnStartedAt, startedApproximate: options.InitialTurnStartedApproximate}
	}
	all := append(append([]map[string]any{}, records...), options.TimingRecords...)
	for index, record := range all {
		payload := object(record["payload"])
		if stringValue(record["type"]) == "turn_context" || stringValue(record["type"]) == "event_msg" && includes(activityStarts, stringValue(payload["type"])) {
			if id := stringValue(payload["turn_id"]); id != "" {
				scannedTurnID = id
			}
		}
		turnID := stringValue(first(payload["turn_id"], object(payload["internal_chat_message_metadata_passthrough"])["turn_id"]))
		if turnID == "" {
			turnID = scannedTurnID
		}
		if turnID != "" {
			timing := turnTimings[turnID]
			if timing == nil {
				timing = &turnTiming{}
			}
			if stringValue(record["type"]) == "turn_context" && timing.startedAt == "" {
				timing.startedAt = recordTimestamp(record)
				if timing.startedAt == "" {
					timing.startedAt = options.RecordedAt[options.ItemOffset+int64(index)+1]
				}
				timing.startedApproximate = timing.startedAt != ""
			}
			if stringValue(record["type"]) == "event_msg" && includes(activityStarts, stringValue(payload["type"])) {
				if started := turnStartedAt(record); started != "" {
					timing.startedAt, timing.startedApproximate = started, false
				}
			}
			if stringValue(record["type"]) == "event_msg" && includes(activityEnds, stringValue(payload["type"])) {
				if started := turnStartedAt(record); started != "" {
					timing.startedAt, timing.startedApproximate = started, false
				}
				timing.completedAt, timing.finished = recordTimestamp(record), true
				if duration, ok := number(payload["duration_ms"]); ok && duration >= 0 {
					timing.elapsed = duration
				}
			}
			turnTimings[turnID] = timing
		}
		itemSeq := options.ItemOffset + int64(index) + 1
		if stringValue(record["type"]) == "response_item" {
			if tool := responseToolCall(payload, itemSeq, index, turnID); tool != nil && (tool.isCall || options.Fragments && tool.isOutput) {
				responseCalls[callKey(turnID, tool.callID)] = true
			}
		}
		if stringValue(record["type"]) == "event_msg" {
			var item map[string]any
			if stringValue(payload["type"]) == "view_image_tool_call" {
				item = map[string]any{"type": "imageView", "id": payload["call_id"], "path": payload["path"]}
			} else if stringValue(payload["type"]) == "item_completed" {
				item = object(payload["item"])
			}
			if item != nil && stringValue(item["id"]) != "" {
				if entry := projectedMaterializedItem(item, itemSeq, index, turnID); stringValue(entry["kind"]) == "tool" {
					materializedTools[callKey(turnID, stringValue(item["id"]))] = entry
				}
			}
		}
	}

	trace := []map[string]any{}
	projectedByID := map[string]map[string]any{}
	projectedNarratives := map[string]map[string]any{}
	toolCalls := map[string]*toolState{}
	currentTurnID := options.InitialTurnID

	withTiming := func(entry map[string]any) map[string]any { return entry }
	withTiming = func(entry map[string]any) map[string]any {
		if entry == nil {
			return nil
		}
		turnID := stringValue(entry["turnId"])
		timing := turnTimings[turnID]
		itemClock := stringValue(entry["completedAt"])
		sequence, _ := safeInteger(entry["sourceItemSeq"])
		index := sequence - options.ItemOffset - 1
		if itemClock == "" && index >= 0 && index < int64(len(records)) {
			itemClock = recordTimestamp(records[index])
		}
		recordedAt := options.RecordedAt[sequence]
		completedAt := itemClock
		if completedAt == "" && includes([]string{"assistant", "user"}, stringValue(entry["kind"])) {
			completedAt = recordedAt
			if completedAt == "" && timing != nil {
				completedAt = timing.completedAt
			}
		}
		if completedAt != "" {
			entry["completedAt"] = completedAt
		}
		if itemClock == "" && completedAt != "" {
			if recordedAt != "" {
				entry["timingScope"] = "recorded"
			} else {
				entry["timingScope"] = "turn"
			}
		}
		if stringValue(entry["kind"]) == "assistant" && timing != nil && timing.finished {
			entry["turnCompletedAt"] = func() any {
				if timing.completedAt == "" {
					return nil
				}
				return timing.completedAt
			}()
			if timing.elapsed != nil {
				entry["turnElapsedMs"] = timing.elapsed
			} else {
				entry["turnElapsedMs"] = elapsedMilliseconds(timing.startedAt, timing.completedAt)
			}
			entry["turnElapsedApproximate"] = timing.elapsed == nil && timing.startedApproximate
		}
		if includes([]string{"user", "assistant"}, stringValue(entry["kind"])) && entry["elapsedMs"] == nil {
			if itemClock == "" && timing != nil && timing.elapsed != nil {
				entry["elapsedMs"] = timing.elapsed
			} else if timing != nil {
				entry["elapsedMs"] = elapsedMilliseconds(timing.startedAt, completedAt)
			} else {
				entry["elapsedMs"] = nil
			}
			if timing != nil && timing.startedApproximate && (itemClock != "" || timing.elapsed == nil) {
				entry["elapsedApproximate"] = true
			}
		}
		return entry
	}

	push := func(entry map[string]any) {}
	push = func(entry map[string]any) {
		entry = withTiming(entry)
		if entry == nil || stringValue(entry["body"]) == "" && !includes([]string{"tool", "image"}, stringValue(entry["kind"])) {
			return
		}
		if includes([]string{"user", "assistant", "reasoning"}, stringValue(entry["kind"])) {
			signatureBytes, _ := json.Marshal([]any{entry["turnId"], entry["kind"], entry["phase"], entry["body"]})
			signature := string(signatureBytes)
			if previous := projectedNarratives[signature]; previous != nil {
				previousSeq, _ := safeInteger(previous["sourceItemSeq"])
				entrySeq, _ := safeInteger(entry["sourceItemSeq"])
				if entry["turnId"] != nil || entrySeq-previousSeq <= 3 {
					if previous["completedAt"] == nil && entry["completedAt"] != nil || previous["timingScope"] != nil && entry["timingScope"] == nil && entry["completedAt"] != nil {
						for _, key := range []string{"completedAt", "elapsedMs", "timingScope", "elapsedApproximate"} {
							if value, ok := entry[key]; ok {
								previous[key] = value
							} else {
								delete(previous, key)
							}
						}
					}
					return
				}
			}
			projectedNarratives[signature] = entry
		}
		key := stringValue(entry["key"])
		if existing := projectedByID[key]; existing != nil {
			for field, value := range entry {
				existing[field] = value
			}
		} else {
			trace = append(trace, entry)
			projectedByID[key] = entry
		}
	}

	for index, record := range records {
		payload := object(record["payload"])
		itemSeq := options.ItemOffset + int64(index) + 1
		recordTurnID := stringValue(first(payload["turn_id"], object(payload["internal_chat_message_metadata_passthrough"])["turn_id"]))
		if recordTurnID == "" {
			recordTurnID = currentTurnID
		}
		recordType := stringValue(record["type"])
		if recordType == "turn_context" {
			if id := stringValue(payload["turn_id"]); id != "" {
				currentTurnID = id
			}
			continue
		}
		if recordType == "compacted" {
			push(map[string]any{"key": fmt.Sprintf("history-%d-compaction", itemSeq), "turnId": nullableString(recordTurnID), "sourceItemSeq": itemSeq, "kind": "compaction", "title": "上下文自动压缩", "markdown": false, "body": "较早的上下文已自动压缩。"})
			continue
		}
		if recordType == "event_msg" && includes(activityStarts, stringValue(payload["type"])) {
			if id := stringValue(payload["turn_id"]); id != "" {
				currentTurnID = id
			}
			continue
		}
		if recordType == "event_msg" && includes(activityEnds, stringValue(payload["type"])) {
			if id := stringValue(payload["turn_id"]); id != "" {
				currentTurnID = id
			}
			if failure := projectedErrorMessage(payload["error"], 0); failure != "" {
				push(map[string]any{"key": fmt.Sprintf("history-%d-turn-error", itemSeq), "turnId": nullableString(currentTurnID), "sourceItemSeq": itemSeq, "kind": "error", "title": "Turn 失败", "markdown": false, "status": "失败", "body": boundedText(failure)})
			}
			continue
		}
		if recordType == "event_msg" && includes([]string{"item_completed", "view_image_tool_call"}, stringValue(payload["type"])) {
			var item map[string]any
			if stringValue(payload["type"]) == "view_image_tool_call" {
				item = map[string]any{"type": "imageView", "id": payload["call_id"], "path": payload["path"]}
			} else {
				item = object(payload["item"])
			}
			materializedTurnID := stringValue(payload["turn_id"])
			if materializedTurnID == "" {
				materializedTurnID = recordTurnID
			}
			key := callKey(materializedTurnID, stringValue(item["id"]))
			if !(materializedTools[key] != nil && responseCalls[key]) {
				entry := projectedMaterializedItem(item, itemSeq, index, materializedTurnID)
				if entry != nil && options.Fragments && stringValue(entry["kind"]) == "tool" && entry["itemId"] != nil {
					entry["key"] = "history-tool-" + func() string {
						if materializedTurnID == "" {
							return "unscoped"
						}
						return materializedTurnID
					}() + "-" + fmt.Sprint(entry["itemId"])
					if options.Fragments {
						entry["toolFragment"] = map[string]any{"materialized": true}
					}
				}
				push(entry)
			}
			continue
		}
		if recordType == "event_msg" {
			switch stringValue(payload["type"]) {
			case "user_message":
				push(map[string]any{"key": fmt.Sprintf("history-%d-user", itemSeq), "turnId": nullableString(recordTurnID), "sourceItemSeq": itemSeq, "kind": "user", "title": "你", "markdown": true, "status": "", "body": boundedText(first(payload["message"], valueText(payload)))})
			case "agent_message":
				push(map[string]any{"key": fmt.Sprintf("history-%d-agent", itemSeq), "turnId": nullableString(recordTurnID), "sourceItemSeq": itemSeq, "kind": "assistant", "title": "Codex", "markdown": true, "status": "", "phase": payload["phase"], "body": boundedText(first(payload["message"], valueText(payload)))})
			case "agent_reasoning":
				push(map[string]any{"key": fmt.Sprintf("history-%d-reasoning", itemSeq), "turnId": nullableString(recordTurnID), "sourceItemSeq": itemSeq, "kind": "reasoning", "title": "推理摘要", "markdown": true, "status": "", "body": boundedText(first(payload["text"], valueText(payload)))})
			}
		}
		if recordType != "response_item" {
			continue
		}
		for _, image := range historyImages(record) {
			push(map[string]any{"key": fmt.Sprintf("history-%d-image-%d", itemSeq, image.index),
				"kind": "image", "title": "图片", "body": "", "turnId": nullableString(recordTurnID),
				"sourceItemSeq": itemSeq, "imageIndex": image.index, "image": map[string]any{"url": image.url}})
		}
		if tool := responseToolCall(payload, itemSeq, index, recordTurnID); tool != nil {
			key := callKey(recordTurnID, tool.callID)
			state := toolCalls[key]
			if state == nil {
				materialized := materializedTools[key]
				entry := cloneMap(tool.entry)
				if materialized != nil {
					for field, value := range materialized {
						entry[field] = value
					}
				}
				entry["key"], entry["sourceItemSeq"] = tool.entry["key"], itemSeq
				entry = withTiming(entry)
				state = &toolState{entry: entry, materialized: materialized}
				toolCalls[key] = state
				trace = append(trace, entry)
				projectedByID[stringValue(entry["key"])] = entry
			}
			if tool.isCall {
				state.input, state.hasInput, state.payload = tool.input, true, payload
				if state.materialized == nil {
					state.entry["title"], state.entry["status"] = tool.entry["title"], first(payload["status"], state.entry["status"])
				}
				old, _ := safeInteger(state.entry["sourceItemSeq"])
				if itemSeq < old {
					state.entry["sourceItemSeq"] = itemSeq
				}
			}
			if tool.isOutput {
				state.output, state.hasOutput = tool.output, true
				if state.materialized == nil {
					state.entry["status"] = "完成"
				}
			}
			if state.materialized == nil {
				if view := ResponseToolView(state.payload, stringValue(state.entry["status"])); view != nil {
					state.entry["activity"] = view["activity"]
				}
			}
			if body := stringValue(state.materialized["body"]); body != "" {
				state.entry["body"] = body
			} else {
				state.entry["body"] = toolBody(state.input, state.hasInput, state.output, state.hasOutput)
			}
			if options.Fragments {
				if state.materialized != nil {
					state.entry["toolFragment"] = map[string]any{"materialized": true}
				} else {
					fragment := map[string]any{"input": nil, "output": nil}
					if state.hasInput {
						fragment["input"] = boundedText(printable(state.input))
					}
					if state.hasOutput {
						fragment["output"] = boundedText(printable(state.output))
					}
					state.entry["toolFragment"] = fragment
				}
			}
			continue
		}
		if stringValue(payload["type"]) == "reasoning" {
			parts := reasoningParts(payload)
			summaries := make([]any, len(parts))
			for i, p := range parts {
				summaries[i] = boundedText(p)
			}
			push(map[string]any{"key": fmt.Sprintf("history-%d-%s", itemSeq, first(payload["id"], "reasoning")), "turnId": nullableString(recordTurnID), "sourceItemSeq": itemSeq, "itemId": payload["id"], "summaryParts": summaries, "kind": "reasoning", "title": "推理摘要", "markdown": true, "status": "", "body": boundedText(reasoningText(payload))})
		}
		if stringValue(payload["type"]) == "message" && visibleResponseMessage(payload) {
			role := stringValue(payload["role"])
			kind, title := "assistant", "Codex"
			if role == "user" {
				kind, title = "user", "你"
			}
			push(map[string]any{"key": fmt.Sprintf("history-%d-%s", itemSeq, first(payload["id"], role)), "turnId": nullableString(recordTurnID), "sourceItemSeq": itemSeq, "kind": kind, "title": title, "markdown": true, "status": "", "phase": payload["phase"], "body": boundedText(valueText(payload["content"]))})
		}
	}
	result := []map[string]any{}
	for _, entry := range trace {
		if stringValue(entry["body"]) != "" || entry["activity"] != nil || stringValue(entry["kind"]) != "tool" {
			result = append(result, entry)
		}
	}
	// Tool calls can be paired across distant records. Image positions always
	// belong to the actual model-facing content record, never to the call.
	sort.SliceStable(result, func(i, j int) bool {
		left, _ := safeInteger(result[i]["sourceItemSeq"])
		right, _ := safeInteger(result[j]["sourceItemSeq"])
		if left != right {
			return left < right
		}
		return result[i]["kind"] != "image" && result[j]["kind"] == "image"
	})
	return result
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func toolBody(input any, hasInput bool, output any, hasOutput bool) string {
	parts := []string{}
	if hasInput {
		if text := printable(input); text != "" {
			parts = append(parts, "输入\n"+text)
		}
	}
	if hasOutput {
		if text := printable(output); text != "" {
			parts = append(parts, "输出\n"+text)
		}
	}
	return boundedText(strings.Join(parts, "\n\n"))
}

// PaginateCodexTranscript pages a fully projected legacy transcript backwards.
func PaginateCodexTranscript(trace []map[string]any, cursor *int, limit int) map[string]any {
	end := len(trace)
	if cursor != nil && *cursor < end {
		end = *cursor
	}
	if end < 0 {
		end = 0
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	var next any
	if start > 0 {
		next = fmt.Sprint(start)
	}
	return map[string]any{"trace": trace[start:end], "nextCursor": next, "totalTraceItems": len(trace)}
}

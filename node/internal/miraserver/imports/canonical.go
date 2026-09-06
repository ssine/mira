package imports

import (
	"encoding/json"
	"fmt"
	"time"
)

// CanonicalRolloutItem converts a source rollout envelope to the representation
// supported by Mira's legacy ThreadStore API. Unknown payload fields retain
// their original JSON representation; the immutable source line remains in the
// import provenance table.
func CanonicalRolloutItem(raw json.RawMessage, clearCopiedFork bool, childThreadID string) (json.RawMessage, error) {
	envelope, err := rawObject(raw)
	if err != nil {
		return nil, fmt.Errorf("decode rollout envelope: %w", err)
	}
	var kind string
	if err := json.Unmarshal(envelope["type"], &kind); err != nil {
		return nil, fmt.Errorf("rollout envelope has no type")
	}
	payloadRaw, ok := envelope["payload"]
	if !ok {
		return nil, fmt.Errorf("rollout envelope has no payload")
	}
	if kind == "session_meta" {
		payload, err := rawObject(payloadRaw)
		if err != nil {
			return nil, fmt.Errorf("decode session metadata: %w", err)
		}
		if source, found := payload["base_instructions"]; found {
			payload["base_instructions"] = canonicalBaseInstructions(source)
		}
		payload["history_mode"] = rawString("legacy")
		var id string
		_ = json.Unmarshal(payload["id"], &id)
		if clearCopiedFork && id == childThreadID {
			payload["history_base"] = json.RawMessage("null")
			payload["forked_from_ordinal_exclusive"] = json.RawMessage("null")
			payload["subagent_history_start_ordinal"] = json.RawMessage("null")
		}
		payloadRaw, err = marshalRawObject(payload)
		if err != nil {
			return nil, err
		}
	}
	return marshalRawObject(map[string]json.RawMessage{
		"type":    rawString(kind),
		"payload": payloadRaw,
	})
}

func canonicalBaseInstructions(raw json.RawMessage) json.RawMessage {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		encoded, _ := marshalRawObject(map[string]json.RawMessage{"text": append(json.RawMessage(nil), raw...)})
		return encoded
	}
	if object, err := rawObject(raw); err == nil {
		if value, found := object["text"]; found && json.Unmarshal(value, &text) == nil {
			return append(json.RawMessage(nil), raw...)
		}
	}
	encoded, _ := marshalRawObject(map[string]json.RawMessage{"text": rawString("")})
	return encoded
}

func baseInstructions(value any) map[string]any {
	if text, ok := value.(string); ok {
		return map[string]any{"text": text}
	}
	if candidate := object(value); candidate != nil {
		if _, ok := candidate["text"].(string); ok {
			return candidate
		}
	}
	return map[string]any{"text": ""}
}

func createdThread(meta map[string]any, threadID string) map[string]any {
	sessionID := stringValue(meta["session_id"])
	if !validUUID(sessionID) {
		sessionID = threadID
	}
	windowID := stringValue(object(meta["context_window"])["window_id"])
	if !validUUID(windowID) {
		windowID = threadID
	}
	memoryMode := "enabled"
	if stringValue(meta["memory_mode"]) == "disabled" {
		memoryMode = "disabled"
	}
	provider := stringValue(meta["model_provider"])
	if provider == "" {
		provider = "openai"
	}
	originator := stringValue(meta["originator"])
	if originator == "" {
		originator = "Codex"
	}
	source := firstNonNil(meta["source"], "cli")
	return map[string]any{
		"source":                         source,
		"metadata":                       map[string]any{"cwd": nullableString(meta["cwd"]), "memory_mode": memoryMode, "model_provider": provider},
		"thread_id":                      threadID,
		"originator":                     originator,
		"session_id":                     sessionID,
		"extra_config":                   nil,
		"history_base":                   meta["history_base"],
		"history_mode":                   "legacy",
		"dynamic_tools":                  arrayOrEmpty(meta["dynamic_tools"]),
		"selected_capability_roots":      arrayOrEmpty(meta["selected_capability_roots"]),
		"thread_source":                  meta["thread_source"],
		"forked_from_id":                 validUUIDOrNil(meta["forked_from_id"]),
		"parent_thread_id":               validUUIDOrNil(meta["parent_thread_id"]),
		"base_instructions":              baseInstructions(meta["base_instructions"]),
		"initial_window_id":              windowID,
		"multi_agent_version":            meta["multi_agent_version"],
		"subagent_history_start_ordinal": safeIntegerOrNil(meta["subagent_history_start_ordinal"]),
	}
}

func metadataPatch(meta, summary map[string]any) map[string]any {
	var firstUserMessage any
	if value, ok := summary["title"].(string); ok {
		firstUserMessage = value
	}
	provider := stringValue(meta["model_provider"])
	if provider == "" {
		provider = "openai"
	}
	memoryMode := "enabled"
	if stringValue(meta["memory_mode"]) == "disabled" {
		memoryMode = "disabled"
	}
	createdAt := dateOrNil(firstNonNil(summary["startedAt"], meta["timestamp"]))
	updatedAt := dateOrNil(summary["modifiedAt"])
	return map[string]any{
		"preview":            firstUserMessage,
		"title":              firstUserMessage,
		"model_provider":     provider,
		"created_at":         createdAt,
		"updated_at":         updatedAt,
		"advance_recency_at": updatedAt,
		"source":             firstNonNil(meta["source"], "cli"),
		"cwd":                nullableString(meta["cwd"]),
		"cli_version":        nullableString(meta["cli_version"]),
		"first_user_message": firstUserMessage,
		"memory_mode":        memoryMode,
	}
}

func nullableString(value any) any {
	if result, ok := value.(string); ok {
		return result
	}
	return nil
}

func arrayOrEmpty(value any) []any {
	if result, ok := value.([]any); ok {
		return result
	}
	return []any{}
}

func validUUIDOrNil(value any) any {
	if result, ok := value.(string); ok && validUUID(result) {
		return result
	}
	return nil
}

func safeIntegerOrNil(value any) any {
	if result, ok := integer(value); ok {
		return result
	}
	return nil
}

func dateOrNil(value any) any {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return nil
	}
	return parsed.UTC().Format("2006-01-02T15:04:05.000Z")
}

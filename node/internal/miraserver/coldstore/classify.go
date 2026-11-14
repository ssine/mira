package coldstore

import (
	"encoding/json"
	"regexp"
)

// metadataPattern is the union of every PostgreSQL text predicate that a
// metadata view (cost, token usage, model, lifecycle, read state) uses to pick
// candidate rows. A row whose text matches stays hot, so those views read the
// same rows with the same bytes before and after freezing.
var metadataPattern = regexp.MustCompile(`"type"[[:space:]]*:[[:space:]]*"(session_meta|turn_context|thread_settings_applied|token_count|task_started|turn_started|task_complete|turn_complete|turn_aborted|error|agent_message|item_completed)"`)

var hotEventTypes = map[string]bool{
	"token_count": true, "thread_settings_applied": true, "task_started": true, "task_complete": true,
	"turn_started": true, "turn_complete": true, "turn_aborted": true, "error": true,
	"agent_message": true, "item_completed": true,
}

type recordShape struct {
	Type      string          `json:"type"`
	Timestamp json.RawMessage `json:"timestamp"`
	Payload   *struct {
		Type string `json:"type"`
		Role string `json:"role"`
	} `json:"payload"`
}

// Placeholder is stored in the hot row instead of a frozen payload. It keeps
// every field that SQL filters on (record type, payload type and role) and the
// record timestamp, plus the content address of the canonical bytes.
type Placeholder struct {
	Type      string            `json:"type"`
	Timestamp json.RawMessage   `json:"timestamp,omitempty"`
	Payload   map[string]string `json:"payload"`
	Cold      PlaceholderRef    `json:"mira_cold_payload"`
}

type PlaceholderRef struct {
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
	Bytes   int    `json:"bytes"`
}

// Freezable reports whether a canonical record may move to cold storage and
// returns its placeholder. Only display-oriented bodies (tool calls and
// outputs, reasoning, compaction history, non-assistant messages and large
// event bodies) qualify; anything a metadata view inspects stays hot.
func Freezable(raw []byte) ([]byte, string, bool) {
	if metadataPattern.Match(raw) {
		return nil, "", false
	}
	var shape recordShape
	if err := json.Unmarshal(raw, &shape); err != nil {
		return nil, "", false
	}
	payloadType, role := "", ""
	if shape.Payload != nil {
		payloadType, role = shape.Payload.Type, shape.Payload.Role
	}
	switch shape.Type {
	case "response_item":
		if payloadType == "" || (payloadType == "message" && role == "assistant") {
			return nil, "", false
		}
	case "event_msg":
		if payloadType == "" || hotEventTypes[payloadType] {
			return nil, "", false
		}
	case "compacted":
	default:
		return nil, "", false
	}
	sha := Digest(raw)
	placeholder := Placeholder{Type: shape.Type, Payload: map[string]string{}, Cold: PlaceholderRef{Version: 1, SHA256: sha, Bytes: len(raw)}}
	if len(shape.Timestamp) > 0 && len(shape.Timestamp) <= 64 && shape.Timestamp[0] == '"' {
		placeholder.Timestamp = shape.Timestamp
	}
	if payloadType != "" {
		placeholder.Payload["type"] = payloadType
	}
	if role != "" && len(role) <= 32 {
		placeholder.Payload["role"] = role
	}
	stub, err := json.Marshal(placeholder)
	if err != nil || metadataPattern.Match(stub) {
		return nil, "", false
	}
	return stub, sha, true
}

package coldstore

import (
	"encoding/json"
)

// claudeKeptFields are the top-level Claude SDK/transcript fields that SQL or
// cheap Go filters inspect without hydrating (root-view omission, transcript
// timestamps, entry identity). They stay in the placeholder when they are small
// JSON scalars.
var claudeKeptFields = []string{"type", "subtype", "parent_tool_use_id", "timestamp", "uuid", "parentUuid", "session_id", "isSidechain", "agentId"}

// ClaudeFreezable reports whether a Claude event or transcript entry may move
// to cold storage and returns its placeholder. Only "user" records qualify:
// they carry tool results (file contents, command output, screenshots), the
// bulk of Claude history. Assistant prose, results, usage and lifecycle
// records always stay hot.
func ClaudeFreezable(raw []byte) ([]byte, string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, "", false
	}
	var recordType string
	if json.Unmarshal(fields["type"], &recordType) != nil || recordType != "user" {
		return nil, "", false
	}
	sha := Digest(raw)
	placeholder := map[string]any{}
	for _, name := range claudeKeptFields {
		value, ok := fields[name]
		if !ok || len(value) == 0 || len(value) > 256 || value[0] == '{' || value[0] == '[' {
			continue
		}
		placeholder[name] = value
	}
	placeholder["mira_cold_payload"] = PlaceholderRef{Version: 1, SHA256: sha, Bytes: len(raw)}
	stub, err := json.Marshal(placeholder)
	if err != nil {
		return nil, "", false
	}
	return stub, sha, true
}

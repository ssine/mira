package coldstore

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestClaudeFreezable(t *testing.T) {
	big := string(bytes.Repeat([]byte("a"), 1000))
	cases := []struct {
		name, record string
		freeze       bool
	}{
		{"user tool result", `{"type":"user","uuid":"e1","timestamp":"2026-10-01T00:00:00Z","parentUuid":"p","session_id":"s","message":{"role":"user","content":[{"type":"tool_result","content":"` + big + `"}]}}`, true},
		{"assistant", `{"type":"assistant","uuid":"e2","message":{"role":"assistant","content":[]}}`, false},
		{"result", `{"type":"result","total_cost_usd":1.2}`, false},
		{"system", `{"type":"system","subtype":"init"}`, false},
		{"stream", `{"type":"stream_event","event":{}}`, false},
		{"not json", `{"type":`, false},
	}
	for _, item := range cases {
		stub, sha, ok := ClaudeFreezable([]byte(item.record))
		if ok != item.freeze {
			t.Fatalf("%s: freeze=%v want %v", item.name, ok, item.freeze)
		}
		if !ok {
			continue
		}
		if sha != Digest([]byte(item.record)) {
			t.Fatalf("%s: wrong content address", item.name)
		}
		var pl map[string]any
		if err := json.Unmarshal(stub, &pl); err != nil {
			t.Fatalf("%s: placeholder not JSON: %v", item.name, err)
		}
		// Fields the cheap Go/SQL filters inspect survive verbatim.
		var orig map[string]json.RawMessage
		if err := json.Unmarshal([]byte(item.record), &orig); err != nil {
			t.Fatal(err)
		}
		var kept map[string]json.RawMessage
		_ = json.Unmarshal(stub, &kept)
		for _, key := range []string{"type", "uuid", "timestamp", "parentUuid", "session_id"} {
			if raw, ok := orig[key]; ok && string(kept[key]) != string(raw) {
				t.Fatalf("%s: placeholder lost %s", item.name, key)
			}
		}
		if pl["mira_cold_payload"] == nil {
			t.Fatalf("%s: missing reference", item.name)
		}
		if len(stub) > 400 {
			t.Fatalf("%s: placeholder is %d bytes", item.name, len(stub))
		}
		// The bulk fields are gone.
		if bytes.Contains(stub, []byte(big)) {
			t.Fatalf("%s: placeholder kept the body", item.name)
		}
	}
}

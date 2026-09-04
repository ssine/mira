package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexSessionsListAndChunkedRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("HOME", home)
	sessionDirectory := filepath.Join(home, "sessions", "2026", "09", "04")
	if err := os.MkdirAll(sessionDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	pathValue := filepath.Join(sessionDirectory, "rollout-2026-09-04-test.jsonl")
	lines := []map[string]any{
		{"timestamp": "2026-09-04T01:02:03Z", "type": "session_meta", "payload": map[string]any{
			"id": "01a0693d-114e-76b1-a994-0a673fe124a2", "cwd": home,
			"source": "cli", "cli_version": "0.152.1",
		}},
		{"timestamp": "2026-09-04T01:02:04Z", "type": "event_msg", "payload": map[string]any{
			"type": "user_message", "message": "Please inspect this session",
		}},
	}
	contents := ""
	for _, line := range lines {
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		contents += string(encoded) + "\n"
	}
	if err := os.WriteFile(pathValue, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	runtimeValue, err := newCapabilityRuntime(config{AllowedRoots: []string{home}})
	if err != nil {
		t.Fatal(err)
	}
	listedRaw, err := runtimeValue.codexSessions(codexSessionsParams{Action: "list"})
	if err != nil {
		t.Fatal(err)
	}
	listed := listedRaw.(map[string]any)["sessions"].([]codexSessionSummary)
	if len(listed) != 1 || listed[0].ThreadID != "01a0693d-114e-76b1-a994-0a673fe124a2" ||
		listed[0].Title != "Please inspect this session" {
		t.Fatalf("unexpected session summary: %#v", listed)
	}
	readRaw, err := runtimeValue.codexSessions(codexSessionsParams{
		Action: "read", Path: pathValue, Limit: len(contents),
	})
	if err != nil {
		t.Fatal(err)
	}
	read := readRaw.(map[string]any)
	if read["content"] != contents || read["eof"] != true || read["nextCursor"] != int64(len(contents)) {
		t.Fatalf("unexpected session chunk: %#v", read)
	}
	outside := filepath.Join(t.TempDir(), "rollout-outside.jsonl")
	if err := os.WriteFile(outside, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeValue.codexSessions(codexSessionsParams{Action: "read", Path: outside}); err == nil {
		t.Fatal("read outside detected Codex homes succeeded")
	}
}

package node

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveImmutableRolloutInSourceHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, dir := range []string{"sessions", "archived_sessions"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	id := "01a065c7-297e-7b53-890e-b2551c7d27f2"
	// Current Codex may preserve the logical thread ID and suffix the
	// referenced immutable rollout ID after an underscore.
	name := "rollout-test-01a0623f-a78b-7260-a419-461af25d1dcd_" + id + ".jsonl"
	source := filepath.Join(home, "sessions", name)
	data := []byte(`{"type":"session_meta","payload":{"id":"01a0623f-a78b-7260-a419-461af25d1dcd"}}` + "\n")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := newCapabilityRuntime(config{AllowedRoots: []string{home}})
	if err != nil {
		t.Fatal(err)
	}
	params := codexSessionsParams{Action: "resolve", Path: source, RolloutID: id}
	result, err := r.codexSessions(params)
	if err != nil || result.(codexSessionSummary).Path != source {
		t.Fatalf("resolve: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "archived_sessions", name), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.codexSessions(params); err == nil {
		t.Fatal("ambiguous rollout accepted")
	}
	params.RolloutID = "../../outside"
	if _, err := r.codexSessions(params); err == nil {
		t.Fatal("invalid rollout ID accepted")
	}
}

func TestCodexSessionsListAndChunkedRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("HOME", home)
	// os.UserHomeDir uses USERPROFILE on Windows. Never scan real user history.
	t.Setenv("USERPROFILE", home)
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
		t.Fatalf("unexpected fixture session summary (count=%d)", len(listed))
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

func TestDesktopArchivedSessionAndBinaryChunks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	directory := filepath.Join(home, "archived_sessions")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "rollout-desktop.jsonl")
	data := []byte(`{"type":"session_meta","payload":{"id":"01a065c7-297e-7b53-890e-b2551c7d27f2","originator":"Codex Desktop","source":"vscode","history_mode":"paginated","cwd":"C:\\project"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"userMessage","content":[{"type":"text","text":"桌面标题"}]}}}` + "\n")
	// A single long record can cross any number of transport chunks.
	data = append(data, []byte(`{"type":"future_item","payload":{"text":"`)...)
	data = append(data, bytes.Repeat([]byte("x"), 9*1024*1024)...)
	data = append(data, []byte("\"}}\n")...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := newCapabilityRuntime(config{AllowedRoots: []string{home}})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := r.listCodexSessions()
	if err != nil {
		t.Fatal(err)
	}
	sessions := listed.(map[string]any)["sessions"].([]codexSessionSummary)
	if len(sessions) != 1 || sessions[0].ClientKind != "desktop" || !sessions[0].Archived || sessions[0].Title != "桌面标题" {
		t.Fatalf("desktop metadata not discovered: %#v", sessions)
	}
	var restored []byte
	for cursor := int64(0); cursor < int64(len(data)); {
		chunk, err := r.readCodexSession(codexSessionsParams{Path: path, Cursor: cursor, Limit: 1024 * 1024, Encoding: "base64"})
		if err != nil {
			t.Fatal(err)
		}
		value := chunk.(map[string]any)
		decoded, err := base64.StdEncoding.DecodeString(value["content"].(string))
		if err != nil {
			t.Fatal(err)
		}
		restored = append(restored, decoded...)
		cursor = value["nextCursor"].(int64)
	}
	if !bytes.Equal(data, restored) {
		t.Fatal("binary chunks changed source JSONL")
	}
}

func TestFileUploadChunksAndOffsetConflict(t *testing.T) {
	home := t.TempDir()
	r, err := newCapabilityRuntime(config{AllowedRoots: []string{home}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "uploaded.bin")
	no := false
	chunk := bytes.Repeat([]byte("a"), 3*1024*1024)
	params := fileParams{Action: "write", Path: path, Content: string(chunk), Overwrite: &no}
	if _, err := r.file(params); err != nil {
		t.Fatal(err)
	}
	params.Append = true
	params.Offset = int64(len(chunk))
	if _, err := r.file(params); err != nil {
		t.Fatal(err)
	}
	if _, err := r.file(params); err == nil {
		t.Fatal("duplicate chunk should fail without corrupting data")
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, append(chunk, chunk...)) {
		t.Fatal("chunked upload content mismatch")
	}
}

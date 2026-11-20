package node

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConversationReadPagesMergesToolsAndPrints(t *testing.T) {
	id := "019a0000-0000-7000-8000-000000000001"
	queries := []string{}
	all := []map[string]any{
		{"seq": 3, "kind": "user", "text": "问题", "recordedAt": "2026-11-21T01:00:00Z"},
		{"seq": 5, "kind": "tool", "tool": map[string]any{"key": "call-1", "name": "exec_command", "status": "运行", "input": "ls ```", "inputBytes": 6}},
		// The output arrives on the next page under the same key.
		{"seq": 6, "kind": "tool", "tool": map[string]any{"key": "call-1", "status": "完成", "output": "a.txt", "outputBytes": 9000, "outputTruncated": true, "hasOutput": true}},
		{"seq": 7, "kind": "assistant", "text": "回答"},
		{"seq": 9, "kind": "user", "text": "追问"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/conversations/"+id+"/messages" || r.Header.Get("X-Mira-Client-Type") != "cli" {
			http.NotFound(w, r)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		page := []map[string]any{}
		for _, item := range all {
			if item["seq"].(int) > after && len(page) < 2 {
				page = append(page, item)
			}
		}
		var next any
		if len(page) == 2 && page[1]["seq"].(int) < 9 {
			next = page[1]["seq"]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"engine": "codex", "threadId": id, "title": "设计讨论", "complete": true, "tools": true,
			"children": []any{map[string]any{"threadId": "child-1", "title": "子任务"}}, "items": page, "nextCursor": next})
	}))
	defer server.Close()
	client := &cliClient{identity: &persistedNodeState{ServerURL: server.URL, Token: "test-only-token"},
		options: cliOptions{Timeout: 5 * time.Second}, http: &http.Client{Timeout: 5 * time.Second}}

	result, err := client.runConversation(context.Background(), []string{"read", strings.ToUpper(id), "--tool-output-limit", "4096"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 3 || !strings.Contains(queries[0], "tools=1") || !strings.Contains(queries[0], "toolOutputLimit=4096") || !strings.Contains(queries[1], "after=5") {
		t.Fatalf("queries: %v", queries)
	}
	read := result.(conversationText).conversationRead
	if len(read.Items) != 4 || read.Items[1].Tool.Name != "exec_command" || read.Items[1].Tool.Output != "a.txt" || read.Items[1].Tool.Status != "完成" {
		t.Fatalf("merged items: %#v", read.Items)
	}
	var out bytes.Buffer
	if err := printConversation(&out, read); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"# 设计讨论", "- subagent: child-1 子任务", "## user", "#3\n\n问题",
		"## tool · exec_command · 完成 · #5", "Input:\n````\nls ```\n````", "Output (first 5 of 9000 bytes", "## assistant · #7\n\n回答"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("missing %q in:\n%s", expected, out.String())
		}
	}

	queries = nil
	result, err = client.runConversation(context.Background(), []string{"read", "--no-tools", "--tail", "1", "--kind", "user", id}, true)
	if err != nil {
		t.Fatal(err)
	}
	if items := result.(conversationRead).Items; len(items) != 1 || items[0].Text != "追问" || strings.Contains(queries[0], "tools") {
		t.Fatalf("tail/kind: %#v %v", items, queries)
	}
	result, err = client.runConversation(context.Background(), []string{"read", id, "--max", "2"}, true)
	if read := result.(conversationRead); err != nil || len(read.Items) != 2 || read.NextCursor == nil || *read.NextCursor != 5 {
		t.Fatalf("max: %v %#v", err, read)
	}

	for _, args := range [][]string{{"read"}, {"read", "not-a-uuid"}, {"read", id, "--kind", "reasoning"}, {"read", id, "--tool-output-limit", "-1"}, {"list"}} {
		if _, err := client.runConversation(context.Background(), args, true); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
	if help, ok := cliHelpText([]string{"conversation", "read"}); !ok || !strings.Contains(help, "--tool-output-limit") {
		t.Fatalf("help: %q", help)
	}
}

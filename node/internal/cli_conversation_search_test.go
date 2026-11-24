package node

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestConversationSearchCLI(t *testing.T) {
	var seen url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/conversations/search" || r.Header.Get("X-Mira-Client-Type") != "cli" {
			http.NotFound(w, r)
			return
		}
		seen = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{"query": seen.Get("q"), "terms": strings.Fields(seen.Get("q")), "match": "any", "truncated": true,
			"results": []any{map[string]any{"engine": "claude", "threadId": "child-1", "rootThreadId": "root-1", "rootTitle": "评测重做", "title": "子 Agent · a1",
				"hitCount": 4, "latestAt": "2026-11-24T07:18:19Z", "hits": []any{map[string]any{"itemSeq": 2060541, "role": "assistant", "snippet": "…统一 评测 仓库…"}}}}})
	}))
	defer server.Close()
	client := &cliClient{identity: &persistedNodeState{ServerURL: server.URL, Token: "test-only-token"}, http: &http.Client{Timeout: 5 * time.Second}}

	result, err := client.runConversationSearch(context.Background(), []string{"评测", "--any", "unify_eval", "--engine", "claude", "--since", "2026-11-01", "--limit", "5"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Get("q") != "评测 unify_eval" || seen.Get("match") != "any" || seen.Get("engine") != "claude" || seen.Get("limit") != "5" ||
		!strings.HasPrefix(seen.Get("since"), "2026-11-01T00:00:00") || seen.Get("archived") != "include" {
		t.Fatalf("query: %v", seen)
	}
	var out bytes.Buffer
	if err := printConversationSearch(&out, result.(conversationSearchText).conversationSearch); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"1 conversation(s) matching any term of: 评测, unify_eval", "more matches exist", "## 子 Agent · a1",
		"- thread: child-1 (claude)", "- root: root-1 评测重做", "- hits: 4", "  - #2060541 assistant: …统一 评测 仓库…", "conversation read <thread> [--after SEQ-1]"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("missing %q in:\n%s", expected, out.String())
		}
	}
	if _, err := client.runConversation(context.Background(), []string{"search", "x"}, true); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	for _, args := range [][]string{{}, {"x", "--limit", "0"}, {"x", "--engine", "gpt"}, {"x", "--archived", "all"}, {"x", "--since", "last week"}} {
		if _, err := client.runConversationSearch(context.Background(), args, true); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
	if help, ok := cliHelpText([]string{"conversation", "search"}); !ok || !strings.Contains(help, "conversation search <terms...>") {
		t.Fatalf("help: %q", help)
	}
}

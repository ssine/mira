package views

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/coldstore"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func TestReadConversationMessagesPostgres(t *testing.T) {
	databaseURL := os.Getenv("MIRA_VIEWS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set MIRA_VIEWS_TEST_DATABASE_URL to run the conversation messages test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano() % 1_000_000_000_000
	store := fmt.Sprintf("messages-%d", suffix)
	fixture := &indexFixture{t: t, ctx: ctx, pool: pool, store: store}
	fixture.exec(`INSERT INTO codex_store_heads(store_id,version,history_floor) VALUES($1,0,0)`, store)
	nodeID := fmt.Sprintf("50000000-0000-4000-8000-%012d", suffix)
	sessionID := fmt.Sprintf("51000000-0000-4000-8000-%012d", suffix)
	turnID := fmt.Sprintf("52000000-0000-4000-8000-%012d", suffix)
	childTranscript := fmt.Sprintf("53000000-0000-4000-8000-%012d", suffix)
	t.Cleanup(func() {
		background := context.Background()
		for _, table := range []string{"mira_conversation_records", "mira_conversation_messages", "mira_conversation_index_state",
			"mira_thread_actions", "codex_thread_events", "codex_thread_projections", "codex_store_events", "codex_store_heads"} {
			if _, err := pool.Exec(background, "DELETE FROM "+table+" WHERE store_id=$1", store); err != nil {
				t.Errorf("cleanup %s: %v", table, err)
			}
		}
		_, _ = pool.Exec(background, `DELETE FROM mira_conversation_messages WHERE engine='claude' AND thread_id IN ($1,$2)`, sessionID, childTranscript)
		_, _ = pool.Exec(background, `DELETE FROM mira_claude_sessions WHERE session_id=$1`, sessionID)
		_, _ = pool.Exec(background, `DELETE FROM codex_nodes WHERE node_id=$1`, nodeID)
	})
	service := New(pool)
	readWith := func(threadID string, options MessagesOptions) map[string]any {
		t.Helper()
		result, err := service.ReadConversationMessages(ctx, store, threadID, options)
		if err != nil || result.Status != 200 {
			t.Fatalf("read %s: %v %#v", threadID, err, result.Body)
		}
		return result.Body
	}
	read := func(threadID string, after int64, limit int) map[string]any {
		t.Helper()
		return readWith(threadID, MessagesOptions{After: after, Limit: limit})
	}
	texts := func(body map[string]any) []string {
		out := []string{}
		for _, item := range body["items"].([]conversationItem) {
			if item.Tool != nil {
				out = append(out, fmt.Sprintf("tool:%s=%s>%s", item.Tool.Name, item.Tool.Input, item.Tool.Output))
				continue
			}
			out = append(out, item.Kind+":"+item.Text)
		}
		return out
	}

	root := fmt.Sprintf("54000000-0000-4000-8000-%012d", suffix)
	child := fmt.Sprintf("55000000-0000-4000-8000-%012d", suffix)
	fixture.thread(root, "读取对话", nil)
	fixture.thread(child, "子任务", &root)
	base := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	fixture.appendItems(root, 1, 0, []map[string]any{
		record("event_msg", map[string]any{"type": "task_started", "turn_id": "t1"}),
		record("event_msg", map[string]any{"type": "user_message", "turn_id": "t1", "message": "第一问"}),
		record("response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": "c1", "arguments": `{"cmd":"ls"}`}),
		record("response_item", map[string]any{"type": "function_call_output", "call_id": "c1", "output": "secret tool output"}),
		record("event_msg", map[string]any{"type": "agent_message", "turn_id": "t1", "message": "第一答"}),
		record("event_msg", map[string]any{"type": "user_message", "turn_id": "t1", "message": "第二问"}),
		record("event_msg", map[string]any{"type": "agent_message", "turn_id": "t1", "message": "第二答"}),
		record("event_msg", map[string]any{"type": "task_complete", "turn_id": "t1"}),
	}, base)
	fixture.appendItems(child, 1, 0, []map[string]any{
		record("event_msg", map[string]any{"type": "user_message", "turn_id": "c1", "message": "子问"}),
	}, base)

	t.Run("codex prose in order with subagents and paging", func(t *testing.T) {
		body := read(root, 0, 0)
		if fmt.Sprint(texts(body)) != "[user:第一问 assistant:第一答 user:第二问 assistant:第二答]" || body["engine"] != "codex" ||
			body["complete"] != true || body["nextCursor"] != nil || *body["title"].(*string) != "读取对话" {
			t.Fatalf("whole read: %#v", body)
		}
		children := body["children"].([]conversationChild)
		if len(children) != 1 || children[0].ThreadID != child {
			t.Fatalf("children: %#v", children)
		}
		page := read(root, 0, 3)
		cursor, ok := page["nextCursor"].(*int64)
		if len(texts(page)) != 3 || !ok {
			t.Fatalf("first page: %#v", page)
		}
		if rest := read(root, *cursor, 3); fmt.Sprint(texts(rest)) != "[assistant:第二答]" || rest["nextCursor"] != nil {
			t.Fatalf("second page: %#v", rest)
		}
		if sub := read(child, 0, 0); fmt.Sprint(texts(sub)) != "[user:子问]" || *sub["parentThreadId"].(*string) != root {
			t.Fatalf("subagent: %#v", sub)
		}
	})

	t.Run("codex tools mode projects calls with clipped output", func(t *testing.T) {
		body := readWith(root, MessagesOptions{Tools: true, ToolLimit: 10})
		items := body["items"].([]conversationItem)
		if len(items) != 5 || items[1].Kind != "tool" || body["nextCursor"] != nil || body["tools"] != true {
			t.Fatalf("tool read: %#v", body)
		}
		tool := items[1].Tool
		if !strings.Contains(tool.Input, `"cmd"`) || !tool.InputTruncated || tool.InputBytes != 17 || tool.Output != "secret too" || !tool.OutputTruncated ||
			tool.OutputBytes != len("secret tool output") || !tool.HasOutput || tool.Name != "exec_command" || tool.Status != "完成" {
			t.Fatalf("tool: %#v", tool)
		}
		if items[0].Text != "第一问" || items[4].Text != "第二答" || items[0].RecordedAt == nil {
			t.Fatalf("prose: %#v", items)
		}
	})

	t.Run("codex tool pages end before a turn start", func(t *testing.T) {
		paged := fmt.Sprintf("57000000-0000-4000-8000-%012d", suffix)
		fixture.thread(paged, "分页", nil)
		records := []map[string]any{}
		for turn := 1; turn <= 3; turn++ {
			id := fmt.Sprintf("p%d", turn)
			records = append(records,
				record("event_msg", map[string]any{"type": "task_started", "turn_id": id}),
				record("event_msg", map[string]any{"type": "user_message", "turn_id": id, "message": "问" + id}),
				record("response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": "call-" + id, "arguments": `{"cmd":"true"}`}),
				record("response_item", map[string]any{"type": "function_call_output", "call_id": "call-" + id, "output": "out-" + id}),
				record("event_msg", map[string]any{"type": "agent_message", "turn_id": id, "message": "答" + id}),
				record("event_msg", map[string]any{"type": "task_complete", "turn_id": id}))
		}
		fixture.appendItems(paged, 1, 0, records, base)
		first := readWith(paged, MessagesOptions{Tools: true, Limit: 2})
		next, ok := first["nextCursor"].(*int64)
		if !ok || next == nil || *next != 6 || fmt.Sprint(texts(first)) != "[user:问p1 tool:exec_command="+first["items"].([]conversationItem)[1].Tool.Input+">out-p1 assistant:答p1]" {
			t.Fatalf("first page: %v %#v", texts(first), first["nextCursor"])
		}
		second := readWith(paged, MessagesOptions{Tools: true, Limit: 2, After: *next})
		if items := second["items"].([]conversationItem); len(items) != 3 || items[1].Tool.Output != "out-p2" {
			t.Fatalf("second page: %v", texts(second))
		}
	})

	t.Run("deleted and unknown conversations are not found", func(t *testing.T) {
		fixture.exec(`INSERT INTO mira_thread_actions(store_id,thread_id,action,operation_id,generation) VALUES($1,$2,'delete',gen_random_uuid(),1)`, store, child)
		for _, id := range []string{child, "56000000-0000-4000-8000-000000000000"} {
			if result, err := service.ReadConversationMessages(ctx, store, id, MessagesOptions{}); err != nil || result.Status != 404 {
				t.Fatalf("%s: %v %#v", id, err, result)
			}
		}
	})

	t.Run("claude session and native subagent transcript", func(t *testing.T) {
		fixture.exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations)
		  VALUES($1::uuid,$1::text,'messages-test','linux','amd64','linux','test','{}','[]')`, nodeID)
		fixture.exec(`INSERT INTO mira_claude_sessions(session_id,request_id,node_id,cwd,title) VALUES($1,gen_random_uuid(),$2,'/work','Claude 对话')`, sessionID, nodeID)
		fixture.exec(`INSERT INTO mira_claude_turns(turn_id,session_id,node_id,revision,request,status) VALUES($1,$2,$3,1,'{}','completed')`, turnID, sessionID, nodeID)
		for _, event := range []map[string]any{
			{"type": "mira_user", "text": "你好 Claude", "timestamp": base.Format(time.RFC3339Nano)},
			{"type": "assistant", "message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "你好"}, map[string]any{"type": "tool_use", "id": "x", "name": "Bash", "input": map[string]any{}}}}},
			{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "x", "content": "ok"}}}},
		} {
			raw, _ := json.Marshal(event)
			fixture.exec(`INSERT INTO mira_claude_events(event_id,session_id,turn_id,event_type,payload) VALUES(gen_random_uuid(),$1,$2,$3,$4::json)`, sessionID, turnID, event["type"], string(raw))
		}
		fixture.exec(`INSERT INTO mira_claude_transcripts(session_id,subpath,thread_id,next_seq) VALUES($1,'subagents/agent-a1.jsonl',$2,2)`, sessionID, childTranscript)
		entry, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": "子 Agent 任务"}})
		fixture.exec(`INSERT INTO mira_claude_entries(session_id,subpath,seq,entry_uuid,payload) VALUES($1,'subagents/agent-a1.jsonl',1,'e1',$2::json)`, sessionID, string(entry))

		body := read(sessionID, 0, 0)
		if fmt.Sprint(texts(body)) != "[user:你好 Claude assistant:你好]" || body["engine"] != "claude" || body["complete"] != true {
			t.Fatalf("claude session: %#v", body)
		}
		children := body["children"].([]conversationChild)
		if len(children) != 1 || children[0].ThreadID != childTranscript {
			t.Fatalf("claude children: %#v", children)
		}
		sub := read(childTranscript, 0, 0)
		if fmt.Sprint(texts(sub)) != "[user:子 Agent 任务]" || *sub["parentThreadId"].(*string) != sessionID {
			t.Fatalf("claude subagent: %#v", sub)
		}
		tools := readWith(sessionID, MessagesOptions{Tools: true})
		if fmt.Sprint(texts(tools)) != "[user:你好 Claude assistant:你好 tool:Bash={}>ok]" {
			t.Fatalf("claude tools: %v", texts(tools))
		}
		// A frozen tool result is hydrated from the cold store.
		objects := &timelineObjects{objects: map[string][]byte{}}
		cold, err := coldstore.New(objects, coldstore.Options{CacheDir: t.TempDir(), CacheBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		coldstore.SetDefault(cold)
		t.Cleanup(func() { coldstore.SetDefault(nil) })
		fixture.exec(`INSERT INTO mira_claude_events(event_id,session_id,turn_id,event_type,payload) VALUES(gen_random_uuid(),$1,$2,'assistant',$3::json)`, sessionID, turnID,
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"y","name":"Read","input":{"file_path":"/a"}}]}}`)
		raw, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "y", "content": "冷数据内容"}}}})
		sha := coldstore.Digest(raw)
		encoded, err := coldstore.Encode(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := objects.Put(ctx, coldstore.ObjectKey(sha), encoded); err != nil {
			t.Fatal(err)
		}
		fixture.exec(`INSERT INTO mira_claude_events(event_id,session_id,turn_id,event_type,payload,cold_sha256) VALUES(gen_random_uuid(),$1,$2,'user',$3::json,$4)`, sessionID, turnID,
			fmt.Sprintf(`{"type":"user","mira_cold_payload":{"version":1,"sha256":%q,"bytes":%d}}`, sha, len(raw)), sha)
		items := readWith(sessionID, MessagesOptions{Tools: true})["items"].([]conversationItem)
		if last := items[len(items)-1]; last.Tool == nil || last.Tool.Name != "Read" || last.Tool.Output != "冷数据内容" || !strings.Contains(last.Tool.Input, "/a") {
			t.Fatalf("cold tool: %#v", last)
		}
		// Small pages still join a call with its later result through the key.
		page := readWith(sessionID, MessagesOptions{Tools: true, Limit: 4})
		cursor := page["nextCursor"].(*int64)
		rest := readWith(sessionID, MessagesOptions{Tools: true, Limit: 4, After: *cursor})["items"].([]conversationItem)
		if len(rest) != 1 || rest[0].Tool.Key != "y" || rest[0].Tool.Output != "冷数据内容" {
			t.Fatalf("split page: %#v", rest)
		}
	})
}

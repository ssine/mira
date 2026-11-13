package views

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

type indexFixture struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgxpool.Pool
	store     string
	operation string
	version   int64
}

func (fixture *indexFixture) exec(query string, args ...any) {
	fixture.t.Helper()
	if _, err := fixture.pool.Exec(fixture.ctx, query, args...); err != nil {
		fixture.t.Fatal(err)
	}
}

// appendItems writes canonical rows the way the ThreadStore does: one
// receipt per batch, raw JSON rows, then the projection head.
func (fixture *indexFixture) appendItems(threadID string, generation int64, start int, items []map[string]any, created time.Time) {
	fixture.t.Helper()
	fixture.version++
	operation := fmt.Sprintf("00000000-0000-4000-8000-%012d", fixture.version)
	fixture.exec(`INSERT INTO codex_store_events(store_id,operation_id,event_seq,previous_event_seq,result_version,appended_item_count) VALUES($1,$2,$3,$4,$3,$5)`,
		fixture.store, operation, fixture.version, fixture.version-1, len(items))
	fixture.exec(`UPDATE codex_store_heads SET version=$2 WHERE store_id=$1`, fixture.store, fixture.version)
	for index, item := range items {
		payload, _ := json.Marshal(item)
		fixture.exec(`INSERT INTO codex_thread_events(store_id,thread_id,generation,item_seq,event_format_version,payload,payload_sha256,operation_id,created_at)
		  VALUES($1,$2,$3,$4,1,$5::json,'unused',$6,$7)`, fixture.store, threadID, generation, start+index+1, string(payload), operation, created.Add(time.Duration(index)*time.Second))
	}
	fixture.exec(`UPDATE codex_thread_projections SET item_count=$3,active_generation=$4,through_event_seq=$5,updated_at=now() WHERE store_id=$1 AND thread_id=$2`,
		fixture.store, threadID, start+len(items), generation, fixture.version)
}

func (fixture *indexFixture) thread(threadID, title string, parent *string) {
	fixture.t.Helper()
	fixture.exec(`INSERT INTO codex_thread_projections(store_id,thread_id,active_generation,item_count,title,parent_thread_id,source_kind,state,through_event_seq)
	  VALUES($1,$2,1,0,$3,$4,CASE WHEN $4::text IS NULL THEN NULL ELSE 'subagent' END,'{}'::jsonb,1)`, fixture.store, threadID, title, parent)
}

func TestConversationIndexPostgres(t *testing.T) {
	databaseURL := os.Getenv("MIRA_VIEWS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set MIRA_VIEWS_TEST_DATABASE_URL to run the conversation index integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close) // Runs after the data cleanup registered below.
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := fmt.Sprintf("index-%d", time.Now().UnixNano())
	fixture := &indexFixture{t: t, ctx: ctx, pool: pool, store: store}
	fixture.exec(`INSERT INTO codex_store_heads(store_id,version,history_floor) VALUES($1,0,0)`, store)
	// Other suites in this database count global projection work; leave nothing behind.
	t.Cleanup(func() {
		for _, table := range []string{"mira_conversation_records", "mira_conversation_messages", "mira_conversation_index_state",
			"codex_thread_events", "codex_thread_projections", "codex_store_events", "codex_store_heads"} {
			if _, err := pool.Exec(context.Background(), "DELETE FROM "+table+" WHERE store_id=$1", store); err != nil {
				t.Errorf("cleanup %s: %v", table, err)
			}
		}
	})
	root := "20000000-0000-4000-8000-000000000001"
	child := "20000000-0000-4000-8000-000000000002"
	fixture.thread(root, "全文搜索设计", nil)
	fixture.thread(child, "子任务", &root)
	base := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	first := []map[string]any{
		{"type": "turn_context", "payload": map[string]any{"turn_id": "t1", "cwd": "/repo", "user_instructions": strings.Repeat("x", 4096)}},
		record("event_msg", map[string]any{"type": "task_started", "turn_id": "t1"}),
		record("event_msg", map[string]any{"type": "user_message", "turn_id": "t1", "message": "帮我设计 Mira 的对话全文搜索"}),
		record("event_msg", map[string]any{"type": "token_count", "info": map[string]any{"last_token_usage": map[string]any{"input_tokens": float64(10), "total_tokens": float64(12)}}}),
		record("response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": "c1", "arguments": `{"cmd":"psql -c 'select 1'"}`}),
		record("response_item", map[string]any{"type": "function_call_output", "call_id": "c1", "output": strings.Repeat("row\n", 20000)}),
		record("event_msg", map[string]any{"type": "agent_message", "turn_id": "t1", "message": "建议用 pg_trgm 建索引。"}),
		record("event_msg", map[string]any{"type": "task_complete", "turn_id": "t1"}),
	}
	fixture.appendItems(root, 1, 0, first, base)
	fixture.appendItems(child, 1, 0, []map[string]any{
		record("event_msg", map[string]any{"type": "task_started", "turn_id": "c1"}),
		record("event_msg", map[string]any{"type": "user_message", "turn_id": "c1", "message": "检查 trigram 是否支持中文"}),
		record("event_msg", map[string]any{"type": "agent_message", "turn_id": "c1", "message": "支持，三字以上走索引"}),
		record("event_msg", map[string]any{"type": "task_complete", "turn_id": "c1"}),
	}, base.Add(3*time.Second))
	service := New(pool)

	t.Run("outline covers the whole thread and matches paged history", func(t *testing.T) {
		result, err := service.GetTranscriptOutline(ctx, store, root, nil, nil)
		if err != nil || result.Status != 200 {
			t.Fatalf("outline: %v %#v", err, result.Body)
		}
		trace := result.Body["trace"].([]map[string]any)
		tail, err := service.GetTranscript(ctx, store, root, TranscriptOptions{Tail: true, Limit: 200, ToolDetails: new(bool)})
		if err != nil || tail.Status != 200 {
			t.Fatalf("tail: %v %#v", err, tail.Body)
		}
		paged := tail.Body["trace"].([]map[string]any)
		if len(trace) != len(paged) || len(trace) != 3 || result.Body["itemCount"] != int64(len(first)) {
			t.Fatalf("outline %d entries, paged %d: %#v", len(trace), len(paged), result.Body)
		}
		for index := range trace {
			left, right := cloneMap(trace[index]), cloneMap(paged[index])
			delete(left, "toolDetail")
			delete(right, "toolDetail")
			if !reflect.DeepEqual(left, right) {
				t.Fatalf("entry %d differs:\n%#v\n%#v", index, left, right)
			}
		}
		tool := trace[1]
		page := object(tool["toolDetail"])["pages"].([]any)[0].(map[string]any)
		from := page["from"].(int64)
		cursor := page["cursor"].(string)
		detail, err := service.GetTranscript(ctx, store, root, TranscriptOptions{Tail: true, Limit: 200, Cursor: &cursor, From: &from})
		if err != nil || detail.Status != 200 {
			t.Fatalf("detail: %v %#v", err, detail.Body)
		}
		details := detail.Body["trace"].([]map[string]any)
		if len(details) != 1 || details[0]["key"] != tool["key"] || !strings.Contains(stringValue(details[0]["body"]), "row") || detail.Body["pageCursor"] != cursor {
			t.Fatalf("tool detail: %#v", detail.Body)
		}
	})

	t.Run("incremental outline returns changed turns only", func(t *testing.T) {
		fixture.appendItems(root, 1, len(first), []map[string]any{
			record("event_msg", map[string]any{"type": "task_started", "turn_id": "t2"}),
			record("event_msg", map[string]any{"type": "user_message", "turn_id": "t2", "message": "继续实现时间线"}),
			record("event_msg", map[string]any{"type": "agent_message", "turn_id": "t2", "message": "时间线已完成"}),
			record("event_msg", map[string]any{"type": "task_complete", "turn_id": "t2"}),
		}, base.Add(2*time.Hour))
		after, generation, stale := int64(len(first)), int64(1), int64(7)
		if result, err := service.GetTranscriptOutline(ctx, store, root, &after, &stale); err != nil || len(result.Body["trace"].([]map[string]any)) != 5 || result.Body["after"] != nil {
			t.Fatalf("a different generation must return the whole outline: %v %#v", err, result.Body)
		}
		result, err := service.GetTranscriptOutline(ctx, store, root, &after, &generation)
		if err != nil || result.Status != 200 {
			t.Fatalf("outline: %v %#v", err, result.Body)
		}
		trace := result.Body["trace"].([]map[string]any)
		if len(trace) != 2 || trace[0]["turnId"] != "t2" || result.Body["itemCount"] != int64(len(first)+4) {
			t.Fatalf("incremental: %#v", trace)
		}
		// A read in the middle of a turn returns that whole turn, projected from
		// its start: the earlier prompt plus the tool whose output just arrived.
		count := len(first) + 4
		fixture.appendItems(root, 1, count, []map[string]any{
			record("event_msg", map[string]any{"type": "task_started", "turn_id": "t3"}),
			record("event_msg", map[string]any{"type": "user_message", "turn_id": "t3", "message": "第三轮"}),
			record("response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": "c3", "arguments": `{"cmd":"sleep 1"}`}),
		}, base.Add(3*time.Hour))
		fixture.appendItems(root, 1, count+3, []map[string]any{
			record("response_item", map[string]any{"type": "function_call_output", "call_id": "c3", "output": "done"}),
			record("event_msg", map[string]any{"type": "agent_message", "turn_id": "t3", "message": "第三轮完成"}),
			record("event_msg", map[string]any{"type": "task_complete", "turn_id": "t3", "duration_ms": float64(4000)}),
		}, base.Add(3*time.Hour+10*time.Second))
		middle := int64(count + 3)
		result, err = service.GetTranscriptOutline(ctx, store, root, &middle, &generation)
		if err != nil || result.Status != 200 {
			t.Fatalf("mid-turn outline: %v %#v", err, result.Body)
		}
		trace = result.Body["trace"].([]map[string]any)
		kinds := []string{}
		for _, item := range trace {
			kinds = append(kinds, stringValue(item["kind"]))
			if item["kind"] == "tool" && object(item["toolFragment"])["hasOutput"] != true {
				t.Fatalf("tool output arrived after the cursor: %#v", item)
			}
			if item["kind"] == "assistant" && item["turnElapsedMs"] != float64(4000) {
				t.Fatalf("completed turn timing: %#v", item)
			}
		}
		if fmt.Sprint(kinds) != "[user tool assistant]" || trace[0]["turnId"] != "t3" {
			t.Fatalf("mid-turn entries: %v %#v", kinds, trace)
		}
	})

	t.Run("search finds prose, titles and subagents with their root", func(t *testing.T) {
		if _, err := service.IndexCodexThread(ctx, store, child, time.Time{}); err != nil {
			t.Fatal(err)
		}
		for query, expected := range map[string][]string{
			"全文搜索":       {root},
			"pg_trgm 索引": {root},
			"trigram 中文": {child},
			"时间线":        {root},
			"全文 trigram": {},
			"支持":         {child},
		} {
			result, err := service.SearchConversations(ctx, store, SearchOptions{Query: query})
			if err != nil {
				t.Fatal(err)
			}
			found := []string{}
			for _, item := range result.Body["results"].([]*searchResult) {
				if item.Engine == "codex" && (item.ThreadID == root || item.ThreadID == child) {
					found = append(found, item.ThreadID)
					if item.ThreadID == child && (item.RootThreadID != root || item.RootTitle == nil || *item.RootTitle != "全文搜索设计") {
						t.Fatalf("child root: %#v", item)
					}
				}
			}
			if fmt.Sprint(found) != fmt.Sprint(expected) {
				t.Fatalf("%q found %v, want %v", query, found, expected)
			}
		}
		result, _ := service.SearchConversations(ctx, store, SearchOptions{Query: "全文搜索"})
		item := result.Body["results"].([]*searchResult)[0]
		if !item.TitleMatch || item.HitCount != 1 || item.Hits[0].ItemSeq != 3 || item.Hits[0].Role != "user" {
			t.Fatalf("hit: %#v", item)
		}
	})

	t.Run("timeline lanes follow the subagent tree", func(t *testing.T) {
		result, err := service.GetCodexTimeline(ctx, store, root)
		if err != nil || result.Status != 200 {
			t.Fatalf("timeline: %v %#v", err, result.Body)
		}
		lanes := result.Body["lanes"].([]*timelineLane)
		if len(lanes) != 2 || lanes[0].ThreadID != root || lanes[1].ThreadID != child || lanes[1].Depth != 1 || len(lanes[0].Turns) != 3 ||
			len(lanes[0].Tools) != 2 || lanes[0].Tools[0].End-lanes[0].Tools[0].Start != 1000 || result.Body["partial"] != false {
			t.Fatalf("lanes: %#v", lanes)
		}
	})

	t.Run("a new generation replaces the index", func(t *testing.T) {
		fixture.appendItems(root, 2, 0, []map[string]any{
			record("event_msg", map[string]any{"type": "user_message", "turn_id": "g2", "message": "重写之后的历史"}),
		}, base.Add(3*time.Hour))
		result, err := service.GetTranscriptOutline(ctx, store, root, nil, nil)
		if err != nil || result.Status != 200 || result.Body["generation"] != int64(2) || len(result.Body["trace"].([]map[string]any)) != 1 {
			t.Fatalf("generation 2: %v %#v", err, result.Body)
		}
		search, _ := service.SearchConversations(ctx, store, SearchOptions{Query: "pg_trgm"})
		for _, item := range search.Body["results"].([]*searchResult) {
			if item.ThreadID == root {
				t.Fatalf("old generation is still searchable: %#v", item)
			}
		}
	})

	t.Run("missing projections drop derived rows", func(t *testing.T) {
		fixture.exec(`DELETE FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2`, store, child)
		if complete, err := service.IndexCodexThread(ctx, store, child, time.Time{}); err != nil || !complete {
			t.Fatalf("drop: %v %v", complete, err)
		}
		var remaining int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM mira_conversation_records WHERE store_id=$1 AND thread_id=$2)+
		  (SELECT count(*) FROM mira_conversation_messages WHERE store_id=$1 AND thread_id=$2)+
		  (SELECT count(*) FROM mira_conversation_index_state WHERE store_id=$1 AND thread_id=$2)`, store, child).Scan(&remaining); err != nil || remaining != 0 {
			t.Fatalf("remaining %d %v", remaining, err)
		}
	})

	t.Run("incomplete canonical history is not published past the gap", func(t *testing.T) {
		gap := "20000000-0000-4000-8000-000000000003"
		fixture.thread(gap, "gap", nil)
		fixture.appendItems(gap, 1, 0, []map[string]any{record("event_msg", map[string]any{"type": "user_message", "message": "one"})}, base)
		fixture.exec(`UPDATE codex_thread_projections SET item_count=3 WHERE store_id=$1 AND thread_id=$2`, store, gap)
		complete, err := service.IndexCodexThread(ctx, store, gap, time.Now().Add(time.Second))
		if err != nil || complete {
			t.Fatalf("gap: %v %v", complete, err)
		}
		result, err := service.GetTranscriptOutline(ctx, store, gap, nil, nil)
		if err != nil || result.Status != 409 || result.Body["code"] != "outline_pending" {
			t.Fatalf("pending: %v %#v", err, result.Body)
		}
	})
}

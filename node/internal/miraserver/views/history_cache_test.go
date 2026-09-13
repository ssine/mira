package views

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

type historyQueryCounter struct{ queries atomic.Int64 }

func (counter *historyQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(query.SQL, "FROM codex_thread_events") {
		counter.queries.Add(1)
	}
	return ctx
}
func (*historyQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestHistoryCacheBoundariesAndEviction(t *testing.T) {
	service := New(nil)
	key := historyKey{"store", "thread", 1, 2}
	service.rememberHistory(historySummary{key: key})
	for _, other := range []historyKey{{"other", "thread", 1, 2}, {"store", "thread", 2, 2}, {"store", "thread", 1, 3}} {
		if _, ok := service.cachedHistory(other); ok {
			t.Fatalf("crossed immutable boundary: %#v", other)
		}
	}
	for i := 0; i < historyCacheLimit; i++ {
		if _, ok := service.cachedHistory(key); !ok {
			t.Fatal("recent entry evicted")
		}
		service.rememberHistory(historySummary{key: historyKey{"store", strconv.Itoa(i), 1, 1}})
	}
	if len(service.historyCache) != historyCacheLimit || service.historyLRU.Len() != historyCacheLimit {
		t.Fatal("cache exceeded its bound")
	}
	if _, ok := service.cachedHistory(historyKey{"store", "0", 1, 1}); ok {
		t.Fatal("oldest entry retained")
	}
}

func TestPostgresHistoryCacheKeepsMutableStateFresh(t *testing.T) {
	endpoint := os.Getenv("MIRA_VIEWS_TEST_DATABASE_URL")
	if endpoint == "" {
		t.Skip("set MIRA_VIEWS_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	counter := &historyQueryCounter{}
	config.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := "history-cache-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	nodeID := "18000000-0000-4000-8000-000000000001"
	threadID := "cached-thread"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, table := range []string{"mira_thread_read_positions", "mira_codex_thread_runtimes", "codex_thread_events", "codex_thread_projections", "codex_store_events", "codex_store_heads"} {
			exec("DELETE FROM "+table+" WHERE store_id=$1", store)
		}
		exec("DELETE FROM codex_nodes WHERE node_id=$1", nodeID)
	})
	exec(`INSERT INTO codex_store_heads(store_id,version,history_floor) VALUES($1,1,0)`, store)
	exec(`INSERT INTO codex_store_events(store_id,operation_id,event_seq,previous_event_seq,result_version,appended_item_count)
 VALUES($1,'6a779dbb-5d85-49b8-af53-e4d8155898aa',1,0,1,4)`, store)
	exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,approval_status,last_seen_at,channel_status,reported_app_server,capabilities,codex_installations)
 VALUES($1::uuid,$1::text,'cache-test','linux','amd64','linux','test','approved',NOW(),'{"connected":true}','{"status":"running"}','{}','[]')`, nodeID)
	exec(`INSERT INTO codex_thread_projections(store_id,thread_id,active_generation,item_count,state,through_event_seq)
 VALUES($1,$2,1,4,'{}',1)`, store, threadID)
	exec(`INSERT INTO mira_codex_thread_runtimes(store_id,thread_id,node_id) VALUES($1,$2,$3)`, store, threadID, nodeID)
	appendRecord := func(generation, seq int, item map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(item)
		exec(`INSERT INTO codex_thread_events(store_id,thread_id,generation,item_seq,event_format_version,payload,payload_sha256,operation_id)
 VALUES($1,$2,$3,$4,1,$5::json,'fixture','6a779dbb-5d85-49b8-af53-e4d8155898aa')`, store, threadID, generation, seq, string(raw))
	}
	appendRecord(1, 1, record("event_msg", map[string]any{"type": "task_started", "turn_id": "turn"}))
	appendRecord(1, 2, record("turn_context", map[string]any{"model": "gpt-6-astra", "effort": "high"}))
	appendRecord(1, 3, usageRecord(usage(100, 80, 10), usage(100, 80, 10), "turn"))
	appendRecord(1, 4, record("event_msg", map[string]any{"type": "agent_message", "message": "Visible\x00 output"}))
	service := New(pool)
	read := func() Thread {
		t.Helper()
		threads, err := service.ListThreads(ctx, store, 10, &threadID, nil)
		if err != nil || len(threads) != 1 {
			t.Fatalf("read: %#v %v", threads, err)
		}
		return threads[0]
	}
	first := read()
	if first.Activity["state"] != "running" || first.ReadState["unread"] != true || first.TokenUsage["inputTokens"] != int64(100) || *first.ReasoningEffort != "high" {
		t.Fatalf("initial summary: %#v", first)
	}
	scans := counter.queries.Load()
	if scans == 0 {
		t.Fatal("cold read did not query canonical history")
	}
	first.Activity["state"] = "caller mutation"
	first.TokenUsage["inputTokens"] = int64(-1)
	exec(`INSERT INTO mira_thread_read_positions(store_id,thread_id,generation,item_count) VALUES($1,$2,1,4)`, store, threadID)
	exec(`UPDATE codex_thread_projections SET state='{"name":"Renamed","metadata":{"model":"new-model","token_usage":{"input_tokens":200}}}' WHERE store_id=$1`, store)
	warm := read()
	if warm.Activity["state"] != "running" || warm.ReadState["unread"] != false || *warm.Title != "Renamed" || *warm.Model != "new-model" || warm.TokenUsage["inputTokens"] != int64(200) {
		t.Fatalf("mutable state became stale: %#v", warm)
	}
	exec(`UPDATE codex_nodes SET channel_status='{"connected":false}' WHERE node_id=$1`, nodeID)
	if offline := read(); offline.Activity["state"] != "unknown" || offline.Activity["reason"] != "offline" {
		t.Fatalf("cached reachability: %#v", offline.Activity)
	}
	exec(`UPDATE codex_nodes SET channel_status='{"connected":true}' WHERE node_id=$1`, nodeID)
	if online := read(); online.Activity["state"] != "running" {
		t.Fatalf("offline result poisoned history cache: %#v", online.Activity)
	}
	if counter.queries.Load() != scans {
		t.Fatalf("unchanged history scanned again: cold=%d warm=%d", scans, counter.queries.Load())
	}
	appendRecord(1, 5, record("event_msg", map[string]any{"type": "task_complete", "turn_id": "turn"}))
	exec(`UPDATE codex_thread_projections SET item_count=5 WHERE store_id=$1`, store)
	if completed := read(); completed.Activity["state"] != "idle" || counter.queries.Load() <= scans {
		t.Fatalf("append did not refresh: %#v", completed.Activity)
	}
	appendRecord(2, 1, record("event_msg", map[string]any{"type": "task_started", "turn_id": "replacement"}))
	exec(`UPDATE codex_thread_projections SET active_generation=2,item_count=1,state='{}' WHERE store_id=$1`, store)
	if replaced := read(); replaced.Activity["turnId"] != "replacement" || replaced.TokenUsage != nil || replaced.Model != nil || replaced.ReadState["latestItemSeq"] != int64(0) {
		t.Fatalf("generation leaked: %#v", replaced)
	}
}

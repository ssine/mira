package views

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func TestCostCacheEvictionAndStaleWriters(t *testing.T) {
	service := New(nil)
	state := NewCostProjection(false, nil)
	service.rememberCostProjection("active", 2, state)
	service.rememberCostProjection("active", 1, state)
	for i := range costCacheLimit {
		entry, ok := service.cachedCostProjection("active")
		if !ok || entry.itemCount != 2 {
			t.Fatal("recent projection evicted or replaced by a stale writer")
		}
		service.rememberCostProjection(strconv.Itoa(i), 1, state)
	}
	if len(service.costCache) != costCacheLimit || service.costLRU.Len() != costCacheLimit {
		t.Fatal("cache exceeded its bound")
	}
	if _, ok := service.cachedCostProjection("0"); ok {
		t.Fatal("oldest projection was not evicted")
	}
}

func TestPostgresCostCacheRetainsLargeSubagentTree(t *testing.T) {
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
	store := "cost-cache-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, table := range []string{"codex_thread_events", "codex_thread_projections", "codex_store_events", "codex_store_heads"} {
			exec("DELETE FROM "+table+" WHERE store_id=$1", store)
		}
	})
	exec(`INSERT INTO codex_store_heads(store_id,version,history_floor) VALUES($1,1,0)`, store)
	exec(`INSERT INTO codex_store_events(store_id,operation_id,event_seq,previous_event_seq,result_version,appended_item_count)
 VALUES($1,'329e5c70-1436-4100-a71b-b4619f280b78',1,0,1,362)`, store)
	metadata, _ := json.Marshal(map[string]any{"metadata": map[string]any{"token_usage": usage(100000, 80000, 1000)}})
	exec(`INSERT INTO codex_thread_projections(store_id,thread_id,parent_thread_id,active_generation,item_count,state,through_event_seq)
 SELECT $1,'thread-'||n,CASE WHEN n=0 THEN NULL ELSE 'thread-0' END,1,2,$2::jsonb,1 FROM generate_series(0,180) n`, store, string(metadata))
	appendTo := func(id *string, generation, seq int, record map[string]any) {
		t.Helper()
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO codex_thread_events(store_id,thread_id,generation,item_seq,event_format_version,payload,payload_sha256,operation_id)
 SELECT store_id,thread_id,$3,$4,1,$5::json,'fixture','329e5c70-1436-4100-a71b-b4619f280b78'
 FROM codex_thread_projections WHERE store_id=$1 AND ($2::text IS NULL OR thread_id=$2)`, store, id, generation, seq, string(raw))
	}
	appendTo(nil, 1, 1, contextRecord("gpt-6-astra", "turn"))
	appendTo(nil, 1, 2, usageRecord(usage(100000, 80000, 1000), usage(100000, 80000, 1000), "turn"))
	service := New(pool)
	root := Thread{ThreadID: "thread-0", Generation: 1, ItemCount: 2}
	check := func(amount float64, inputTokens int64) {
		t.Helper()
		statistics, err := service.GetThreadStatistics(ctx, store, root)
		estimate := statistics.CostEstimate
		if err != nil || estimate["status"] != "complete" || estimate["subagentCount"] != 180 || !closeFloat(estimate["amount"].(float64), amount) {
			t.Fatalf("cost: %#v %v", estimate, err)
		}
		if total := object(statistics.TokenUsageSummary["total"]); total["inputTokens"] != inputTokens || total["status"] != "complete" {
			t.Fatalf("stale token total: %#v", statistics.TokenUsageSummary)
		}
	}
	check(181*.33, 18100000)
	scans := counter.queries.Load()
	if scans < 181 {
		t.Fatal("cold request did not scan the entire tree")
	}
	check(181*.33, 18100000)
	check(181*.33, 18100000)
	if counter.queries.Load() != scans {
		t.Fatalf("unchanged large tree rescanned: cold=%d warm=%d", scans, counter.queries.Load())
	}
	child := "thread-1"
	appendTo(&child, 1, 3, usageRecord(usage(200000, 160000, 2000), usage(100000, 80000, 1000), "turn"))
	advanced, _ := json.Marshal(map[string]any{"metadata": map[string]any{"token_usage": usage(200000, 160000, 2000)}})
	exec(`UPDATE codex_thread_projections SET item_count=3,state=$3::jsonb WHERE store_id=$1 AND thread_id=$2`, store, child, string(advanced))
	check(182*.33, 18200000)
	if counter.queries.Load() != scans+1 {
		t.Fatal("child append did not read exactly its new history")
	}
	appendTo(&child, 2, 1, contextRecord("gpt-5.6-sol", "new-turn"))
	appendTo(&child, 2, 2, usageRecord(usage(100000, 80000, 1000), usage(100000, 80000, 1000), "new-turn"))
	exec(`UPDATE codex_thread_projections SET active_generation=2,item_count=2,state=$3::jsonb WHERE store_id=$1 AND thread_id=$2`, store, child, string(metadata))
	check(180*.33+.132, 18100000)
}

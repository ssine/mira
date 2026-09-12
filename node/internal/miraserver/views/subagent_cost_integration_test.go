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

func TestSubagentCostTree(t *testing.T) {
	endpoint := os.Getenv("MIRA_VIEWS_TEST_DATABASE_URL")
	if endpoint == "" {
		t.Skip("MIRA_VIEWS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := "cost-tree-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, table := range []string{"codex_thread_events", "codex_thread_projections", "mira_thread_actions", "codex_store_events", "codex_store_heads"} {
			exec("DELETE FROM "+table+" WHERE store_id=ANY($1::text[])", []string{store, store + "-other"})
		}
	})
	for _, target := range []string{store, store + "-other"} {
		exec(`INSERT INTO codex_store_heads(store_id,version,history_floor) VALUES($1,1,0)`, target)
		exec(`INSERT INTO codex_store_events(store_id,operation_id,event_seq,previous_event_seq,result_version,appended_item_count)
		 VALUES($1,'6a779dbb-5d85-49b8-af53-e4d8155898aa',1,0,1,1)`, target)
	}
	insert := func(targetStore, id, parent string, generation int64, fork string, items []map[string]any) Thread {
		t.Helper()
		state := map[string]any{}
		var forkID *string
		if fork != "" {
			state["createdThread"] = map[string]any{"forked_from_id": fork}
			forkID = &fork
		}
		raw, _ := json.Marshal(state)
		exec(`INSERT INTO codex_thread_projections(store_id,thread_id,parent_thread_id,active_generation,item_count,state,through_event_seq)
		 VALUES($1,$2,NULLIF($3,''),$4,$5,$6::jsonb,1) ON CONFLICT(store_id,thread_id) DO UPDATE SET active_generation=$4,item_count=$5,state=$6::jsonb`, targetStore, id, parent, generation, len(items), string(raw))
		for i, item := range items {
			raw, _ := json.Marshal(item)
			exec(`INSERT INTO codex_thread_events(store_id,thread_id,generation,item_seq,event_format_version,payload,payload_sha256,operation_id)
			 VALUES($1,$2,$3,$4,1,$5::json,'fixture','6a779dbb-5d85-49b8-af53-e4d8155898aa')`, targetStore, id, generation, i+1, string(raw))
		}
		return Thread{ThreadID: id, Generation: generation, ItemCount: int64(len(items)), ForkedFromID: forkID}
	}
	items := func(model string) []map[string]any {
		return []map[string]any{contextRecord(model, "turn"), usageRecord(usage(100000, 80000, 1000), usage(100000, 80000, 1000), "turn")}
	}
	root := insert(store, "root", "", 1, "", items("gpt-6-astra"))
	forkItems := append(items("gpt-6-astra"),
		record("event_msg", map[string]any{"type": "thread_settings_applied", "thread_id": "child", "thread_settings": map[string]any{"model": "gpt-5.6-sol"}}),
		usageRecord(usage(150000, 120000, 1500), usage(50000, 40000, 500), "child-turn"))
	child := insert(store, "child", "root", 1, "root", forkItems)
	insert(store, "grandchild", "child", 1, "", items("gpt-6-astra"))
	insert(store, "archived", "root", 1, "", []map[string]any{contextRecord("gpt-6-astra", "zero"), usageRecord(usage(0, 0, 0), usage(0, 0, 0), "zero")})
	exec(`INSERT INTO mira_thread_actions(store_id,thread_id,action,operation_id,generation) VALUES($1,'archived','archive','d079b7da-4072-4c02-b012-1eb9001fc211',1)`, store)
	insert(store+"-other", "foreign", "root", 1, "", items("gpt-6-astra"))
	service := New(pool)
	check := func(thread Thread, amount, self, children float64, count int, status string) {
		t.Helper()
		estimate, err := service.GetThreadCost(ctx, store, thread)
		if err != nil {
			t.Fatal(err)
		}
		if estimate["status"] != status || estimate["includesSubagents"] != true || estimate["subagentCount"] != count ||
			!closeFloat(estimate["amount"].(float64), amount) || !closeFloat(estimate["selfAmount"].(float64), self) || !closeFloat(estimate["subagentAmount"].(float64), children) {
			t.Fatalf("unexpected tree cost: %#v", estimate)
		}
	}
	check(root, .726, .33, .396, 3, "complete")
	check(child, .396, .066, .33, 1, "complete")
	// Child-only updates must invalidate its cached projection even if the root
	// has no new history. Old generations and another store must not contribute.
	insert(store, "grandchild", "child", 2, "", items("gpt-5.6-sol"))
	check(root, .528, .33, .198, 3, "complete")
	insert(store, "unknown", "root", 1, "", items("unknown-model"))
	check(root, .528, .33, .198, 4, "partial")
	exec(`UPDATE codex_thread_projections SET parent_thread_id='grandchild' WHERE store_id=$1 AND thread_id='root'`, store)
	check(root, .528, .33, .198, 4, "partial")
	// Traversal uses bounded pages, not a maximum tree size.
	exec(`INSERT INTO codex_thread_projections(store_id,thread_id,parent_thread_id,active_generation,item_count,state,through_event_seq)
	 SELECT $1,'pending-'||n::text,'root',1,0,'{}',1 FROM generate_series(1,260) n`, store)
	check(root, .528, .33, .198, 264, "partial")
}

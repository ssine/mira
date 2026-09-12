package views

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func TestAccountDailyCost(t *testing.T) {
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	const store = "account-cost-fixture"
	const nodeA = "10000000-0000-4000-8000-000000009001"
	const nodeB = "10000000-0000-4000-8000-000000009002"
	const a = "20000000-0000-4000-8000-000000009001"
	const b = "20000000-0000-4000-8000-000000009002"
	const c = "20000000-0000-4000-8000-000000009003"
	t.Cleanup(func() {
		for _, table := range []string{"mira_codex_execution_events", "codex_thread_events", "codex_thread_projections", "codex_store_events", "codex_store_heads"} {
			exec("DELETE FROM "+table+" WHERE store_id=$1", store)
		}
		exec(`DELETE FROM mira_node_codex_accounts WHERE node_id=ANY($1::uuid[])`, []string{nodeA, nodeB})
		exec(`DELETE FROM mira_codex_accounts WHERE account_id=ANY($1::uuid[])`, []string{a, b, c})
		exec(`DELETE FROM codex_nodes WHERE node_id=ANY($1::uuid[])`, []string{nodeA, nodeB})
	})
	for _, id := range []string{nodeA, nodeB} {
		exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status)
		 VALUES($1::uuid,$1::text,'fixture','linux','amd64','linux','test','{}','[]','approved')`, id)
	}
	for _, binding := range []struct{ id, node, name string }{{a, nodeA, "Shared cost fixture"}, {b, nodeB, "Shared cost fixture"}, {c, nodeA, "Other cost fixture"}} {
		exec(`INSERT INTO mira_codex_accounts(account_id,name) VALUES($1::uuid,$2)`, binding.id, binding.name)
		exec(`INSERT INTO mira_node_codex_accounts(node_account_id,node_id,account_id) VALUES($1::uuid,$2::uuid,$1::uuid)`, binding.id, binding.node)
	}
	exec(`INSERT INTO codex_store_heads(store_id,version,history_floor) VALUES($1,1,0)`, store)
	exec(`INSERT INTO codex_store_events(store_id,operation_id,event_seq,previous_event_seq,result_version,appended_item_count) VALUES($1,'6a779dbb-5d85-49b8-af53-e4d8155898aa',1,0,1,1)`, store)
	stamp := func(item map[string]any, at string) map[string]any { item["timestamp"] = at; return item }
	request := func(total int64, turn, at string) map[string]any {
		return stamp(usageRecord(usage(total, total*8/10, total/100), usage(100000, 80000, 1000), turn), at)
	}
	insert := func(id, fork string, generation int, items ...map[string]any) {
		state, _ := json.Marshal(map[string]any{"createdThread": map[string]any{"forked_from_id": fork}})
		exec(`INSERT INTO codex_thread_projections(store_id,thread_id,active_generation,item_count,state,through_event_seq) VALUES($1,$2,$3,$4,$5::jsonb,1)
		 ON CONFLICT(store_id,thread_id) DO UPDATE SET active_generation=$3,item_count=$4`, store, id, generation, len(items), state)
		for i, item := range items {
			raw, _ := json.Marshal(item)
			exec(`INSERT INTO codex_thread_events(store_id,thread_id,generation,item_seq,event_format_version,payload,payload_sha256,operation_id)
		 VALUES($1,$2,$3,$4,1,$5::json,'fixture','6a779dbb-5d85-49b8-af53-e4d8155898aa')`, store, id, generation, i+1, raw)
		}
	}
	bind := func(id, turn, binding string, generation int) {
		exec(`INSERT INTO mira_codex_execution_events(operation_id,store_id,thread_id,generation,node_account_id,runtime_id,revision,kind,turn_id,created_at)
		 VALUES(gen_random_uuid(),$1,$2,$3,$4::uuid,'runtime',1,'turn/started',$5,'2026-09-12T08:00:00Z')`, store, id, generation, binding, turn)
	}
	// Shanghai midnight lies between these requests. Later ownership records
	// deliberately have skewed timestamps: the exact turn must win.
	insert("root", "", 1, contextRecord("gpt-6-astra", "old"), request(100000, "old", "2026-09-11T15:59:00Z"),
		request(200000, "new", "2026-09-11T16:01:00Z"), request(200000, "new", "2026-09-11T16:02:00Z"))
	bind("root", "old", a, 1)
	bind("root", "new", c, 1)
	insert("child", "root", 1, contextRecord("gpt-6-astra", "old"), request(100000, "old", "2026-09-11T15:59:00Z"),
		record("event_msg", map[string]any{"type": "thread_settings_applied", "thread_id": "child", "thread_settings": map[string]any{"model": "gpt-6-astra"}}),
		request(200000, "child-turn", "2026-09-11T16:20:00Z"))
	bind("child", "child-turn", b, 1)
	insert("recreated", "", 1, contextRecord("gpt-6-astra", "stale"), request(100000, "stale", "2026-09-11T17:00:00Z"))
	bind("recreated", "stale", a, 1)
	insert("recreated", "", 2, contextRecord("unknown-model", "unknown"), request(100000, "unknown", "2026-09-11T17:00:00Z"))
	bind("recreated", "unknown", a, 2)
	insert("zero", "", 1, contextRecord("gpt-6-astra", "zero"), stamp(usageRecord(usage(0, 0, 0), usage(0, 0, 0), "zero"), "2026-09-09T17:00:00Z"))
	bind("zero", "zero", a, 1)
	service := New(pool)
	service.now = func() time.Time { value, _ := time.Parse(time.RFC3339, "2026-09-12T09:00:00Z"); return value }
	result, err := service.AccountCostHistory(ctx, "Shared cost fixture", "7d", "Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	days := result["days"].([]map[string]any)
	if len(days) != 7 || days[6]["date"] != "2026-09-12" || days[4]["amount"] != float64(0) || !closeFloat(days[5]["amount"].(float64), .33) || !closeFloat(days[6]["amount"].(float64), .33) || days[6]["status"] != "partial" {
		t.Fatalf("daily ownership: %#v", days)
	}
	if total := result["estimate"].(map[string]any); !closeFloat(total["amount"].(float64), .66) {
		t.Fatalf("duplicate requests or inherited history counted: %#v", total)
	}
	other, err := service.AccountCostHistory(ctx, "Other cost fixture", "24h", "Asia/Shanghai")
	if err != nil || !closeFloat(other["estimate"].(map[string]any)["amount"].(float64), .33) {
		t.Fatalf("handoff: %#v %v", other, err)
	}
	for _, zone := range []string{"America/New_York", "Asia/Kathmandu"} {
		if _, err := service.AccountCostHistory(ctx, "Shared cost fixture", "30d", zone); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.AccountCostHistory(ctx, "Shared cost fixture", "7d", "invalid-zone"); err == nil {
		t.Fatal("invalid timezone accepted")
	}
}

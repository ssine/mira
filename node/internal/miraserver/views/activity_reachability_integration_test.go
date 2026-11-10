package views

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

// A turn on a non-default account runs in that account's App Server. The
// Node row reports only the default instance, which may be stopped or retired.
func TestPostgresActivityReachabilityUsesAccountRuntime(t *testing.T) {
	endpoint := os.Getenv("MIRA_VIEWS_TEST_DATABASE_URL")
	if endpoint == "" {
		t.Skip("set MIRA_VIEWS_TEST_DATABASE_URL")
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
	const node = "10000000-0000-4000-8000-000000009101"
	const running = "20000000-0000-4000-8000-000000009101"
	const stopped = "20000000-0000-4000-8000-000000009102"
	const fallback = "20000000-0000-4000-8000-000000009103"
	accounts := []string{running, stopped, fallback}
	cleanup := func() {
		exec(`DELETE FROM mira_node_codex_accounts WHERE node_id=$1::uuid`, node)
		exec(`DELETE FROM mira_codex_accounts WHERE account_id=ANY($1::uuid[])`, accounts)
		exec(`DELETE FROM codex_nodes WHERE node_id=$1::uuid`, node)
	}
	cleanup()
	t.Cleanup(cleanup)
	exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status,
	 last_seen_at,channel_status,reported_app_server)
	 VALUES($1::uuid,$1::text,'fixture','linux','amd64','linux','test','{}','[]','approved',now(),'{"connected":true}','{"status":"stopped"}')`, node)
	for _, binding := range []struct{ id, reported string }{
		{running, `{"status":"running","startedAt":"2026-01-01T00:00:00Z"}`},
		{stopped, `{"status":"stopped"}`},
	} {
		exec(`INSERT INTO mira_codex_accounts(account_id,name) VALUES($1::uuid,$1::text)`, binding.id)
		exec(`INSERT INTO mira_node_codex_accounts(node_account_id,node_id,account_id,reported) VALUES($1::uuid,$2::uuid,$1::uuid,$3::jsonb)`, binding.id, node, binding.reported)
	}
	service := New(pool)
	started := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	thread := func(id string, account *string) Thread {
		nodeID := node
		return Thread{ThreadID: id, RuntimeNodeID: &nodeID, NodeAccountID: account,
			Activity: map[string]any{"state": "running", "turnId": "turn", "startedAt": started}}
	}
	runningID, stoppedID, unknownID := running, stopped, fallback
	threads, err := service.addActivityReachability(ctx, []Thread{
		thread("account-running", &runningID),
		thread("account-stopped", &stoppedID),
		thread("unknown-account", &unknownID),
		thread("default-runtime", nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]any{
		"account-running": {"running", nil},
		"account-stopped": {"unknown", "runtime"},
		"unknown-account": {"unknown", "runtime"},
		"default-runtime": {"unknown", "runtime"},
	}
	for _, thread := range threads {
		expected := want[thread.ThreadID]
		if thread.Activity["state"] != expected[0] || thread.Activity["reason"] != expected[1] {
			t.Fatalf("%s activity = %#v, want state=%v reason=%v", thread.ThreadID, thread.Activity, expected[0], expected[1])
		}
	}
}

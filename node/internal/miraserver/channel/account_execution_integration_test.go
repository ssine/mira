package channel

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

func executionTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	endpoint := os.Getenv("MIRA_HISTORY_UPLOAD_TEST_DATABASE_URL")
	if endpoint == "" {
		t.Skip("MIRA_HISTORY_UPLOAD_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	id, err := randomUUID()
	if err != nil {
		t.Fatal(err)
	}
	database := pgx.Identifier{"execution_test_" + strings.ReplaceAll(id, "-", "")}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+database.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+database.Sanitize()); err != nil {
			t.Error(err)
		}
	})
	config, err := pgxpool.ParseConfig(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = database[0]
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestAccountExecutionAllowsActiveTurnInput(t *testing.T) {
	pool := executionTestDatabase(t)
	ctx := context.Background()
	const binding = "523e4567-e89b-12d3-a456-426614174000"
	const otherBinding = "623e4567-e89b-12d3-a456-426614174000"
	const runtime = "723e4567-e89b-12d3-a456-426614174000"
	const storeID, threadID = "steering-test", "thread"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations)
	 VALUES($1::uuid,$1::text,'steering-test','linux','amd64','linux','test','{}','[]')`, testNodeA)
	for _, id := range []string{binding, otherBinding} {
		exec(`INSERT INTO mira_codex_accounts(account_id,name) VALUES($1::uuid,'test')`, id)
		exec(`INSERT INTO mira_node_codex_accounts(node_account_id,node_id,account_id) VALUES($1::uuid,$2::uuid,$1::uuid)`, id, testNodeA)
	}
	exec(`INSERT INTO codex_thread_projections(store_id,thread_id,active_generation,item_count,state,through_event_seq) VALUES($1,$2,1,0,'{}',1)`, storeID, threadID)
	registry := &fakeRegistry{values: map[string]nodes.Node{testNodeA: {
		NodeID: testNodeA, Capabilities: map[string]any{"codexAccountsV1": true},
		CodexAccounts: []nodes.CodexAccount{
			{NodeAccountID: binding, Enabled: true, Reported: map[string]any{"status": "running", "runtimeId": runtime}},
			{NodeAccountID: otherBinding, Enabled: true, Reported: map[string]any{"status": "running", "runtimeId": runtime}},
		},
	}}}
	channel := &Channel{db: pool, nodes: registry}
	client := &proxy{targetNodeID: testNodeA, nodeAccountID: binding, runtimeID: runtime, storeID: storeID}
	claim := func() {
		t.Helper()
		if _, err := channel.claimExecution(ctx, client, threadID, false, true); err != nil {
			t.Fatal(err)
		}
	}
	status := func(kind, state, turnID string) {
		t.Helper()
		if err := channel.recordExecutionStatus(ctx, client, threadID, kind, state, turnID); err != nil {
			t.Fatal(err)
		}
	}
	assertRoute := func(wantState, wantTurn string, wantRequests int) {
		t.Helper()
		var state, turnID string
		var requests int
		if err := pool.QueryRow(ctx, `SELECT state,COALESCE(turn_id,'') FROM mira_codex_execution_routes WHERE store_id=$1 AND thread_id=$2`, storeID, threadID).Scan(&state, &turnID); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM mira_codex_execution_events WHERE store_id=$1 AND thread_id=$2 AND kind='turn_requested'`, storeID, threadID).Scan(&requests); err != nil {
			t.Fatal(err)
		}
		if state != wantState || turnID != wantTurn || requests != wantRequests {
			t.Fatalf("route = (%s, %s, %d requests), want (%s, %s, %d requests)", state, turnID, requests, wantState, wantTurn, wantRequests)
		}
	}

	claim()
	assertRoute("starting", "", 1)
	// Requests before turn/started also belong to Codex's input serialization.
	claim()
	assertRoute("starting", "", 1)
	status("turn/started", "running", "turn-1")
	for range 3 {
		claim()
		assertRoute("running", "turn-1", 1)
	}
	// A rejected follow-up must not end an already-running turn.
	status("request_failed", "idle", "")
	status("turn/completed", "idle", "unrelated-turn")
	assertRoute("running", "turn-1", 1)
	status("turn/completed", "idle", "turn-1")
	assertRoute("idle", "turn-1", 1)
	claim()
	assertRoute("starting", "", 2)
	status("turn/started", "running", "turn-2")
	status("turn/completed", "idle", "turn-1")
	assertRoute("running", "turn-2", 2)

	for _, stale := range []*proxy{
		{targetNodeID: testNodeA, nodeAccountID: otherBinding, runtimeID: runtime, storeID: storeID},
		{targetNodeID: testNodeA, nodeAccountID: binding, runtimeID: "stale-runtime", storeID: storeID},
	} {
		if _, err := channel.claimExecution(ctx, stale, threadID, false, true); err == nil {
			t.Fatal("input from a different account or stale runtime was accepted")
		}
	}
	assertRoute("running", "turn-2", 2)
}

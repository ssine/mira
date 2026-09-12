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

func TestAccountExecutionHandoffMovesWholeCurrentGenerationTree(t *testing.T) {
	pool := executionTestDatabase(t)
	ctx := context.Background()
	const a = "123e4567-e89b-12d3-a456-426614174011"
	const b = "123e4567-e89b-12d3-a456-426614174012"
	const runtimeA = "123e4567-e89b-12d3-a456-426614174013"
	const runtimeB = "123e4567-e89b-12d3-a456-426614174014"
	const store = "tree-handoff"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations) VALUES($1::uuid,$1::text,'tree','linux','amd64','linux','test','{}','[]')`, testNodeA)
	for _, id := range []string{a, b} {
		exec(`INSERT INTO mira_codex_accounts(account_id,name) VALUES($1::uuid,'fixture')`, id)
		exec(`INSERT INTO mira_node_codex_accounts(node_account_id,node_id,account_id) VALUES($1::uuid,$2::uuid,$1::uuid)`, id, testNodeA)
	}
	for _, row := range [][3]string{{"root", "", "1"}, {"child", "root", "1"}, {"grandchild", "child", "1"}, {"closed", "root", "1"}, {"legacy", "root", "1"}, {"fork", "", "1"}, {"recreated", "root", "2"}} {
		exec(`INSERT INTO codex_thread_projections(store_id,thread_id,parent_thread_id,active_generation,item_count,state,through_event_seq) VALUES($1,$2,NULLIF($3,''),$4::bigint,0,'{}',1)`, store, row[0], row[1], row[2])
		exec(`INSERT INTO mira_codex_execution_routes(store_id,thread_id,generation,node_account_id,runtime_id,revision,state) VALUES($1,$2,$3::bigint,$4::uuid,$5,1,'idle')`, store, row[0], row[2], a, runtimeA)
		exec(`INSERT INTO mira_codex_thread_runtimes(store_id,thread_id,node_id,node_account_id) VALUES($1,$2,$3::uuid,$4::uuid)`, store, row[0], testNodeA, a)
	}
	for _, row := range [][3]string{{"child", "root", "open"}, {"grandchild", "child", "open"}, {"closed", "root", "closed"}, {"recreated", "root", "open"}} {
		operation, err := randomUUID()
		if err != nil {
			t.Fatal(err)
		}
		exec(`WITH event AS (INSERT INTO mira_agent_graph_events(store_id,operation_id,request_sha256,child_thread_id,parent_thread_id,child_generation,parent_generation,status) VALUES($1,$5::uuid,repeat('0',64),$2,$3,1,1,$4) RETURNING event_seq)
		INSERT INTO mira_agent_graph_edges(store_id,child_thread_id,parent_thread_id,child_generation,parent_generation,status,event_seq) SELECT $1,$2,$3,1,1,$4,event_seq FROM event`, store, row[0], row[1], row[2], operation)
	}
	registry := &fakeRegistry{values: map[string]nodes.Node{testNodeA: {NodeID: testNodeA, Status: "online", Capabilities: map[string]any{"codexAccountsV1": true}, CodexAccounts: []nodes.CodexAccount{
		{NodeAccountID: a, Enabled: true, Reported: map[string]any{"status": "stopped", "runtimeId": runtimeA}},
		{NodeAccountID: b, Enabled: true, Reported: map[string]any{"status": "running", "runtimeId": runtimeB}},
	}}}}
	channel := &Channel{db: pool, nodes: registry, nodeSockets: map[string]*socket{testNodeA: {}}}
	client := &proxy{targetNodeID: testNodeA, nodeAccountID: b, runtimeID: runtimeB, storeID: store}
	assertBindings := func(moved bool) {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT r.thread_id,r.node_account_id::text,l.node_account_id::text FROM mira_codex_execution_routes r JOIN mira_codex_thread_runtimes l USING(store_id,thread_id) WHERE r.store_id=$1`, store)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, route, legacy string
			if err := rows.Scan(&id, &route, &legacy); err != nil {
				t.Fatal(err)
			}
			want := a
			if moved && id != "fork" && id != "recreated" {
				want = b
			}
			if route != want || legacy != want {
				t.Fatalf("%s has route %s / legacy %s, want %s", id, route, legacy, want)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	// Old runtimes must fail before any route changes; provider projection is a
	// prerequisite for restoring children from a different service provider.
	if _, err := channel.claimExecution(ctx, client, "root", true, false); err == nil || !strings.Contains(err.Error(), "升级") {
		t.Fatalf("old runtime accepted: %v", err)
	}
	assertBindings(false)
	exec(`INSERT INTO mira_codex_account_protocols(node_account_id,runtime_id,protocol) VALUES($1::uuid,$2::uuid,2)`, b, runtimeB)
	// A child cannot split itself away from its owning root.
	if _, err := channel.claimExecution(ctx, client, "child", true, false); err == nil || !strings.Contains(err.Error(), "主会话") {
		t.Fatalf("child-only handoff accepted: %v", err)
	}
	assertBindings(false)
	if _, err := channel.claimExecution(ctx, client, "root", true, false); err != nil {
		t.Fatal(err)
	}
	assertBindings(true)
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mira_codex_execution_events WHERE store_id=$1`, store).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 5 {
		t.Fatalf("tree handoff wrote %d events, want 5", events)
	}
	// Membership and history remain intact, including archived/closed children.
	var projections, edges int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM codex_thread_projections WHERE store_id=$1),(SELECT count(*) FROM mira_agent_graph_edges WHERE store_id=$1)`, store).Scan(&projections, &edges); err != nil {
		t.Fatal(err)
	}
	if projections != 7 || edges != 4 {
		t.Fatalf("handoff rewrote history/graph: %d / %d", projections, edges)
	}
	// Repeating a resume is idempotent; all old child writers remain fenced.
	if _, err := channel.claimExecution(ctx, client, "root", true, false); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mira_codex_execution_events WHERE store_id=$1`, store).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 5 {
		t.Fatalf("repeat handoff appended %d events", events)
	}
	if _, err := channel.claimExecution(ctx, client, "grandchild", false, true); err != nil {
		t.Fatal(err)
	}
	old := &proxy{targetNodeID: testNodeA, nodeAccountID: a, runtimeID: runtimeA, storeID: store}
	registry.values[testNodeA].CodexAccounts[0].Reported["status"] = "running"
	for _, id := range []string{"root", "child", "grandchild", "closed", "legacy"} {
		if _, err := channel.claimExecution(ctx, old, id, false, true); err == nil {
			t.Fatalf("old account can still start %s", id)
		}
	}
	assertBindings(true)
}

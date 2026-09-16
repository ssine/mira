package miraserver

import (
	"context"
	"net/http"
	"testing"
)

func TestDeleteThreadProtectsPendingAndRunningTurns(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	uuid := func() string {
		id, err := randomUUID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	nodeID, binding, runtime := uuid(), uuid(), uuid()
	exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations)
 VALUES($1::uuid,$1::text,'delete-test','linux','amd64','linux','test','{}','[]')`, nodeID)
	exec(`INSERT INTO mira_codex_accounts(account_id,name) VALUES($1::uuid,'delete-test')`, binding)
	exec(`INSERT INTO mira_node_codex_accounts(node_account_id,node_id,account_id) VALUES($1::uuid,$2::uuid,$1::uuid)`, binding, nodeID)
	for _, test := range []struct {
		name, state, status, actualRuntime string
		generation                         int
		busy                               bool
	}{
		{"pending", "starting", "running", runtime, 1, true},
		{"running", "running", "running", runtime, 1, true},
		{"unknown-runtime", "starting", "", "", 1, true},
		{"idle", "idle", "running", runtime, 1, false},
		{"stopped", "starting", "stopped", runtime, 1, false},
		{"restarted", "running", "running", uuid(), 1, false},
		{"old-generation", "running", "running", runtime, 2, false},
	} {
		for _, isDefault := range []bool{false, true} {
			t.Run(test.name+map[bool]string{false: "/account", true: "/default"}[isDefault], func(t *testing.T) {
				storeID, threadID := "delete-"+uuid(), uuid()
				exec(`UPDATE mira_node_codex_accounts SET is_default=$2,reported=jsonb_build_object('status',$3::text,'runtimeId',$4::text) WHERE node_account_id=$1::uuid`, binding, isDefault, test.status, test.actualRuntime)
				exec(`UPDATE codex_nodes SET reported_app_server=jsonb_build_object('status',$2::text,'runtimeId',$3::text) WHERE node_id=$1::uuid`, nodeID, test.status, test.actualRuntime)
				created, err := PutSnapshot(ctx, pool, storeID, map[string]any{"expectedVersion": 0, "snapshot": map[string]any{
					"created_threads": map[string]any{threadID: map[string]any{"thread_id": threadID}},
					"histories":       map[string]any{threadID: []any{map[string]any{"type": "session_meta", "payload": map[string]any{"id": threadID}}}},
				}}, http.Header{"X-Codex-Operation-Id": []string{uuid()}})
				if err != nil || created.Status != 200 {
					t.Fatalf("create: %+v %v", created, err)
				}
				exec(`INSERT INTO mira_codex_execution_routes(store_id,thread_id,generation,node_account_id,runtime_id,revision,state) VALUES($1,$2,$3,$4::uuid,$5,1,$6)`, storeID, threadID, test.generation, binding, runtime, test.state)
				body := map[string]any{"operationId": uuid(), "generation": 1, "itemCount": 1}
				result, err := ManageThread(ctx, pool, storeID, threadID, "delete", body)
				if err != nil {
					t.Fatal(err)
				}
				if test.busy {
					if result.Status != 409 || result.Body["code"] != "thread_busy" {
						t.Fatalf("active deletion: %+v", result)
					}
					history, err := GetThreadHistory(ctx, pool, storeID, threadID, nil, nil)
					if err != nil || history.Status != 200 || len(history.Body["items"].([]any)) != 1 {
						t.Fatalf("rejection changed history: %+v %v", history, err)
					}
					var tombstones int
					if err := pool.QueryRow(ctx, `SELECT count(*) FROM mira_thread_actions WHERE store_id=$1 AND thread_id=$2 AND action='delete'`, storeID, threadID).Scan(&tombstones); err != nil || tombstones != 0 {
						t.Fatalf("rejection left tombstone: %d %v", tombstones, err)
					}
					exec(`UPDATE mira_codex_execution_routes SET state='idle' WHERE store_id=$1 AND thread_id=$2`, storeID, threadID)
					// A rejected operation must remain usable after the turn ends.
					result, err = ManageThread(ctx, pool, storeID, threadID, "delete", body)
				}
				if err != nil || result.Status != 200 {
					t.Fatalf("idle deletion: %+v %v", result, err)
				}
				replay, err := ManageThread(ctx, pool, storeID, threadID, "delete", body)
				if err != nil || replay.Status != 200 || replay.Body["duplicate"] != true {
					t.Fatalf("deletion replay: %+v %v", replay, err)
				}
			})
		}
	}
}

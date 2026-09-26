package miraserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

type executionHistoryFixture struct {
	t                               *testing.T
	pool                            *pgxpool.Pool
	store, thread, binding, runtime string
	version, count                  int64
	items                           []any
}

func newExecutionHistoryFixture(t *testing.T, pool *pgxpool.Pool) *executionHistoryFixture {
	t.Helper()
	id, _ := randomUUID()
	binding, _ := randomUUID()
	runtime, _ := randomUUID()
	f := &executionHistoryFixture{t: t, pool: pool, store: id, thread: id, binding: binding, runtime: runtime}
	f.exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations)
 VALUES($1::uuid,$1::text,'execution-history','linux','amd64','linux','test','{}','[]')`, binding)
	f.exec(`INSERT INTO mira_codex_accounts(account_id,name) VALUES($1::uuid,'execution-history')`, binding)
	f.exec(`INSERT INTO mira_node_codex_accounts(node_account_id,node_id,account_id,reported)
 VALUES($1::uuid,$1::uuid,$1::uuid,jsonb_build_object('status','running','runtimeId',$2::text))`, binding, runtime)
	f.append("v2", map[string]any{"type": "session_meta", "payload": map[string]any{"id": id}})
	f.exec(`INSERT INTO mira_codex_execution_routes(store_id,thread_id,generation,node_account_id,runtime_id,revision,state)
 VALUES($1,$2,1,$3::uuid,$4,1,'starting')`, f.store, f.thread, binding, runtime)
	return f
}

func (f *executionHistoryFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func lifecycle(kind, turn string) any {
	return map[string]any{"type": "event_msg", "payload": map[string]any{"type": kind, "turn_id": turn}}
}

func (f *executionHistoryFixture) append(mode string, items ...any) {
	f.t.Helper()
	op, _ := randomUUID()
	headers := http.Header{"X-Codex-Operation-Id": []string{op}}
	all := append(append([]any{}, f.items...), items...)
	ctx := context.Background()
	commit := func() (operationResponse, error) {
		if mode == "v1" {
			return PutSnapshot(ctx, f.pool, f.store, map[string]any{"expectedVersion": f.version, "snapshot": map[string]any{
				"created_threads": map[string]any{f.thread: map[string]any{"thread_id": f.thread}}, "histories": map[string]any{f.thread: all},
			}}, headers)
		}
		generation := 1
		states := []any{}
		if f.count == 0 {
			generation = 0
			states = append(states, map[string]any{"path": []any{"created_threads", f.thread}, "mode": "set", "conflictPolicy": "compareAndSwap", "expected": map[string]any{"exists": false}, "value": map[string]any{"thread_id": f.thread}})
		}
		change := map[string]any{"threadId": f.thread, "mode": "append", "expectedGeneration": generation, "expectedItemCount": f.count, "items": items}
		if mode == "upload" {
			// Stable staging identity also exercises a lost-response replay.
			f.exec(`INSERT INTO mira_history_uploads(store_id,upload_id,thread_id,item_count,total_bytes,received_bytes,status)
 VALUES($1,$2,$3,$4,1,1,'sealed') ON CONFLICT DO NOTHING`, f.store, op, f.thread, len(items))
			for i, item := range items {
				raw, _ := json.Marshal(item)
				hash, _ := digestJSON(item)
				f.exec(`INSERT INTO mira_history_upload_items(store_id,upload_id,item_seq,payload,payload_sha256) VALUES($1,$2,$3,$4::json,$5) ON CONFLICT DO NOTHING`, f.store, op, i+1, raw, hash)
			}
			delete(change, "items")
			change["itemsUploadId"] = op
		}
		return CommitDelta(ctx, f.pool, f.store, map[string]any{"expectedVersion": f.version, "stateChanges": states, "historyChanges": []any{change}}, headers)
	}
	result, err := commit()
	if err != nil || result.Status != 200 {
		f.t.Fatalf("append %s: %+v %v", mode, result, err)
	}
	replay, err := commit()
	if err != nil || replay.Status != 200 || replay.Body["duplicate"] != true {
		f.t.Fatalf("replay: %+v %v", replay, err)
	}
	f.version, _ = integer(result.Body["version"])
	f.count += int64(len(items))
	f.items = all
}

func (f *executionHistoryFixture) route(state, turn string) {
	f.t.Helper()
	var gotState, gotTurn string
	if err := f.pool.QueryRow(context.Background(), `SELECT state,COALESCE(turn_id,'') FROM mira_codex_execution_routes WHERE store_id=$1 AND thread_id=$2`, f.store, f.thread).Scan(&gotState, &gotTurn); err != nil {
		f.t.Fatal(err)
	}
	if gotState != state || gotTurn != turn {
		f.t.Fatalf("route = %s/%s, want %s/%s", gotState, gotTurn, state, turn)
	}
}

func (f *executionHistoryFixture) delete(want int) {
	f.t.Helper()
	op, _ := randomUUID()
	result, err := ManageThread(context.Background(), f.pool, f.store, f.thread, "delete", map[string]any{"generation": 1, "itemCount": f.count, "operationId": op})
	if err != nil || result.Status != want {
		f.t.Fatalf("delete: %+v %v, want %d", result, err, want)
	}
}

func TestHistoryUpdatesExecutionWithoutAppServerNotifications(t *testing.T) {
	pool := accountTestDatabase(t)
	for _, mode := range []string{"v1", "v2", "upload"} {
		for _, end := range []string{"task_complete", "turn_complete", "turn_aborted"} {
			t.Run(mode+"/"+end, func(t *testing.T) {
				f := newExecutionHistoryFixture(t, pool)
				f.append(mode, lifecycle("task_started", "turn-1"))
				f.route("running", "turn-1")
				f.delete(409)
				f.append(mode, lifecycle(end, "turn-1"))
				f.route("idle", "turn-1")
				var events int
				if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM mira_codex_execution_events WHERE store_id=$1`, f.store).Scan(&events); err != nil || events != 2 {
					t.Fatalf("lifecycle replay: %d %v", events, err)
				}
				f.delete(200)
			})
		}
	}
}

func TestDeleteReconcilesOnlyMatchingCompletedExecution(t *testing.T) {
	pool := accountTestDatabase(t)
	for _, test := range []struct {
		name, state, turn string
		generation        int
		want              int
	}{
		{"missed-notification", "running", "turn-1", 1, 200},
		{"pending-next-turn", "starting", "", 1, 409},
		{"different-turn", "running", "turn-2", 1, 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newExecutionHistoryFixture(t, pool)
			f.append("v2", lifecycle("task_started", "turn-1"), lifecycle("task_complete", "turn-1"))
			// Reproduce state left by a Server that only watched WebSockets.
			f.exec(`UPDATE mira_codex_execution_routes SET state=$3,turn_id=NULLIF($4,''),generation=$5 WHERE store_id=$1 AND thread_id=$2`, f.store, f.thread, test.state, test.turn, test.generation)
			f.delete(test.want)
		})
	}
}

func TestExecutionHistoryIgnoresUnrelatedAndOpaqueRecords(t *testing.T) {
	f := newExecutionHistoryFixture(t, accountTestDatabase(t))
	f.append("v2", lifecycle("task_started", "turn-2"))
	f.append("v2", lifecycle("task_complete", "turn-1"),
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "task_complete", "turn_id": "turn-2", "text": "opaque\x00text"}},
		lifecycle("error", "turn-2"), lifecycle("task_complete", ""), lifecycle("task_complete", "invalid\x00turn"))
	f.route("running", "turn-2")
	f.delete(409)
}

func TestExecutionHistoryAdvancesAcrossBatchesAndRejectsOldGeneration(t *testing.T) {
	f := newExecutionHistoryFixture(t, accountTestDatabase(t))
	var items []any
	for i := 0; i < 40; i++ {
		turn := fmt.Sprintf("batch-turn-%d", i)
		items = append(items, lifecycle("task_started", turn), lifecycle("task_complete", turn))
	}
	items = append(items, lifecycle("task_started", "last-turn"))
	f.append("upload", items...)
	f.route("running", "last-turn")
	f.delete(409)
	f.exec(`UPDATE mira_codex_execution_routes SET generation=2 WHERE store_id=$1 AND thread_id=$2`, f.store, f.thread)
	f.append("v2", lifecycle("task_complete", "last-turn"))
	f.route("running", "last-turn")
}

func TestDeleteDoesNotIgnoreNewStartWithoutTurnID(t *testing.T) {
	f := newExecutionHistoryFixture(t, accountTestDatabase(t))
	f.append("v2", lifecycle("task_started", "old-turn"), lifecycle("task_complete", "old-turn"), lifecycle("task_started", ""))
	f.exec(`UPDATE mira_codex_execution_routes SET state='running',turn_id='old-turn' WHERE store_id=$1 AND thread_id=$2`, f.store, f.thread)
	f.delete(409)
}

func TestExecutionCompletionAndHistoryCommitAreAtomic(t *testing.T) {
	f := newExecutionHistoryFixture(t, accountTestDatabase(t))
	f.append("v2", lifecycle("task_started", "turn"))
	f.exec(`CREATE FUNCTION reject_execution_completion() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'injected execution event write failure'; END $$`)
	f.exec(`CREATE TRIGGER reject_execution_completion BEFORE INSERT ON mira_codex_execution_events
 FOR EACH ROW WHEN (NEW.kind='turn/completed') EXECUTE FUNCTION reject_execution_completion()`)
	op, _ := randomUUID()
	body := map[string]any{"expectedVersion": f.version, "stateChanges": []any{}, "historyChanges": []any{map[string]any{
		"threadId": f.thread, "mode": "append", "expectedGeneration": 1, "expectedItemCount": f.count, "items": []any{lifecycle("task_complete", "turn")},
	}}}
	headers := http.Header{"X-Codex-Operation-Id": []string{op}}
	if _, err := CommitDelta(context.Background(), f.pool, f.store, body, headers); err == nil {
		t.Fatal("injected failure was ignored")
	}
	f.route("running", "turn")
	head, err := currentHead(context.Background(), f.pool, f.store, []string{f.thread})
	if err != nil || head.Version != f.version || head.HistoryManifest[f.thread].ItemCount != f.count {
		t.Fatalf("partial commit: %+v %v", head, err)
	}
	f.exec(`DROP TRIGGER reject_execution_completion ON mira_codex_execution_events`)
	result, err := CommitDelta(context.Background(), f.pool, f.store, body, headers)
	if err != nil || result.Status != 200 {
		t.Fatalf("same-operation retry: %+v %v", result, err)
	}
	f.route("idle", "turn")
}

func TestHistoryReplacementDoesNotReplayExecution(t *testing.T) {
	f := newExecutionHistoryFixture(t, accountTestDatabase(t))
	f.append("v2", lifecycle("task_started", "turn"))
	op, _ := randomUUID()
	result, err := CommitDelta(context.Background(), f.pool, f.store, map[string]any{"expectedVersion": f.version, "stateChanges": []any{}, "historyChanges": []any{map[string]any{
		"threadId": f.thread, "mode": "replace", "expectedGeneration": 1, "expectedItemCount": f.count,
		"items": []any{lifecycle("task_started", "turn"), lifecycle("task_complete", "turn")},
	}}}, http.Header{"X-Codex-Operation-Id": []string{op}})
	if err != nil || result.Status != 200 {
		t.Fatalf("replace: %+v %v", result, err)
	}
	f.route("running", "turn")
}

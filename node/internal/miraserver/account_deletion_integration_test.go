package miraserver

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ssine/mira/node/internal/miraserver/nodes"
	"github.com/ssine/mira/node/internal/miraserver/views"
)

func TestAccountDeletionPreservesHistoryAndLatestSettings(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	id, _ := randomUUID()
	other, _ := randomUUID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status,desired_app_server,reported_app_server)
 VALUES($1::uuid,$1::text,'delete-test','linux','amd64','linux','test','{"appServer":true,"codexAccountsV1":true}','[]','approved','{"running":false}','{"status":"stopped"}')`, id)
	for _, a := range []string{id, other} {
		exec(`INSERT INTO mira_codex_accounts(account_id,name) VALUES($1::uuid,$1::text)`, a)
		exec(`INSERT INTO mira_node_codex_accounts(node_account_id,node_id,account_id,is_default,desired,reported) VALUES($1,$2,$1,$3,'{"running":false}','{"status":"stopped"}')`, a, id, a == id)
	}
	op, _ := randomUUID()
	exec(`INSERT INTO codex_store_heads(store_id,version,history_floor) VALUES('delete-test',1,0)`)
	exec(`INSERT INTO codex_store_events(store_id,operation_id,event_seq,previous_event_seq,result_version,appended_item_count) VALUES('delete-test',$1,1,0,1,3)`, op)
	for _, thread := range []string{"root", "child", "other"} {
		binding := id
		if thread == "other" {
			binding = other
		}
		exec(`INSERT INTO codex_thread_projections(store_id,thread_id,active_generation,item_count,state,through_event_seq,parent_thread_id) VALUES('delete-test',$1,1,2,jsonb_build_object('metadata',jsonb_build_object('updated_at',$2::text)),1,$3)`, thread, map[string]string{"root": "2026-10-01T01:00:00Z", "child": "2026-10-03T01:00:00Z", "other": "2026-10-04T01:00:00Z"}[thread], map[string]any{"child": "root"}[thread])
		exec(`INSERT INTO mira_codex_thread_runtimes(store_id,thread_id,node_id,node_account_id) VALUES('delete-test',$1,$2,$3)`, thread, id, binding)
		exec(`INSERT INTO codex_thread_events(store_id,thread_id,generation,item_seq,event_format_version,payload,payload_sha256,operation_id) VALUES('delete-test',$1,1,1,1,'{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"model":"fixture","reasoning_effort":"high","service_tier":"priority"}}}','fixture',$2)`, thread, op)
	}
	// Upstream omits service_tier when it resets to Standard. A subsequent
	// turn_context carries no tier in this runtime and must not restore Fast.
	exec(`INSERT INTO codex_thread_events(store_id,thread_id,generation,item_seq,event_format_version,payload,payload_sha256,operation_id) VALUES('delete-test','root',1,2,1,'{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"model":"fixture","reasoning_effort":"medium"}}}','fixture',$1)`, op)
	settings, err := views.New(pool).LatestAccountSettings(ctx, id)
	if err != nil || settings["threadId"] != "root" || settings["serviceTier"] == nil || *settings["serviceTier"].(*string) != "default" {
		t.Fatalf("latest settings: %v %v", settings, err)
	}
	before, err := GetThreadHistory(ctx, pool, "delete-test", "root", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	service := nodes.New(pool, nodes.Options{})
	request := httptest.NewRequest("DELETE", "/", nil)
	exec(`UPDATE codex_nodes SET desired_app_server='{"running":true}',reported_app_server='{"status":"running"}' WHERE node_id=$1`, id)
	result, err := service.DeleteAccount(ctx, request, nil, id, id)
	if err != nil || result.Status != 409 {
		t.Fatalf("running deletion: %+v %v", result, err)
	}
	exec(`UPDATE codex_nodes SET desired_app_server='{"running":false}',reported_app_server='{"status":"stopped"}' WHERE node_id=$1`, id)
	for i := 0; i < 2; i++ {
		result, err = service.DeleteAccount(ctx, request, nil, id, id)
		if err != nil || result.Status != 200 {
			t.Fatalf("delete: %+v %v", result, err)
		}
	}
	node, err := service.Get(ctx, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = nodes.SelectAccount(node, ""); err == nil {
		t.Fatal("deleted default was selected")
	}
	if _, err = nodes.SelectAccount(node, id); err == nil {
		t.Fatal("deleted explicit account was selected")
	}
	if _, err = nodes.SelectAccount(node, other); err != nil {
		t.Fatal("unrelated account was removed", err)
	}
	desired, err := service.DesiredAccounts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 2 || desired[0]["enabled"] != false {
		t.Fatalf("default tombstone absent from desired state: %v", desired)
	}
	after, err := GetThreadHistory(ctx, pool, "delete-test", "root", nil, nil)
	if err != nil || !reflect.DeepEqual(before.Body, after.Body) {
		t.Fatalf("history changed: %v", err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM mira_codex_thread_runtimes WHERE node_account_id=$1`, id).Scan(&count); err != nil || count != 2 {
		t.Fatalf("historical bindings changed: %d %v", count, err)
	}
}

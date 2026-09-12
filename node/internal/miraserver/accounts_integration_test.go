package miraserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

func accountTestDatabase(t *testing.T) *pgxpool.Pool {
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
	id, _ := randomUUID()
	schema := "account_test_" + strings.ReplaceAll(id, "-", "")
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	configuration, err := pgxpool.ParseConfig(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	configuration.ConnConfig.Database = schema
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{schema}.Sanitize())
		admin.Close()
	})
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestAccountHistoryIsolationAndRecoveryProjection(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	uuid := func() string {
		id, err := randomUUID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	nodeID, bindingA, bindingB, runtimeA, runtimeB, threadID := uuid(), uuid(), uuid(), uuid(), uuid(), uuid()
	storeID := "accounts-test"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status,approved_at,channel_status,reported_app_server)
	 VALUES($1::uuid,$1::text,'accounts-test','linux','amd64','linux','test','{"appServer":true,"codexAccountsV1":true}','[]','approved',NOW(),'{"connected":true}',jsonb_build_object('status','running','runtimeId',$2::text))`, nodeID, runtimeA)
	for i, binding := range []string{bindingA, bindingB} {
		exec(`INSERT INTO mira_codex_accounts(account_id,name) VALUES($1::uuid,$2)`, binding, "Account "+binding)
		exec(`INSERT INTO mira_node_codex_accounts(node_account_id,node_id,account_id,is_default,reported) VALUES($1::uuid,$2::uuid,$1::uuid,$3,jsonb_build_object('status','running','runtimeId',$4::text))`, binding, nodeID, i == 0, runtimeB)
	}
	items := []any{
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": threadID}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "original"}}}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "reasoning", "encrypted_content": "opaque-old", "summary": []any{}}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "compaction", "id": "cmp_opaque", "encrypted_content": "opaque-compaction"}},
		map[string]any{"type": "future_rollout", "payload": map[string]any{"opaque": "preserve\x00future"}},
	}
	body := map[string]any{"expectedVersion": 0, "stateChanges": []any{map[string]any{"path": []any{"created_threads", threadID}, "mode": "set", "conflictPolicy": "compareAndSwap", "expected": map[string]any{"exists": false}, "value": map[string]any{"thread_id": threadID}}}, "historyChanges": []any{map[string]any{"threadId": threadID, "mode": "append", "expectedGeneration": 0, "expectedItemCount": 0, "items": items}}}
	opID := uuid()
	headers := http.Header{"X-Codex-Operation-Id": []string{opID}}
	created, err := CommitDelta(ctx, pool, storeID, body, headers)
	if err != nil || created.Status != 200 {
		t.Fatalf("create: %+v %v", created, err)
	}
	exec(`INSERT INTO mira_codex_execution_routes(store_id,thread_id,generation,node_account_id,runtime_id,revision,state) VALUES($1,$2,1,$3::uuid,$4,1,'idle')`, storeID, threadID, bindingA, runtimeA)
	// A user may retry before confirming recovery. Confirmation of the latest
	// failure must not make an older failure reappear as another recovery plan.
	exec(`INSERT INTO mira_codex_execution_events(operation_id,store_id,thread_id,generation,node_account_id,runtime_id,revision,kind,detail)
	 VALUES($1::uuid,$2,$3,1,$4::uuid,$5,1,'invalid_encrypted_content','{"throughItemSeq":3,"credentialRevision":1}')`, uuid(), storeID, threadID, bindingA, runtimeA)
	failureID := uuid()
	exec(`INSERT INTO mira_codex_execution_events(operation_id,store_id,thread_id,generation,node_account_id,runtime_id,revision,kind,detail)
	 VALUES($1::uuid,$2,$3,1,$4::uuid,$5,1,'invalid_encrypted_content','{"throughItemSeq":5,"credentialRevision":1}')`, failureID, storeID, threadID, bindingA, runtimeA)
	server := &Server{pool: pool, nodes: nodes.New(pool, nodes.Options{})}
	a := &nodes.CodexAccount{NodeAccountID: bindingA, CredentialRevision: 1}
	plan, err := server.inputRecoveryPlan(ctx, storeID, threadID, a)
	if err != nil || plan.FailureID != failureID || !plan.Recoverable || plan.Policy != "rebuildContext" || plan.ThroughItemSeq != 5 || plan.EncryptedItems != 2 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if _, err := server.inputRecoveryPlan(ctx, storeID, threadID, &nodes.CodexAccount{NodeAccountID: bindingB, CredentialRevision: 1}); err == nil {
		t.Fatal("other account inherited a failure")
	}
	before, err := GetThreadHistory(ctx, pool, storeID, threadID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Mira-Codex-Account", bindingA)
	request.Header.Set("X-Mira-Codex-Runtime", runtimeA)
	principal := &foundation.Principal{Kind: "node", NodeID: nodeID}
	if err := server.observeAccountProtocol(ctx, request, principal); err == nil {
		t.Fatal("missing protocol version accepted")
	}
	request.Header.Set("X-Mira-Accounts-Version", "1")
	if err := server.observeAccountProtocol(ctx, request, principal); err != nil {
		t.Fatal(err)
	}
	if err := server.observeAccountProtocol(ctx, request, &foundation.Principal{Kind: "node", NodeID: uuid()}); err == nil {
		t.Fatal("foreign Node account context accepted")
	}
	exec(`INSERT INTO mira_codex_input_compatibility(decision_id,store_id,thread_id,generation,node_account_id,model,policy,item_refs,credential_revision)
	 VALUES($1::uuid,$2,$3,1,$4::uuid,'','rebuildContext',jsonb_build_object('throughItemSeq',5,'failureId',$5::text),1)`, uuid(), storeID, threadID, bindingA, failureID)
	_, err = server.inputRecoveryPlan(ctx, storeID, threadID, a)
	var resolved *HTTPError
	if !errors.As(err, &resolved) || resolved.Code != "no_context_failure" {
		t.Fatalf("confirmed recovery exposed an older failure: %v", err)
	}
	// A genuinely new failure still requires its own explicit confirmation,
	// even if its encrypted input falls within the previous recovery prefix.
	newFailureID := uuid()
	exec(`INSERT INTO mira_codex_execution_events(operation_id,store_id,thread_id,generation,node_account_id,runtime_id,revision,kind,detail)
	 VALUES($1::uuid,$2,$3,1,$4::uuid,$5,1,'invalid_encrypted_content','{"throughItemSeq":5,"credentialRevision":1}')`, newFailureID, storeID, threadID, bindingA, runtimeA)
	plan, err = server.inputRecoveryPlan(ctx, storeID, threadID, a)
	if err != nil || plan.FailureID != newFailureID || !plan.Recoverable {
		t.Fatalf("new failure was hidden by an older confirmation: %+v %v", plan, err)
	}
	projected, err := GetThreadHistory(ctx, pool, storeID, threadID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.addInputRecovery(ctx, request, storeID, threadID, &projected); err != nil {
		t.Fatal(err)
	}
	if projected.Body["inputRecovery"] == nil || !reflect.DeepEqual(before.Body["items"], projected.Body["items"]) {
		t.Fatal("recovery must add policy without filtering canonical history")
	}
	request.Header.Set("X-Mira-Codex-Account", bindingB)
	other, err := GetThreadHistory(ctx, pool, storeID, threadID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.addInputRecovery(ctx, request, storeID, threadID, &other); err != nil || other.Body["inputRecovery"] != nil {
		t.Fatal("recovery leaked across accounts")
	}
	exec(`UPDATE mira_node_codex_accounts SET credential_revision=2 WHERE node_account_id=$1::uuid`, bindingA)
	request.Header.Set("X-Mira-Codex-Account", bindingA)
	other, err = GetThreadHistory(ctx, pool, storeID, threadID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.addInputRecovery(ctx, request, storeID, threadID, &other); err != nil || other.Body["inputRecovery"] != nil {
		t.Fatal("recovery leaked across credential changes")
	}
	// A delayed old writer cannot append after handoff. A lost acknowledgement
	// still resolves to its original receipt before checking the new route.
	exec(`UPDATE mira_codex_execution_routes SET node_account_id=$3::uuid,runtime_id=$4,revision=2 WHERE store_id=$1 AND thread_id=$2`, storeID, threadID, bindingB, runtimeB)
	oldContext := withCodexWriter(ctx, request, principal)
	duplicate, err := CommitDelta(oldContext, pool, storeID, body, headers)
	if err != nil || duplicate.Body["duplicate"] != true {
		t.Fatalf("receipt replay: %+v %v", duplicate, err)
	}
	appendBody := map[string]any{"expectedVersion": 1, "stateChanges": []any{}, "historyChanges": []any{map[string]any{"threadId": threadID, "mode": "append", "expectedGeneration": 1, "expectedItemCount": 5, "items": []any{map[string]any{"type": "future", "payload": "stale"}}}}}
	_, err = CommitDelta(oldContext, pool, storeID, appendBody, http.Header{"X-Codex-Operation-Id": []string{uuid()}})
	var conflict *HTTPError
	if !errors.As(err, &conflict) || conflict.Code != "account_execution_changed" {
		t.Fatalf("old writer accepted: %v", err)
	}
	snapshot, err := GetSnapshot(ctx, pool, storeID)
	if err != nil {
		t.Fatal(err)
	}
	encodedSnapshot, _ := json.Marshal(snapshot["snapshot"])
	var raw map[string]any
	if err := json.Unmarshal(encodedSnapshot, &raw); err != nil {
		t.Fatal(err)
	}
	histories := object(raw["histories"])
	records := histories[threadID].([]any)
	histories[threadID] = append(records, map[string]any{"type": "future", "payload": "stale-v1"})
	_, err = PutSnapshot(oldContext, pool, storeID, map[string]any{"expectedVersion": 1, "snapshot": raw}, http.Header{"X-Codex-Operation-Id": []string{uuid()}})
	if !errors.As(err, &conflict) || conflict.Code != "account_execution_changed" {
		t.Fatalf("legacy writer accepted: %v", err)
	}
	after, err := GetThreadHistory(ctx, pool, storeID, threadID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(before.Body["items"])
	c, _ := json.Marshal(after.Body["items"])
	if string(b) != string(c) {
		t.Fatal("canonical history changed after rejected writes")
	}
}

func TestEncryptedItemsUseCanonicalTypes(t *testing.T) {
	for _, test := range []struct {
		item                            map[string]any
		encrypted, compact, unsupported bool
	}{
		{map[string]any{"type": "reasoning", "id": "cmp_unrelated", "encrypted_content": "opaque"}, true, false, false},
		{map[string]any{"type": "compaction", "id": "rs_unrelated", "encrypted_content": "opaque"}, true, true, false},
		{map[string]any{"type": "message", "id": "cmp_unrelated"}, false, false, false},
		{map[string]any{"type": "future_encrypted", "encrypted_content": "opaque"}, false, false, true},
	} {
		a, b, c := classifyEncryptedResponse(test.item)
		if a != test.encrypted || b != test.compact || c != test.unsupported {
			t.Fatalf("classification: %#v", test.item)
		}
	}
}

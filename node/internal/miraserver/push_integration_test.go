package miraserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

type pushTestClient struct {
	status, calls int
	t             *testing.T
	duringSend    func()
}

func (client *pushTestClient) Do(request *http.Request) (*http.Response, error) {
	client.calls++
	if client.duringSend != nil {
		client.duringSend()
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		client.t.Fatal(err)
	}
	if request.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(request.Header.Get("Authorization"), "vapid ") || bytes.Contains(body, []byte("对话已完成")) {
		client.t.Fatal("push was not encrypted and VAPID authenticated")
	}
	return &http.Response{StatusCode: client.status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func TestPushDurableCompletionDelivery(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	uuid := func() string { id, _ := randomUUID(); return id }
	server := &Server{pool: pool, auth: foundation.NewAuthService(pool, foundation.AuthOptions{})}
	if _, err := server.auth.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := foundation.SetAdminPassword(ctx, pool, "admin", "push-test-password"); err != nil {
		t.Fatal(err)
	}
	login, err := server.auth.Login(ctx, httptest.NewRequest("POST", "/", nil), "admin", "push-test-password")
	if err != nil || login == nil {
		t.Fatalf("login: %v", err)
	}
	request := func(method, path string, body any, authenticated, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		if authenticated {
			req.Header.Set("Cookie", strings.Split(login.Cookie, ";")[0])
		}
		if csrf {
			req.Header.Set("X-Mira-Csrf", login.CSRFToken)
		}
		response := httptest.NewRecorder()
		handled, err := server.routePush(ctx, response, req)
		if !handled || err != nil {
			t.Fatalf("push route: %t %v", handled, err)
		}
		return response
	}
	if r := request("GET", "/v1/push/config", nil, false, false); r.Code != 401 {
		t.Fatalf("unauthenticated config: %d", r.Code)
	}
	config := request("GET", "/v1/push/config", nil, true, false)
	if config.Code != 200 || strings.Contains(config.Body.String(), "private") {
		t.Fatal("invalid public config")
	}
	if again := request("GET", "/v1/push/config", nil, true, false); again.Body.String() != config.Body.String() {
		t.Fatal("VAPID key changed")
	}
	sub := testPushSubscription(t)
	if r := request("POST", "/v1/push/subscription", sub, true, false); r.Code != 403 {
		t.Fatalf("missing CSRF: %d", r.Code)
	}
	if r := request("POST", "/v1/push/subscription", sub, true, true); r.Code != 200 {
		t.Fatalf("subscribe: %d %s", r.Code, r.Body)
	}
	root, child, imported := uuid(), uuid(), uuid()
	version := int64(0)
	counts := map[string]int{}
	commit := func(id string, items []any, mode string, states []any) (map[string]any, http.Header) {
		t.Helper()
		generation := 1
		if counts[id] == 0 {
			generation = 0
		}
		body := map[string]any{"expectedVersion": version, "stateChanges": states, "historyChanges": []any{map[string]any{"threadId": id, "mode": mode, "expectedGeneration": generation, "expectedItemCount": counts[id], "items": items}}}
		headers := http.Header{"X-Codex-Operation-Id": []string{uuid()}}
		result, err := CommitDelta(ctx, pool, "personal", body, headers)
		if err != nil || result.Status != 200 {
			t.Fatalf("commit: %+v %v", result, err)
		}
		version, _ = integer(result.Body["version"])
		counts[id] += len(items)
		return body, headers
	}
	record := func(kind, turn string) any {
		return map[string]any{"type": "event_msg", "payload": map[string]any{"type": kind, "turn_id": turn}}
	}
	create := func(id string, items []any) {
		commit(id, items, "append", []any{map[string]any{"path": []any{"created_threads", id}, "mode": "set", "conflictPolicy": "compareAndSwap", "expected": map[string]any{"exists": false}, "value": map[string]any{"thread_id": id, "title": "通知测试"}}})
	}
	create(root, []any{record("task_started", "root-turn")})
	create(child, []any{record("task_started", "child-turn")})
	exec(`UPDATE codex_thread_projections SET parent_thread_id=$2 WHERE thread_id=$1`, child, root)
	create(imported, []any{record("task_started", "old"), record("task_complete", "old")})
	commit(imported, []any{record("task_complete", "replacement")}, "replace", []any{})
	commit(child, []any{record("task_complete", "child-turn")}, "append", []any{})
	commit(root, []any{record("turn_aborted", "cancelled")}, "append", []any{})
	count := func(expected int) {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM mira_push_deliveries`).Scan(&n); err != nil || n != expected {
			t.Fatalf("deliveries = %d want %d: %v", n, expected, err)
		}
	}
	count(0)
	body, headers := commit(root, []any{record("task_complete", "root-turn")}, "append", []any{})
	count(1)
	if result, err := CommitDelta(ctx, pool, "personal", body, headers); err != nil || result.Status != 200 {
		t.Fatalf("retry: %+v %v", result, err)
	}
	commit(root, []any{record("task_complete", "root-turn")}, "append", []any{})
	count(1)
	// v1 compatibility writes participate in the same outbox transaction.
	snapshot, err := GetSnapshot(ctx, pool, "personal")
	if err != nil {
		t.Fatal(err)
	}
	state := snapshot["snapshot"].(map[string]any)
	histories := state["histories"].(map[string][]any)
	histories[root] = append(histories[root], record("task_complete", "v1-turn"))
	result, err := PutSnapshot(ctx, pool, "personal", map[string]any{"expectedVersion": version, "snapshot": state}, http.Header{"X-Codex-Operation-Id": []string{uuid()}})
	if err != nil || result.Status != 200 {
		t.Fatalf("v1 append: %+v %v", result, err)
	}
	count(2)
	// No browser/Node connection is involved. A fresh Server consumes the queue.
	restarted := &Server{pool: pool}
	client := &pushTestClient{t: t, status: 503}
	client.duringSend = func() {
		// Provider latency must never hold a queue row locked: unsubscribing
		// cascades through it and could otherwise stall canonical commits.
		var id int64
		if err := pool.QueryRow(ctx, `SELECT delivery_id FROM mira_push_deliveries ORDER BY delivery_id LIMIT 1 FOR UPDATE NOWAIT`).Scan(&id); err != nil {
			t.Fatalf("push held a database lock during network I/O: %v", err)
		}
	}
	if err := restarted.processPushDelivery(ctx, client); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var finished bool
	if err := pool.QueryRow(ctx, `SELECT attempts,finished FROM mira_push_deliveries ORDER BY delivery_id LIMIT 1`).Scan(&attempts, &finished); err != nil || attempts != 1 || finished {
		t.Fatalf("retry state: %d %t %v", attempts, finished, err)
	}
	client.status = 201
	exec(`UPDATE mira_push_deliveries SET next_attempt_at=now()`)
	for i := 0; i < 2; i++ {
		if err := restarted.processPushDelivery(ctx, client); err != nil {
			t.Fatal(err)
		}
	}
	if client.calls != 3 {
		t.Fatalf("requests = %d", client.calls)
	}
	if err := restarted.processPushDelivery(ctx, client); err != nil {
		t.Fatal(err)
	}
	if client.calls != 3 {
		t.Fatal("sent notification replayed")
	}
	exec(`UPDATE mira_push_deliveries SET finished=false,next_attempt_at=now()`)
	client.status = 410
	if err := restarted.processPushDelivery(ctx, client); err != nil {
		t.Fatal(err)
	}
	count(0)
	if r := request("POST", "/v1/push/subscription", sub, true, true); r.Code != 200 {
		t.Fatal("resubscribe failed")
	}
	// Enqueue a pending notification, then revoke the login as logout/password reset does.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = enqueuePush(ctx, tx, "codex", "personal", root, 1, "logout-turn", "test"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE mira_admin_sessions SET revoked_at=now()`)
	if err := restarted.processPushDelivery(ctx, client); err != nil {
		t.Fatal(err)
	}
	count(0)
	if client.calls != 4 {
		t.Fatal("revoked subscription received a notification")
	}
}

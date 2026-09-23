package miraserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

type notificationTestSender func(context.Context, string, map[string]any) (any, error)

func (f notificationTestSender) DeliverNotification(c context.Context, n string, p map[string]any) (any, error) {
	return f(c, n, p)
}

func TestNativeNotificationsDurableOptIn(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	uuid := func() string { id, _ := randomUUID(); return id }
	server := &Server{pool: pool, auth: foundation.NewAuthService(pool, foundation.AuthOptions{})}
	if _, err := server.auth.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := foundation.SetAdminPassword(ctx, pool, "admin", "native-notification-test"); err != nil {
		t.Fatal(err)
	}
	login, err := server.auth.Login(ctx, httptest.NewRequest("POST", "/", nil), "admin", "native-notification-test")
	if err != nil {
		t.Fatal(err)
	}
	nodeID := uuid()
	exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status)
 VALUES($1,'test-android','test','android','arm64','android','test','{}','[]','approved')`, nodeID)
	request := func(method string, auth, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/v1/push/node-subscription?nodeKey=test-android", strings.NewReader(`{"nodeKey":"test-android"}`))
		if auth {
			req.Header.Set("Cookie", strings.Split(login.Cookie, ";")[0])
		}
		if csrf {
			req.Header.Set("X-Mira-Csrf", login.CSRFToken)
		}
		w := httptest.NewRecorder()
		handled, err := server.routeNodeNotifications(ctx, w, req)
		if !handled || err != nil {
			t.Fatalf("route: %t %v", handled, err)
		}
		return w
	}
	if w := request("POST", false, false); w.Code != 401 {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	if w := request("POST", true, false); w.Code != 403 {
		t.Fatalf("CSRF: %d", w.Code)
	}
	if w := request("POST", true, true); w.Code != 200 {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body)
	}
	version := int64(0)
	counts := map[string]int{}
	root, child, imported := uuid(), uuid(), uuid()
	record := func(kind, turn string) any {
		return map[string]any{"type": "event_msg", "payload": map[string]any{"type": kind, "turn_id": turn}}
	}
	commit := func(id string, items []any) {
		t.Helper()
		generation := 1
		var states []any
		if counts[id] == 0 {
			generation = 0
			states = []any{map[string]any{"path": []any{"created_threads", id}, "mode": "set", "conflictPolicy": "compareAndSwap", "expected": map[string]any{"exists": false}, "value": map[string]any{"thread_id": id, "title": "Native test"}}}
		}
		body := map[string]any{"expectedVersion": version, "stateChanges": states, "historyChanges": []any{map[string]any{"threadId": id, "mode": "append", "expectedGeneration": generation, "expectedItemCount": counts[id], "items": items}}}
		result, err := CommitDelta(ctx, pool, "personal", body, http.Header{"X-Codex-Operation-Id": []string{uuid()}})
		if err != nil || result.Status != 200 {
			t.Fatalf("commit: %v %+v", err, result)
		}
		version, _ = integer(result.Body["version"])
		counts[id] += len(items)
	}
	count := func(where string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM mira_node_notification_deliveries `+where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	commit(root, []any{record("task_started", "root")})
	commit(child, []any{record("task_started", "child")})
	exec(`UPDATE codex_thread_projections SET parent_thread_id=$2 WHERE thread_id=$1`, child, root)
	commit(imported, []any{record("task_complete", "historical")})
	commit(child, []any{record("task_complete", "child")})
	commit(root, []any{record("turn_aborted", "cancelled")})
	if count("") != 0 {
		t.Fatal("historical, child or cancelled turn notified")
	}
	commit(root, []any{record("task_complete", "root")})
	commit(root, []any{record("task_complete", "root")})
	if count("") != 1 {
		t.Fatal("native-only opt-in did not enqueue exactly once")
	}
	var webCount int
	pool.QueryRow(ctx, `SELECT count(*) FROM mira_push_deliveries`).Scan(&webCount)
	if webCount != 0 {
		t.Fatal("unexpected web push")
	}
	calls := 0
	var first string
	fail := true
	sender := notificationTestSender(func(_ context.Context, target string, p map[string]any) (any, error) {
		calls++
		id := p["deliveryId"].(string)
		if first == "" {
			first = id
		} else if first != id {
			t.Fatal("retry changed notification identity")
		}
		if target != nodeID || p["threadId"] != root {
			t.Fatal("wrong target")
		}
		raw, _ := json.Marshal(p)
		if len(raw) > 4096 || bytes.Contains(raw, []byte("token")) {
			t.Fatal("invalid payload")
		}
		// Neither canonical completion nor disabling should wait on network I/O.
		var unlocked string
		if err := pool.QueryRow(ctx, `SELECT delivery_id FROM mira_node_notification_deliveries LIMIT 1 FOR UPDATE NOWAIT`).Scan(&unlocked); err != nil {
			t.Fatal(err)
		}
		if fail {
			return nil, errors.New("offline")
		}
		return map[string]any{"accepted": true}, nil
	})
	if err := server.processNodeNotificationWith(ctx, sender); err != nil {
		t.Fatal(err)
	}
	if count("WHERE finished") != 0 {
		t.Fatal("offline delivery acknowledged")
	}
	fail = false
	exec(`UPDATE mira_node_notification_deliveries SET next_attempt_at=now()`)
	restarted := &Server{pool: pool}
	if err := restarted.processNodeNotificationWith(ctx, sender); err != nil {
		t.Fatal(err)
	}
	if count("WHERE finished") != 1 || calls != 2 {
		t.Fatal("restart did not acknowledge retry")
	}
	if err := restarted.processNodeNotificationWith(ctx, sender); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("acknowledged event replayed")
	}
	// Native subscribers use the same v1 compatibility boundary.
	snapshot, err := GetSnapshot(ctx, pool, "personal")
	if err != nil {
		t.Fatal(err)
	}
	state := snapshot["snapshot"].(map[string]any)
	state["histories"].(map[string][]any)[root] = append(state["histories"].(map[string][]any)[root], record("task_complete", "v1"))
	result, err := PutSnapshot(ctx, pool, "personal", map[string]any{"expectedVersion": version, "snapshot": state}, http.Header{"X-Codex-Operation-Id": []string{uuid()}})
	if err != nil || result.Status != 200 {
		t.Fatalf("v1: %v", err)
	}
	if count("") != 2 {
		t.Fatal("v1 append did not enqueue")
	}
	if w := request("DELETE", true, true); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if count("") != 0 {
		t.Fatal("unsubscribe retained queue")
	}
	if w := request("POST", true, true); w.Code != 200 {
		t.Fatal(w.Body)
	}
	for i := 0; i < 135; i++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = enqueuePush(ctx, tx, "codex", "personal", root, 1, fmt.Sprint(i), "bounded"); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := count("WHERE NOT finished"); n != 128 {
		t.Fatalf("backlog = %d", n)
	}
	exec(`UPDATE mira_admin_sessions SET revoked_at=now()`)
	if err := restarted.processNodeNotificationWith(ctx, sender); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || count("") != 0 {
		t.Fatal("logout delivered or retained notifications")
	}
}

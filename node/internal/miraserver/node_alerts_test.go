package miraserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func TestNodeAlertValidation(t *testing.T) {
	cases := []struct {
		in nodeAlertRequest
		ok bool
	}{
		{nodeAlertRequest{Title: "iOA 代理故障"}, true},
		{nodeAlertRequest{Title: " x ", Level: "critical", URL: "https://portal.example/work/?tab=status"}, true},
		{nodeAlertRequest{Title: ""}, false},
		{nodeAlertRequest{Title: strings.Repeat("长", 121)}, false},
		{nodeAlertRequest{Title: "x", Body: strings.Repeat("b", 1001)}, false},
		{nodeAlertRequest{Title: "x", Level: "urgent"}, false},
		{nodeAlertRequest{Title: "x", URL: "http://portal.example/"}, false},
		{nodeAlertRequest{Title: "x", URL: "https://user:pass@portal.example/"}, false},
		{nodeAlertRequest{Title: "x", URL: "intent://evil"}, false},
		{nodeAlertRequest{Title: strings.Repeat("标", 30), Body: strings.Repeat("长", 999)}, false}, // 3087 bytes: over the relay budget
		{nodeAlertRequest{Title: "x", Body: strings.Repeat("长", 900)}, true},
	}
	for i, c := range cases {
		in := c.in
		err := in.validate()
		if (err == nil) != c.ok {
			t.Fatalf("case %d: %+v -> %v", i, c.in, err)
		}
		if c.ok && in.Level == "" {
			t.Fatalf("case %d: default level missing", i)
		}
	}
}

func TestNodeAlertQueueAndDelivery(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	server := &Server{pool: pool, auth: foundation.NewAuthService(pool, foundation.AuthOptions{})}
	if _, err := server.auth.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := foundation.SetAdminPassword(ctx, pool, "admin", "node-alert-test"); err != nil {
		t.Fatal(err)
	}
	login, err := server.auth.Login(ctx, httptest.NewRequest("POST", "/", nil), "admin", "node-alert-test")
	if err != nil {
		t.Fatal(err)
	}
	nodeID, _ := randomUUID()
	if _, err = pool.Exec(ctx, `INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status)
 VALUES($1,'alert-android','test','android','arm64','android','test','{}','[]','approved')`, nodeID); err != nil {
		t.Fatal(err)
	}
	call := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Cookie", strings.Split(login.Cookie, ";")[0])
		req.Header.Set("X-Mira-Csrf", login.CSRFToken)
		w := httptest.NewRecorder()
		handled, err := server.routeNodeNotifications(ctx, w, req)
		if !handled {
			t.Fatalf("route not handled: %s", path)
		}
		if err != nil {
			var httpErr *HTTPError
			if e, ok := err.(*HTTPError); ok {
				httpErr = e
				w.Code = httpErr.Status
				return w
			}
			t.Fatal(err)
		}
		return w
	}
	alert := `{"title":"iOA 代理故障","body":"心跳 5 分钟未更新","level":"critical","url":"https://portal.example/work/?tab=status"}`
	if w := call("/v1/notifications/alerts", alert); w.Code != 409 {
		t.Fatalf("alert without subscribed device: %d %s", w.Code, w.Body)
	}
	if w := call("/v1/push/node-subscription", `{"nodeKey":"alert-android"}`); w.Code != 200 {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body)
	}
	if w := call("/v1/notifications/alerts", `{"title":"x","url":"http://insecure.example/"}`); w.Code != 400 {
		t.Fatalf("insecure url accepted: %d", w.Code)
	}
	w := call("/v1/notifications/alerts", alert)
	if w.Code != 202 {
		t.Fatalf("alert: %d %s", w.Code, w.Body)
	}
	var queued struct {
		Queued      int      `json:"queued"`
		DeliveryIDs []string `json:"deliveryIds"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &queued); err != nil || queued.Queued != 1 || len(queued.DeliveryIDs) != 1 {
		t.Fatalf("queued: %s %v", w.Body, err)
	}
	attempts := 0
	sender := notificationTestSender(func(_ context.Context, target string, p map[string]any) (any, error) {
		attempts++
		if target != nodeID || p["kind"] != "alert" || p["deliveryId"] != queued.DeliveryIDs[0] || p["level"] != "critical" ||
			p["title"] != "iOA 代理故障" || p["body"] != "心跳 5 分钟未更新" || p["url"] != "https://portal.example/work/?tab=status" {
			t.Fatalf("unexpected alert payload: %v", p)
		}
		if _, ok := p["threadId"]; ok {
			t.Fatal("alert payload must not carry a thread")
		}
		raw, _ := json.Marshal(p)
		if len(raw) > 4096 {
			t.Fatal("alert payload exceeds the Node relay bound")
		}
		if attempts == 1 {
			return map[string]any{"accepted": false}, nil
		}
		return map[string]any{"accepted": true}, nil
	})
	if err = server.processNodeNotificationWith(ctx, sender); err != nil {
		t.Fatal(err)
	}
	var finished bool
	pool.QueryRow(ctx, `SELECT finished FROM mira_node_notification_deliveries WHERE delivery_id=$1`, queued.DeliveryIDs[0]).Scan(&finished)
	if finished {
		t.Fatal("unacknowledged alert finished")
	}
	if _, err = pool.Exec(ctx, `UPDATE mira_node_notification_deliveries SET next_attempt_at=now()`); err != nil {
		t.Fatal(err)
	}
	if err = server.processNodeNotificationWith(ctx, sender); err != nil {
		t.Fatal(err)
	}
	pool.QueryRow(ctx, `SELECT finished FROM mira_node_notification_deliveries WHERE delivery_id=$1`, queued.DeliveryIDs[0]).Scan(&finished)
	if !finished || attempts != 2 {
		t.Fatalf("acknowledged alert: finished=%t attempts=%d", finished, attempts)
	}
	// An alert never consults thread projections, so a missing thread is not a reason to drop it.
	var kind string
	pool.QueryRow(ctx, `SELECT kind FROM mira_node_notification_deliveries WHERE delivery_id=$1`, queued.DeliveryIDs[0]).Scan(&kind)
	if kind != "alert" {
		t.Fatalf("kind %q", kind)
	}
}

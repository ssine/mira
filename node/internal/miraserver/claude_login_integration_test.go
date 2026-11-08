package miraserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestClaudeSubscriptionLoginRoutes(t *testing.T) {
	f := newClaudeFixture(t)
	dialer := websocket.Dialer{Subprotocols: []string{"mira-node-v1", "auth." + base64.RawURLEncoding.EncodeToString([]byte(f.token))}}
	ws, _, err := dialer.Dial("ws"+strings.TrimPrefix(f.endpoint, "http")+"/v1/nodes/"+f.nodeID+"/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var writes sync.Mutex
	go func() {
		for {
			var message map[string]any
			if ws.ReadJSON(&message) != nil {
				return
			}
			if message["type"] != "request" || message["capability"] != "claude" {
				continue
			}
			params, _ := message["params"].(map[string]any)
			var result any
			switch params["action"] {
			case "account/configure":
				result = map[string]any{"configured": false, "authType": "subscription"}
			case "account/login/start":
				result = map[string]any{"sessionId": "fixture-session", "status": "pending", "verificationUrl": "https://claude.com/cai/oauth/authorize?code=true"}
			case "account/login/status":
				result = map[string]any{"sessionId": "fixture-session", "status": "pending"}
			case "account/login/complete":
				result = map[string]any{"sessionId": "fixture-session", "status": "completed"}
			case "account/login/cancel":
				result = map[string]any{"sessionId": "fixture-session", "status": "cancelled"}
			case "account/logout":
				result = map[string]any{"configured": false, "authType": "subscription"}
			default:
				t.Errorf("unexpected Claude action: %v", params["action"])
			}
			reply := map[string]any{"type": "response", "requestId": message["requestId"], "ok": true, "result": result}
			writes.Lock()
			_ = ws.WriteJSON(reply)
			writes.Unlock()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !f.server.channel.IsConnected(f.nodeID) {
		if time.Now().After(deadline) {
			t.Fatal("Node did not connect")
		}
		time.Sleep(10 * time.Millisecond)
	}

	account := f.call("POST", "/v1/claude/accounts", map[string]any{
		"nodeId": f.nodeID, "name": "Max subscription", "authType": "subscription", "loginEmail": "person@example.test",
	})
	id := account["nodeAccountId"].(string)
	if account["authType"] != "subscription" || account["email"] != "person@example.test" {
		t.Fatalf("account auth metadata: %#v", account)
	}
	f.call("POST", "/v1/claude/accounts/"+id+"/configure", map[string]any{
		"provider": map[string]any{"id": "anthropic", "baseUrl": "https://api.anthropic.com", "model": "opus"},
		"authType": "subscription", "loginEmail": "person@example.test",
	})
	started := f.call("POST", "/v1/claude/accounts/"+id+"/login", map[string]any{})
	sessionID := started["sessionId"].(string)
	if sessionID != "fixture-session" || started["verificationUrl"] == nil {
		t.Fatalf("login start: %#v", started)
	}
	status := f.call("GET", "/v1/claude/accounts/"+id+"/login?sessionId="+sessionID, nil)
	if status["status"] != "pending" {
		t.Fatalf("login status: %#v", status)
	}
	completed := f.call("POST", "/v1/claude/accounts/"+id+"/login/complete", map[string]any{"sessionId": sessionID, "code": "code#state"})
	if completed["status"] != "completed" {
		t.Fatalf("login complete: %#v", completed)
	}
	var configured bool
	var revision int64
	if err = f.server.pool.QueryRow(context.Background(), `SELECT configured,credential_revision FROM mira_claude_accounts WHERE node_account_id=$1`, id).Scan(&configured, &revision); err != nil {
		t.Fatal(err)
	}
	if !configured || revision < 3 {
		t.Fatalf("login state: configured=%v revision=%d", configured, revision)
	}
	public := f.call("GET", "/v1/nodes/"+f.nodeID, nil)
	raw, _ := json.Marshal(public)
	if !strings.Contains(string(raw), `"authType":"subscription"`) || !strings.Contains(string(raw), `"email":"person@example.test"`) {
		t.Fatalf("public subscription metadata: %s", raw)
	}
	f.call("POST", "/v1/claude/accounts/"+id+"/logout", map[string]any{})
	if err = f.server.pool.QueryRow(context.Background(), `SELECT configured FROM mira_claude_accounts WHERE node_account_id=$1`, id).Scan(&configured); err != nil {
		t.Fatal(err)
	}
	if configured {
		t.Fatal("logout remained configured")
	}
}

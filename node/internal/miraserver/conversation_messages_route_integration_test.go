package miraserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	serverviews "github.com/ssine/mira/node/internal/miraserver/views"
)

// A Node credential (the Mira CLI) reads conversation prose without an
// administrator session; anonymous requests are rejected.
func TestConversationMessagesRouteAcceptsNodeCredential(t *testing.T) {
	f := newClaudeFixture(t)
	f.server.views = serverviews.New(f.server.pool)
	ctx := context.Background()
	sessionID, turnID := uuidClaude(), uuidClaude()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.server.pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO mira_claude_sessions(session_id,request_id,node_id,cwd,title) VALUES($1,gen_random_uuid(),$2,'/work','CLI 读取')`, sessionID, f.nodeID)
	exec(`INSERT INTO mira_claude_turns(turn_id,session_id,node_id,revision,request,status) VALUES($1,$2,$3,1,'{}','completed')`, turnID, sessionID, f.nodeID)
	exec(`INSERT INTO mira_claude_events(event_id,session_id,turn_id,event_type,payload) VALUES(gen_random_uuid(),$1,$2,'mira_user','{"type":"mira_user","text":"从 CLI 读我"}'::json)`, sessionID, turnID)
	get := func(authorization string, query string) (int, map[string]any) {
		t.Helper()
		request, _ := http.NewRequest("GET", f.endpoint+"/v1/conversations/"+strings.ToUpper(sessionID)+"/messages"+query, nil)
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
			request.Header.Set("X-Mira-Client-Type", "cli")
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		return response.StatusCode, body
	}
	if status, _ := get("", ""); status != 401 {
		t.Fatalf("anonymous status %d", status)
	}
	if status, _ := get("Bearer "+f.token, "?tools=yes"); status != 400 {
		t.Fatalf("invalid tools flag status %d", status)
	}
	for _, query := range []string{"", "?tools=1&toolOutputLimit=0"} {
		status, body := get("Bearer "+f.token, query)
		items, _ := body["items"].([]any)
		if status != 200 || body["engine"] != "claude" || body["title"] != "CLI 读取" || len(items) != 1 ||
			items[0].(map[string]any)["text"] != "从 CLI 读我" || body["tools"] != (query != "") {
			t.Fatalf("node read %q %d: %#v", query, status, body)
		}
	}
}

// Launcher instructions are served only to the Node they describe.
func TestLauncherInstructionsRouteIsNodeScoped(t *testing.T) {
	f := newClaudeFixture(t)
	if _, err := f.server.pool.Exec(context.Background(), `UPDATE codex_nodes SET machine_status='{"miraCliPath":"/opt/mira/mira"}'::jsonb WHERE node_id=$1`, f.nodeID); err != nil {
		t.Fatal(err)
	}
	get := func(nodeID string, admin bool) (int, map[string]any) {
		t.Helper()
		request, _ := http.NewRequest("GET", f.endpoint+"/v1/nodes/"+nodeID+"/launcher-instructions", nil)
		if admin {
			request.Header.Set("Cookie", f.cookie)
		} else {
			request.Header.Set("Authorization", "Bearer "+f.token)
			request.Header.Set("X-Mira-Client-Type", "cli")
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body := map[string]any{}
		_ = json.NewDecoder(response.Body).Decode(&body)
		return response.StatusCode, body
	}
	status, body := get(f.nodeID, false)
	cli, _ := body["cliInstructions"].(string)
	if status != 200 || !strings.Contains(cli, "/opt/mira/mira") || !strings.Contains(cli, "conversation read <conversation-id>") || body["developerInstructions"] != "" {
		t.Fatalf("own node %d: %#v", status, body)
	}
	if status, _ := get(uuidClaude(), false); status != 403 {
		t.Fatalf("other node status %d", status)
	}
	if status, _ := get(f.nodeID, true); status != 403 {
		t.Fatalf("administrator status %d", status)
	}
}

package miraserver

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestClaudeUnifiedUsage(t *testing.T) {
	f := newClaudeFixture(t)
	ctx := context.Background()
	p := f.server.pool
	accountA, accountB := uuidClaude(), uuidClaude()
	for id, name := range map[string]string{accountA: "Messages", accountB: "AWS"} {
		if _, err := p.Exec(ctx, `INSERT INTO mira_claude_accounts(node_account_id,node_id,name) VALUES($1,$2,$3)`, id, f.nodeID, name); err != nil {
			t.Fatal(err)
		}
	}
	id, _, _ := f.reserved()
	// The pre-existing empty turn is intentionally removed for this clean first-result case.
	if _, err := p.Exec(ctx, `DELETE FROM mira_claude_turns WHERE session_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	result := func(revision int, account string, cost float64, input int) {
		turn, event := uuidClaude(), uuidClaude()
		if _, err := p.Exec(ctx, `INSERT INTO mira_claude_turns(turn_id,session_id,node_id,revision,request,node_account_id,account_name) VALUES($1,$2,$3,$4,$5::json,$6,(SELECT name FROM mira_claude_accounts WHERE node_account_id=$6))`, turn, id, f.nodeID, revision, fmt.Sprintf(`{"nodeAccountId":%q}`, account), account); err != nil {
			t.Fatal(err)
		}
		raw := fmt.Sprintf(`{"type":"result","total_cost_usd":%v,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},"modelUsage":{"claude":{"inputTokens":%d,"outputTokens":%d,"cacheReadInputTokens":0,"cacheCreationInputTokens":0}}}`, cost, input, revision*2)
		if _, err := p.Exec(ctx, `INSERT INTO mira_claude_events(event_id,session_id,turn_id,event_type,payload) VALUES($1,$2,$3,'result',$4::json)`, event, id, turn, raw); err != nil {
			t.Fatal(err)
		}
		// Transport duplicates do not reapply the projection.
		if _, err := p.Exec(ctx, `INSERT INTO mira_claude_events(event_id,session_id,turn_id,event_type,payload) VALUES($1,$2,$3,'result',$4::json) ON CONFLICT DO NOTHING`, event, id, turn, raw); err != nil {
			t.Fatal(err)
		}
	}
	result(1, accountA, 1, 10)
	result(2, accountB, 1.4, 20)
	summary := f.call("GET", "/v1/claude/conversations/"+id, nil)
	cost := summary["costEstimate"].(map[string]any)
	if cost["amount"] != 1.4 || cost["basis"] != "claude_sdk" {
		t.Fatalf("cost: %#v", cost)
	}
	usage := summary["tokenUsage"].(map[string]any)
	if usage["inputTokens"] != float64(20) {
		t.Fatalf("usage duplicated: %#v", usage)
	}
	for name, amount := range map[string]float64{"Messages": 1, "AWS": .4} {
		history := f.call("GET", "/v1/claude/accounts/cost-history?name="+name+"&range=7d&timezone=Asia%2FShanghai", nil)
		got := history["estimate"].(map[string]any)["amount"].(float64)
		if got < amount-.000001 || got > amount+.000001 {
			t.Fatalf("account %s got %v", name, got)
		}
	}
	// A genuine reported zero is distinct from missing usage; reset is visible.
	result(3, accountB, 0, 0)
	summary = f.call("GET", "/v1/claude/conversations/"+id, nil)
	if summary["costEstimate"].(map[string]any)["status"] != "partial" {
		t.Fatal("counter reset must be partial")
	}
}

func TestClaudeUnifiedProjectPagination(t *testing.T) {
	f := newClaudeFixture(t)
	ctx := context.Background()
	p := f.server.pool
	for i := 0; i < 65; i++ {
		id := uuidClaude()
		cwd := "/project"
		if i == 64 {
			cwd = "/older-project"
		}
		if _, err := p.Exec(ctx, `INSERT INTO mira_claude_sessions(session_id,request_id,node_id,cwd,title) VALUES($1,$1,$2,$3,$4)`, id, f.nodeID, cwd, fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	first := f.call("GET", "/v1/claude/conversations?view=roots", nil)
	if len(first["data"].([]any)) != 50 || len(first["projects"].([]any)) != 2 {
		t.Fatalf("incomplete directory: %#v", first)
	}
	second := f.call("GET", "/v1/claude/conversations?view=roots&cursor="+url.QueryEscape(first["nextCursor"].(string)), nil)
	if len(second["data"].([]any)) != 15 || second["nextCursor"] != nil {
		t.Fatal("lost page")
	}
	seen := map[string]bool{}
	for _, page := range []map[string]any{first, second} {
		for _, raw := range page["data"].([]any) {
			id := raw.(map[string]any)["threadId"].(string)
			if seen[id] {
				t.Fatal("duplicate page")
			}
			seen[id] = true
		}
	}
	status, _ := f.request("GET", "/v1/claude/conversations?view=roots&archived=1&cursor="+url.QueryEscape(first["nextCursor"].(string)), nil, nil)
	if status != 400 {
		t.Fatal("cursor scope not checked")
	}
}

func TestClaudeProjectionCannotRejectNativeNUL(t *testing.T) {
	f := newClaudeFixture(t)
	id, turn, headers := f.reserved()
	event := map[string]any{"eventId": uuidClaude(), "payload": map[string]any{"type": "result", "total_cost_usd": 0, "unknown": "native\x00field"}}
	status, raw := f.request("POST", "/v1/claude/sessions/"+id+"/events", event, headers)
	if status != 200 {
		t.Fatalf("projection blocked raw storage: %d %s", status, raw)
	}
	var stored string
	if err := f.server.pool.QueryRow(context.Background(), `SELECT payload::text FROM mira_claude_events WHERE turn_id=$1`, turn).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, `\u0000`) {
		t.Fatalf("native data lost: %q", stored)
	}
}

package miraserver

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
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

func TestClaudeCostHistoryPreservesTurnsAndBoundsDenseDays(t *testing.T) {
	f := newClaudeFixture(t)
	ctx := context.Background()
	p := f.server.pool
	id, _, _ := f.reserved()
	account := uuidClaude()
	if _, err := p.Exec(ctx, `INSERT INTO mira_claude_accounts(node_account_id,node_id,name) VALUES($1,$2,'Curve')`, account, f.nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `DELETE FROM mira_claude_turns WHERE session_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(24 * time.Hour).Add(-23 * time.Hour)
	add := func(first, last int) {
		t.Helper()
		_, err := p.Exec(ctx, `WITH turns AS (
 INSERT INTO mira_claude_turns(turn_id,session_id,node_id,revision,request,node_account_id,account_name,created_at)
 SELECT gen_random_uuid(),$1,$2,n,'{}'::json,$3,'Curve',$4::timestamptz+n*interval '1 second'
 FROM generate_series($5::int,$6::int) n RETURNING turn_id,revision)
 INSERT INTO mira_claude_events(event_id,session_id,turn_id,event_type,payload)
 SELECT gen_random_uuid(),$1,turn_id,'result',json_build_object('type','result','total_cost_usd',revision/10.0) FROM turns`, id, f.nodeID, account, start, first, last)
		if err != nil {
			t.Fatal(err)
		}
	}
	history := func() map[string]any {
		return f.call("GET", "/v1/claude/accounts/cost-history?name=Curve&range=7d&timezone=UTC", nil)
	}
	add(1, 2)
	points := history()["points"].([]any)
	if len(points) != 2 {
		t.Fatalf("same-hour turns collapsed: %#v", points)
	}
	for i, raw := range points {
		point := raw.(map[string]any)
		if point["at"] != float64(start.Add(time.Duration(i+1)*time.Second).UnixMilli()) || point["amount"] != .1 || point["turnCount"] != float64(1) {
			t.Fatalf("turn time or delta lost: %#v", point)
		}
	}
	add(3, 260)
	data := history()
	points = data["points"].([]any)
	amount := data["estimate"].(map[string]any)["amount"].(float64)
	if len(points) > 128 || amount < 25.999999 || amount > 26.000001 {
		t.Fatalf("unbounded or incomplete history: points=%d amount=%v", len(points), amount)
	}
	var sum, turns float64
	var previous float64
	for _, raw := range points {
		point := raw.(map[string]any)
		at := point["at"].(float64)
		if at <= previous {
			t.Fatal("dense groups must remain chronological")
		}
		previous = at
		sum += point["amount"].(float64)
		turns += point["turnCount"].(float64)
	}
	if sum < 25.999999 || sum > 26.000001 || turns != 260 {
		t.Fatalf("dense grouping discarded costs: sum=%v turns=%v", sum, turns)
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
	// Imported Codex projects may have no execution Node yet. That is a valid
	// shared project key, but must not select every Claude session.
	unbound := f.call("GET", "/v1/claude/conversations?view=roots&projectKey="+url.QueryEscape(`["","/project"]`), nil)
	if len(unbound["data"].([]any)) != 0 || len(unbound["projects"].([]any)) != 2 {
		t.Fatal("unbound Codex project leaked Claude rows or lost the directory")
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

func TestClaudeRunningTurnCost(t *testing.T) {
	f := newClaudeFixture(t)
	ctx := context.Background()
	id, turn, _ := f.reserved()
	event := func(kind, raw string) {
		t.Helper()
		if _, err := f.server.pool.Exec(ctx, `INSERT INTO mira_claude_events(event_id,session_id,turn_id,event_type,payload) VALUES($1,$2,$3,$4,$5::json)`, uuidClaude(), id, turn, kind, raw); err != nil {
			t.Fatal(err)
		}
	}
	// Each content block repeats its response usage; only the latest snapshot counts.
	event("assistant", `{"type":"assistant","message":{"id":"msg_a","model":"claude-opus-5-5","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200}}}`)
	event("assistant", `{"type":"assistant","message":{"id":"msg_a","model":"claude-opus-5-5","usage":{"input_tokens":10,"output_tokens":100,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200,"cache_creation":{"ephemeral_5m_input_tokens":200,"ephemeral_1h_input_tokens":0}}}}`)
	event("assistant", `{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"id":"msg_b","model":"us.anthropic.claude-haiku-4-5-20251001-v1:0","usage":{"input_tokens":100,"output_tokens":50}}}`)
	event("assistant", `{"type":"assistant","message":{"id":"msg_c","model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0}}}`)
	near := func(value any, want float64) bool {
		got, ok := value.(float64)
		return ok && got > want-1e-9 && got < want+1e-9
	}
	// Opus 5.5: 10*$4 + 100*$20 + 1000*$0.20 + 200*$5 per MTok; Haiku 4.5: 100*$1 + 50*$5.
	const live = 0.00324 + 0.00035
	summary := f.call("GET", "/v1/claude/conversations/"+id, nil)
	cost := summary["costEstimate"].(map[string]any)
	activity := summary["activity"].(map[string]any)
	turnCost, _ := activity["costEstimate"].(map[string]any)
	if !near(cost["amount"], live) || cost["status"] != "complete" || cost["running"] != true || turnCost == nil || !near(turnCost["amount"], live) {
		t.Fatalf("running summary: %#v %#v", cost, activity)
	}
	if usage := summary["tokenUsage"].(map[string]any); usage["inputTokens"] != float64(1310) || usage["outputTokens"] != float64(150) {
		t.Fatalf("running usage: %#v", usage)
	}
	costs := f.call("GET", "/v1/claude/sessions/"+id+"/costs?turnId="+turn, nil)["turnCostEstimates"].(map[string]any)[turn].(map[string]any)
	if !near(costs["amount"], live) || costs["running"] != true {
		t.Fatalf("running turn cost: %#v", costs)
	}
	event("assistant", `{"type":"assistant","message":{"id":"msg_d","model":"gateway-model","usage":{"input_tokens":1,"output_tokens":1}}}`)
	costs = f.call("GET", "/v1/claude/sessions/"+id+"/costs?turnId="+turn, nil)["turnCostEstimates"].(map[string]any)[turn].(map[string]any)
	if !near(costs["amount"], live) || costs["status"] != "partial" {
		t.Fatalf("unpriced response must be partial: %#v", costs)
	}
	// The SDK result replaces the estimate even before the session releases the turn.
	event("result", `{"type":"result","total_cost_usd":0.5,"usage":{"input_tokens":1,"output_tokens":1},"modelUsage":{"claude":{"inputTokens":1,"outputTokens":1,"cacheReadInputTokens":0,"cacheCreationInputTokens":0}}}`)
	costs = f.call("GET", "/v1/claude/sessions/"+id+"/costs?turnId="+turn, nil)["turnCostEstimates"].(map[string]any)[turn].(map[string]any)
	summary = f.call("GET", "/v1/claude/conversations/"+id, nil)
	if !near(costs["amount"], .5) || costs["running"] != nil || !near(summary["costEstimate"].(map[string]any)["amount"], .5) || summary["activity"].(map[string]any)["costEstimate"] != nil {
		t.Fatalf("settled cost: %#v %#v", costs, summary)
	}
}

func TestClaudeModelPrice(t *testing.T) {
	for model, want := range map[string]int64{"claude-opus-5-5": 4_000, "claude-opus-5": 5_000, "claude-opus-4-1-20250805": 15_000,
		"anthropic.claude-sonnet-4-6": 3_000, "claude-fable-5-1": 10_000} {
		if price, ok := claudeModelPrice(model); !ok || price.input != want {
			t.Fatalf("%s: %#v %v", model, price, ok)
		}
	}
	if _, ok := claudeModelPrice("opus"); ok {
		t.Fatal("aliases need a resolved model")
	}
}

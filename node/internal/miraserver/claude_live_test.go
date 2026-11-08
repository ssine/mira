package miraserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClaudeEventBatchLiveRelay(t *testing.T) {
	f := newClaudeFixture(t)
	id, turn, headers := f.reserved()
	route := "/v1/claude/sessions/" + id
	ctx := context.Background()
	post := func(body any) map[string]any {
		t.Helper()
		status, raw := f.request("POST", route+"/events", body, headers)
		if status != 200 {
			t.Fatalf("batch: %d %s", status, raw)
		}
		var result map[string]any
		_ = json.Unmarshal(raw, &result)
		return result
	}
	// Subscribe before writing: the stream pushes live items and durable progress.
	r, _ := http.NewRequest("GET", f.endpoint+route+"/live?scope=root", nil)
	r.Header.Set("Cookie", f.cookie)
	stream, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != 200 || !strings.HasPrefix(stream.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("live stream: %d %s", stream.StatusCode, stream.Header.Get("Content-Type"))
	}
	events := make(chan [2]string, 64)
	go func() {
		scanner := bufio.NewScanner(stream.Body)
		var name string
		for scanner.Scan() {
			line := scanner.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				events <- [2]string{name, v}
			}
		}
		close(events)
	}()
	next := func(want string) map[string]any {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case e, ok := <-events:
				if !ok {
					t.Fatalf("stream closed waiting for %s", want)
				}
				if e[0] != want {
					continue
				}
				var data map[string]any
				_ = json.Unmarshal([]byte(e[1]), &data)
				return data
			case <-deadline:
				t.Fatalf("timed out waiting for %s", want)
			}
		}
	}
	next("durable") // initial position

	user := uuidClaude()
	batch := map[string]any{"items": []any{
		map[string]any{"eventId": user, "payload": map[string]any{"type": "mira_user", "text": "hi"}},
		map[string]any{"ephemeral": true, "payload": map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta"}}},
		map[string]any{"ephemeral": true, "payload": map[string]any{"type": "stream_event", "parent_tool_use_id": "toolu_child", "event": map[string]any{}}},
	}}
	first := post(batch)
	if again := post(batch); again["seq"] != first["seq"] {
		t.Fatalf("replayed batch moved the durable position: %v vs %v", again["seq"], first["seq"])
	}
	var stored, streamed int
	if err = f.server.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE event_type='stream_event') FROM mira_claude_events WHERE session_id=$1`, id).Scan(&stored, &streamed); err != nil {
		t.Fatal(err)
	}
	if stored != 1 || streamed != 0 {
		t.Fatalf("durable rows=%d stream rows=%d; deltas must not be stored and replays must not duplicate", stored, streamed)
	}
	live := next("live")["items"].([]any)
	item := live[0].(map[string]any)
	if len(live) != 1 || item["afterSeq"] != first["seq"] || item["turnId"] != turn {
		t.Fatalf("root live items: %#v (durable %v)", live, first["seq"])
	}
	if got := next("durable")["seq"]; got != first["seq"] {
		t.Fatalf("durable signal %v, want %v", got, first["seq"])
	}
	// The write itself is a liveness observation for the owning turn.
	f.server.claudeExecutionMu.Lock()
	observation := f.server.claudeExecution[id]
	f.server.claudeExecutionMu.Unlock()
	if observation.state != "running" || observation.turn != turn || time.Since(observation.checked) > time.Minute {
		t.Fatalf("write did not record a running observation: %#v", observation)
	}
	// An empty heartbeat batch keeps the observation fresh without storing anything.
	f.server.rememberClaudeExecution(claudeExecutionTarget{session: id, turn: turn, node: f.nodeID, revision: 1}, "unknown", "connection")
	post(map[string]any{"items": []any{}})
	f.server.claudeExecutionMu.Lock()
	observation = f.server.claudeExecution[id]
	f.server.claudeExecutionMu.Unlock()
	if observation.state != "running" {
		t.Fatalf("heartbeat observation: %#v", observation)
	}
	// Completion in a batch ends the turn and is not mistaken for liveness.
	f.server.rememberClaudeExecution(claudeExecutionTarget{session: id, turn: turn, node: f.nodeID, revision: 1}, "unknown", "connection")
	post(map[string]any{"items": []any{map[string]any{"eventId": uuidClaude(), "payload": map[string]any{"type": "mira_completed"}}}})
	var active *string
	var status string
	if err = f.server.pool.QueryRow(ctx, `SELECT s.active_turn::text,t.status FROM mira_claude_sessions s JOIN mira_claude_turns t ON t.session_id=s.session_id WHERE s.session_id=$1`, id).Scan(&active, &status); err != nil {
		t.Fatal(err)
	}
	f.server.claudeExecutionMu.Lock()
	observation = f.server.claudeExecution[id]
	f.server.claudeExecutionMu.Unlock()
	if active != nil || status != "completed" || observation.state != "unknown" {
		t.Fatalf("completion: active=%v status=%s observation=%#v", active, status, observation)
	}
	// Validation: durable items need UUIDs; legacy single events still work.
	if code, raw := f.request("POST", route+"/events", map[string]any{"items": []any{map[string]any{"payload": map[string]any{"type": "assistant"}}}}, headers); code != 400 {
		t.Fatalf("durable item without eventId: %d %s", code, raw)
	}
}

func TestClaudeLiveHubBounds(t *testing.T) {
	var hub claudeLiveHub
	hub.publish("s", "t1", 3, []claudeLiveItem{{Payload: json.RawMessage(`{"a":1}`), root: true}})
	items, reset, durable, next := hub.read("s", 0, true)
	if len(items) != 1 || reset || durable != 3 || next != 1 {
		t.Fatalf("read: %v %v %v %v", items, reset, durable, next)
	}
	// A new turn starts a new ring; a cursor that fell out of it reports reset.
	big := make([]claudeLiveItem, claudeLiveRingItems+10)
	for i := range big {
		big[i] = claudeLiveItem{Payload: json.RawMessage(`{}`), root: true}
	}
	hub.publish("s", "t2", 4, big)
	items, reset, _, _ = hub.read("s", 1, true)
	if !reset || len(items) != claudeLiveRingItems || items[0].TurnID != "t2" {
		t.Fatalf("bounded ring: reset=%v items=%d", reset, len(items))
	}
	// Root subscribers do not receive subagent deltas.
	hub.publish("s", "t2", 4, []claudeLiveItem{{Payload: json.RawMessage(`{}`), root: false}})
	_, _, _, last := hub.read("s", 0, false)
	items, _, _, _ = hub.read("s", last-1, true)
	if len(items) != 0 {
		t.Fatalf("subagent delta leaked to root view: %v", items)
	}
	// Subscriber capacity is bounded.
	cancels := []func(){}
	for range claudeLiveSubscribersMax {
		_, cancel, ok := hub.subscribe(fmt.Sprint("s", len(cancels)%3))
		if !ok {
			t.Fatal("subscriber rejected below the bound")
		}
		cancels = append(cancels, cancel)
	}
	if _, _, ok := hub.subscribe("s"); ok {
		t.Fatal("subscriber accepted above the bound")
	}
	for _, cancel := range cancels {
		cancel()
	}
}

func TestClaudeSubagentSummariesAndTitles(t *testing.T) {
	f := newClaudeFixture(t)
	id, _, headers := f.reserved()
	route := "/v1/claude/sessions/" + id
	appendEntries := func(subpath string, stamps ...string) {
		t.Helper()
		lines := ""
		for _, stamp := range stamps {
			lines += fmt.Sprintf(`{"type":"assistant","uuid":"%s","timestamp":"%s"}`+"\n", uuidClaude(), stamp)
		}
		status, raw := f.request("POST", route+"/entries?subpath="+subpath+"&operationId="+uuidClaude(), lines, headers)
		if status != 200 {
			t.Fatalf("append %s: %d %s", subpath, status, raw)
		}
	}
	appendEntries("", "2026-11-08T10:00:00Z")
	appendEntries("subagents/agent-a1", "2026-11-08T10:01:00Z", "2026-11-08T10:05:00Z")
	appendEntries("subagents/agent-a1", "2026-11-08T10:02:00.5Z") // out-of-order batch keeps the latest end
	appendEntries("subagents/agent-a2", "2026-11-08T11:00:00Z")
	summary := f.call("GET", "/v1/claude/conversations/"+id, nil)
	if summary["subagentCount"] != float64(2) || summary["childCount"] != float64(2) {
		t.Fatalf("root summary counts: %v %v", summary["subagentCount"], summary["childCount"])
	}
	children := f.call("GET", "/v1/claude/conversations?view=children&parentThreadId="+id, nil)["data"].([]any)
	times := map[string][2]string{}
	for _, raw := range children {
		child := raw.(map[string]any)
		times[child["subpath"].(string)] = [2]string{child["createdAt"].(string), child["updatedAt"].(string)}
		if child["parentThreadId"] != id || child["listRoot"] != false {
			t.Fatalf("child grouping: %#v", child)
		}
	}
	at := func(value, want string) bool {
		got, err1 := time.Parse(time.RFC3339Nano, value)
		expected, err2 := time.Parse(time.RFC3339Nano, want)
		return err1 == nil && err2 == nil && got.Equal(expected)
	}
	if got := times["subagents/agent-a1"]; !at(got[0], "2026-11-08T10:01:00Z") || !at(got[1], "2026-11-08T10:05:00Z") {
		t.Fatalf("agent-a1 times: %v", got)
	}
	if got := times["subagents/agent-a2"]; !at(got[1], "2026-11-08T11:00:00Z") {
		t.Fatalf("agent-a2 times: %v", got)
	}
	// Title generation needs a capable Node and a bounded prompt.
	if status, raw := f.request("POST", "/v1/claude/runtimes/"+f.nodeID+"/title", map[string]any{"prompt": "x"}, nil); status != 409 {
		t.Fatalf("title without claudeTitleV1: %d %s", status, raw)
	}
	if _, err := f.server.pool.Exec(context.Background(), `UPDATE codex_nodes SET capabilities=capabilities||'{"claudeTitleV1":true}' WHERE node_id=$1`, f.nodeID); err != nil {
		t.Fatal(err)
	}
	if status, raw := f.request("POST", "/v1/claude/runtimes/"+f.nodeID+"/title", map[string]any{"prompt": ""}, nil); status != 400 {
		t.Fatalf("empty title prompt: %d %s", status, raw)
	}
	// A generated title is compared with the title it was based on.
	f.call("PATCH", route, map[string]any{"title": "manual"})
	if status, raw := f.request("PATCH", route, map[string]any{"title": "generated", "expectedTitle": "old"}, nil); status != 409 {
		t.Fatalf("stale generated title: %d %s", status, raw)
	}
	f.call("PATCH", route, map[string]any{"title": "generated", "expectedTitle": "manual"})
	if got := f.call("GET", route, nil)["title"]; got != "generated" {
		t.Fatalf("title: %v", got)
	}
}

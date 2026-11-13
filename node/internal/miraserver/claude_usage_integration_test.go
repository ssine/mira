package miraserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestClaudePlanUsageRoutesSamplerAndHistory(t *testing.T) {
	f := newClaudeFixture(t)
	ctx := context.Background()
	if _, err := f.server.pool.Exec(ctx, `UPDATE codex_nodes SET capabilities=capabilities||'{"claudeUsageV1":true}' WHERE node_id=$1`, f.nodeID); err != nil {
		t.Fatal(err)
	}
	dialer := websocket.Dialer{Subprotocols: []string{"mira-node-v1", "auth." + base64.RawURLEncoding.EncodeToString([]byte(f.token))}}
	ws, _, err := dialer.Dial("ws"+strings.TrimPrefix(f.endpoint, "http")+"/v1/nodes/"+f.nodeID+"/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var usageReads atomic.Int32
	var weeklyUsed atomic.Int32
	weeklyUsed.Store(4)
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
				result = map[string]any{"configured": true, "authType": params["authType"]}
			case "account/usage":
				usageReads.Add(1)
				result = map[string]any{"subscriptionType": "max", "available": true,
					"fiveHour": map[string]any{"utilization": 12.5, "resetsAt": time.Now().Add(3 * time.Hour).UnixMilli()},
					"sevenDay": map[string]any{"utilization": weeklyUsed.Load(), "resetsAt": time.Now().Add(96 * time.Hour).UnixMilli()}}
			default:
				t.Errorf("unexpected Claude action: %v", params["action"])
			}
			writes.Lock()
			_ = ws.WriteJSON(map[string]any{"type": "response", "requestId": message["requestId"], "ok": true, "result": result})
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
	provider := map[string]any{"id": "anthropic", "baseUrl": "https://api.anthropic.com", "model": "opus"}
	id := f.call("POST", "/v1/claude/accounts", map[string]any{"nodeId": f.nodeID, "name": "Max", "authType": "subscription"})["nodeAccountId"].(string)
	f.call("POST", "/v1/claude/accounts/"+id+"/configure", map[string]any{"provider": provider, "authType": "subscription"})
	apiID := f.call("POST", "/v1/claude/accounts", map[string]any{"nodeId": f.nodeID, "name": "Gateway"})["nodeAccountId"].(string)
	f.call("POST", "/v1/claude/accounts/"+apiID+"/configure", map[string]any{"provider": provider, "apiKey": "fixture-key"})

	if status, raw := f.request("GET", "/v1/claude/accounts/"+apiID+"/usage", nil, nil); status != 409 {
		t.Fatalf("API-key usage: %d %s", status, raw)
	}
	live := f.call("GET", "/v1/claude/accounts/"+id+"/usage", nil)
	usage, _ := live["usage"].(map[string]any)
	if live["stale"] != false || usage["subscriptionType"] != "max" || usage["sevenDay"].(map[string]any)["utilization"] != float64(4) {
		t.Fatalf("live usage: %#v", live)
	}
	f.call("GET", "/v1/claude/accounts/"+id+"/usage", nil)
	if usageReads.Load() != 1 {
		t.Fatalf("browser reads share the cached result: %d Node reads", usageReads.Load())
	}

	// The sampler reuses the recent read and records one sample per interval.
	if err = f.server.sampleClaudeUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.server.sampleClaudeUsage(ctx); err != nil {
		t.Fatal(err)
	}
	var samples int
	if err = f.server.pool.QueryRow(ctx, `SELECT count(*) FROM mira_claude_account_usage_samples WHERE node_account_id=$1`, id).Scan(&samples); err != nil {
		t.Fatal(err)
	}
	if samples != 1 || usageReads.Load() != 1 {
		t.Fatalf("sampler: samples=%d reads=%d", samples, usageReads.Load())
	}
	// Older observations: an earlier failure gap and a higher remaining value.
	old := time.Now().Add(-20 * time.Minute)
	if _, err = f.server.pool.Exec(ctx, `INSERT INTO mira_claude_account_usage_samples(node_account_id,sample_slot,sampled_at,credential_revision,status,usage)
	  VALUES($1,$2,$3,1,'ok','{"available":true,"sevenDay":{"utilization":1,"resetsAt":1800000000000},"fiveHour":{"utilization":50}}'),
	        ($1,$4,$5,1,'error',NULL)`, id, old.UnixMilli()/300000, old, old.Add(10*time.Minute).UnixMilli()/300000, old.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	history := f.call("GET", "/v1/claude/accounts/"+id+"/usage-history?range=24h", nil)
	points, _ := history["points"].([]any)
	raw, _ := json.Marshal(points)
	if len(points) != 3 || history["intervalMs"] != float64(300000) {
		t.Fatalf("history: %s", raw)
	}
	first, gap, latest := points[0].(map[string]any), points[1].(map[string]any), points[2].(map[string]any)
	if first["remaining"] != float64(99) || first["session"] != float64(50) || first["resetsAt"] != float64(1800000000000) ||
		gap["status"] != "error" || gap["remaining"] != nil || latest["remaining"] != float64(96) || latest["session"] != float64(87.5) {
		t.Fatalf("history points: %s", raw)
	}
	if status, raw := f.request("GET", "/v1/claude/accounts/"+id+"/usage-history?range=1y", nil, nil); status != 400 {
		t.Fatalf("invalid range: %d %s", status, raw)
	}

	// Without a live read, the latest stored sample is shown as stale.
	f.server.claudePlanUsage.mu.Lock()
	f.server.claudePlanUsage.entries = nil
	f.server.claudePlanUsage.mu.Unlock()
	_ = ws.Close()
	for f.server.channel.IsConnected(f.nodeID) {
		time.Sleep(10 * time.Millisecond)
	}
	stale := f.call("GET", "/v1/claude/accounts/"+id+"/usage", nil)
	if stale["stale"] != true || stale["message"] != "Claude account Node is offline" || stale["usage"].(map[string]any)["sevenDay"].(map[string]any)["utilization"] != float64(4) {
		t.Fatalf("offline fallback: %#v", stale)
	}
}

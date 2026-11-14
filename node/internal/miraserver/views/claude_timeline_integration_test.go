package views

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/coldstore"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

type timelineObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (store *timelineObjects) Put(_ context.Context, key string, body []byte) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.objects[key] = append([]byte(nil), body...)
	return nil
}

func (store *timelineObjects) Get(_ context.Context, key string) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	body, ok := store.objects[key]
	if !ok {
		return nil, fmt.Errorf("missing object %s", key)
	}
	return body, nil
}

func (store *timelineObjects) Delete(_ context.Context, key string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.objects, key)
	return nil
}

// The Claude timeline reads turns, Mira prompts, assistant tool calls and
// tool results; a frozen (cold) tool result still closes its tool interval.
func TestClaudeTimelinePostgres(t *testing.T) {
	databaseURL := os.Getenv("MIRA_VIEWS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set MIRA_VIEWS_TEST_DATABASE_URL to run the Claude timeline test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	suffix := time.Now().UnixNano() % 1_000_000_000_000
	nodeID := fmt.Sprintf("40000000-0000-4000-8000-%012d", suffix)
	sessionID := fmt.Sprintf("41000000-0000-4000-8000-%012d", suffix)
	turnID := fmt.Sprintf("42000000-0000-4000-8000-%012d", suffix)
	exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations)
	  VALUES($1::uuid,$1::text,'timeline-test','linux','amd64','linux','test','{}','[]')`, nodeID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM mira_claude_sessions WHERE session_id=$1`, sessionID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM codex_nodes WHERE node_id=$1`, nodeID)
	})
	start := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	exec(`INSERT INTO mira_claude_sessions(session_id,request_id,node_id,cwd,title) VALUES($1,gen_random_uuid(),$2,'/work','时间线测试')`, sessionID, nodeID)
	exec(`INSERT INTO mira_claude_turns(turn_id,session_id,node_id,revision,request,status,created_at,completed_at)
	  VALUES($1,$2,$3,1,'{}','completed',$4,$5)`, turnID, sessionID, nodeID, start, start.Add(time.Minute))
	at := func(seconds int) string { return start.Add(time.Duration(seconds) * time.Second).Format(time.RFC3339Nano) }
	events := []map[string]any{
		{"type": "mira_user", "text": "检查测试", "timestamp": at(0)},
		{"type": "assistant", "timestamp": at(5), "message": map[string]any{"id": "m1", "content": []any{
			map[string]any{"type": "text", "text": "先跑测试"}, map[string]any{"type": "tool_use", "id": "tool-1", "name": "Bash", "input": map[string]any{"command": "go test"}}}}},
		{"type": "user", "timestamp": at(25), "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": "ok"}}}},
		{"type": "assistant", "timestamp": at(50), "message": map[string]any{"id": "m2", "content": []any{map[string]any{"type": "text", "text": "测试通过"}}}},
	}
	objects := &timelineObjects{objects: map[string][]byte{}}
	store, err := coldstore.New(objects, coldstore.Options{CacheDir: t.TempDir(), CacheBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	coldstore.SetDefault(store)
	t.Cleanup(func() { coldstore.SetDefault(nil) })
	for index, event := range events {
		raw, _ := json.Marshal(event)
		var cold *string
		payload := string(raw)
		if event["type"] == "user" {
			// Store the tool result like the freezer: object in the bucket, stub in the row.
			sha := coldstore.Digest(raw)
			encoded, err := coldstore.Encode(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := objects.Put(ctx, coldstore.ObjectKey(sha), encoded); err != nil {
				t.Fatal(err)
			}
			cold = &sha
			payload = fmt.Sprintf(`{"type":"user","mira_cold_payload":{"version":1,"sha256":%q,"bytes":%d}}`, sha, len(raw))
		}
		exec(`INSERT INTO mira_claude_events(event_id,session_id,turn_id,event_type,payload,cold_sha256)
		  VALUES(gen_random_uuid(),$1,$2,$3,$4::json,$5)`, sessionID, turnID, event["type"], payload, cold)
		_ = index
	}
	result, err := New(pool).GetClaudeTimeline(ctx, sessionID)
	if err != nil || result.Status != 200 {
		t.Fatalf("timeline: %v %#v", err, result.Body)
	}
	lanes := result.Body["lanes"].([]*timelineLane)
	if len(lanes) != 1 || len(lanes[0].Turns) != 1 || len(lanes[0].Tools) != 1 || len(lanes[0].Messages) != 3 {
		t.Fatalf("lanes: %#v", lanes)
	}
	tool := lanes[0].Tools[0]
	if tool.Open || tool.End-tool.Start != 20_000 || tool.Name != "Bash" {
		t.Fatalf("the frozen tool result must close the tool interval: %#v", tool)
	}
}

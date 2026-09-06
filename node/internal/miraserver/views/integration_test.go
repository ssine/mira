package views

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func TestPostgresReadViews(t *testing.T) {
	databaseURL := os.Getenv("MIRA_VIEWS_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set MIRA_VIEWS_TEST_DATABASE_URL to run the PostgreSQL read views")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	operationID := "6a779dbb-5d85-49b8-af53-e4d8155898aa"
	threadID := "10000000-0000-4000-8000-000000000001"
	if _, err := pool.Exec(ctx, `INSERT INTO codex_store_heads(store_id,version,history_floor) VALUES('personal',1,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO codex_store_events(store_id,operation_id,event_seq,previous_event_seq,result_version,appended_item_count)
		VALUES('personal',$1,1,0,1,6)`, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO codex_thread_projections(store_id,thread_id,active_generation,item_count,title,state,through_event_seq)
		VALUES('personal',$1,1,6,'Integration thread',$2::jsonb,1)`, threadID,
		`{"metadata":{"created_at":"2026-09-05T10:00:00.000Z","updated_at":"2026-09-05T10:00:06.000Z"}}`); err != nil {
		t.Fatal(err)
	}
	items := []map[string]any{
		contextRecord("gpt-6-astra", "turn"),
		record("event_msg", map[string]any{"type": "task_started", "turn_id": "turn", "started_at": float64(1788602400)}),
		record("event_msg", map[string]any{"type": "user_message", "turn_id": "turn", "message": "Inspect"}),
		record("event_msg", map[string]any{"type": "agent_message", "turn_id": "turn", "message": "Done"}),
		usageRecord(usage(100000, 80000, 1000), usage(100000, 80000, 1000), "turn"),
		record("event_msg", map[string]any{"type": "task_complete", "turn_id": "turn", "completed_at": float64(1788602406)}),
	}
	for index, item := range items {
		payload, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO codex_thread_events(store_id,thread_id,generation,item_seq,event_format_version,payload,payload_sha256,operation_id)
		VALUES('personal',$1,1,$2,1,$3::jsonb,'unused',$4)`, threadID, index+1, string(payload), operationID); err != nil {
			t.Fatal(err)
		}
	}
	service := New(pool)
	threads, err := service.ListThreads(ctx, "personal", 10, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].Title == nil || *threads[0].Title != "Integration thread" || threads[0].Activity["state"] != "idle" || threads[0].ReadState["unread"] != true {
		t.Fatalf("threads: %#v", threads)
	}
	if threads[0].TokenUsage["inputTokens"] != int64(100000) {
		t.Fatalf("usage: %#v", threads[0].TokenUsage)
	}
	cost, err := service.GetThreadCost(ctx, "personal", threads[0])
	if err != nil {
		t.Fatal(err)
	}
	if !closeFloat(cost["amount"].(float64), .33) {
		t.Fatalf("cost: %#v", cost)
	}
	tail, err := service.GetTranscript(ctx, "personal", threadID, TranscriptOptions{Tail: true, Limit: 60})
	if err != nil {
		t.Fatal(err)
	}
	if tail.Status != 200 || len(array(tail.Body["trace"])) != 2 || tail.Body["nextCursor"] != nil {
		t.Fatalf("tail: %#v", tail)
	}
	legacy, err := service.GetTranscript(ctx, "personal", threadID, TranscriptOptions{Limit: 60})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Status != 200 || len(array(legacy.Body["trace"])) != 2 {
		t.Fatalf("legacy: %#v", legacy)
	}
}

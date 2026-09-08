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
		VALUES('personal',$1,1,0,1,100)`, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO codex_thread_projections(store_id,thread_id,active_generation,item_count,title,state,through_event_seq)
		VALUES('personal',$1,1,100,'Integration thread',$2::jsonb,1)`, threadID,
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
	// Keep more than nine rows in the fixture. PostgreSQL resolves an unqualified
	// ORDER BY item_seq to the selected item_seq::text output column, which sorts
	// 100 before 99 and used to make every existing long transcript look corrupt.
	for len(items) < 100 {
		items = append(items, record("unrecognized_fixture_record", map[string]any{"index": len(items) + 1}))
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
	t.Run("reasoning effort follows canonical settings in the active generation", func(t *testing.T) {
		// A metadata model must not prevent effort enrichment. Records are
		// deliberately beyond ordinal 9 to exercise numeric reverse ordering.
		if _, err := pool.Exec(ctx, `UPDATE codex_thread_projections SET state=jsonb_set(state,'{metadata,model}','"gpt-6-astra"') WHERE thread_id=$1`, threadID); err != nil {
			t.Fatal(err)
		}
		defer pool.Exec(ctx, `UPDATE codex_thread_projections SET state=state #- '{metadata,model}' WHERE thread_id=$1`, threadID)
		for _, entry := range []struct {
			seq  int
			item map[string]any
		}{
			{90, record("turn_context", map[string]any{"effort": "ultra"})},
			{98, record("turn_context", map[string]any{"effort": "medium"})},
			{99, record("event_msg", map[string]any{"type": "thread_settings_applied", "thread_settings": map[string]any{"reasoning_effort": "xhigh"}})},
			{100, record("turn_context", map[string]any{"model": "gpt-6-astra"})},
		} {
			raw, _ := json.Marshal(entry.item)
			if _, err := pool.Exec(ctx, `UPDATE codex_thread_events SET payload=$1::json WHERE thread_id=$2 AND generation=1 AND item_seq=$3`, string(raw), threadID, entry.seq); err != nil {
				t.Fatal(err)
			}
			defer pool.Exec(ctx, `UPDATE codex_thread_events SET payload='{"type":"unrecognized_fixture_record"}'::json WHERE thread_id=$1 AND generation=1 AND item_seq=$2`, threadID, entry.seq)
		}
		for _, id := range []*string{nil, &threadID} {
			got, err := service.ListThreads(ctx, "personal", 10, id, nil)
			if err != nil || len(got) != 1 {
				t.Fatalf("threads: %#v %v", got, err)
			}
			if got[0].ReasoningEffort == nil || *got[0].ReasoningEffort != "xhigh" {
				t.Fatalf("effort: %v", got[0].ReasoningEffort)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE codex_thread_projections SET active_generation=2 WHERE thread_id=$1`, threadID); err != nil {
			t.Fatal(err)
		}
		defer pool.Exec(ctx, `UPDATE codex_thread_projections SET active_generation=1 WHERE thread_id=$1`, threadID)
		got, err := service.ListThreads(ctx, "personal", 10, &threadID, nil)
		if err != nil || len(got) != 1 || got[0].ReasoningEffort != nil {
			t.Fatalf("old generation effort leaked: %#v %v", got, err)
		}
	})
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
	t.Run("history images paginate and resolve independently", func(t *testing.T) {
		content := []any{map[string]any{"type": "input_text", "text": "images"}}
		for i := 0; i < 15; i++ {
			content = append(content, map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"})
		}
		raw, _ := json.Marshal(record("response_item", map[string]any{"type": "message", "role": "user", "content": content}))
		if _, err := pool.Exec(ctx, `UPDATE codex_thread_events SET payload=$1::json WHERE store_id='personal' AND thread_id=$2 AND item_seq=95`, string(raw), threadID); err != nil {
			t.Fatal(err)
		}
		loaded := false
		page, err := service.GetTranscript(ctx, "personal", threadID, TranscriptOptions{Tail: true, Limit: 10, ToolDetails: &loaded})
		if err != nil || page.Status != 200 {
			t.Fatalf("page: %#v %v", page, err)
		}
		images := 0
		for _, value := range array(page.Body["trace"]) {
			if object(value)["kind"] == "image" {
				images++
			}
		}
		if images != 15 {
			t.Fatalf("page boundary lost images: %d %#v", images, page)
		}
		encoded, _ := json.Marshal(page.Body)
		if contains(string(encoded), "base64") {
			t.Fatal("summary contains image bytes")
		}
		cursor := stringValue(page.Body["nextCursor"])
		older, err := service.GetTranscript(ctx, "personal", threadID, TranscriptOptions{Tail: true, Limit: 10, Cursor: &cursor})
		if err != nil || older.Status != 200 {
			t.Fatalf("older: %#v %v", older, err)
		}
		for _, value := range array(older.Body["trace"]) {
			if object(value)["kind"] == "image" {
				t.Fatal("older page repeated images")
			}
		}
		image, err := service.GetTranscriptImage(ctx, "personal", threadID, 1, 95, 15)
		if err != nil || image.Status != 200 || image.Body["url"] != "data:image/png;base64,AAAA" {
			t.Fatalf("image: %#v %v", image, err)
		}
		for _, args := range [][3]int64{{2, 95, 1}, {1, 95, 0}, {1, 95, 16}, {1, 94, 1}} {
			result, err := service.GetTranscriptImage(ctx, "personal", threadID, args[0], args[1], args[2])
			if err != nil || result.Status != 404 {
				t.Fatalf("invalid reference: %#v %v", result, err)
			}
		}
		other, err := service.GetTranscriptImage(ctx, "other", threadID, 1, 95, 1)
		if err != nil || other.Status != 404 {
			t.Fatal("image crossed store boundary")
		}
		if _, err := pool.Exec(ctx, `UPDATE codex_thread_projections SET active_generation=2 WHERE store_id='personal' AND thread_id=$1`, threadID); err != nil {
			t.Fatal(err)
		}
		stale, err := service.GetTranscriptImage(ctx, "personal", threadID, 1, 95, 1)
		if err != nil || stale.Status != 404 {
			t.Fatal("old generation image remained accessible")
		}
		if _, err := pool.Exec(ctx, `UPDATE codex_thread_projections SET active_generation=1 WHERE store_id='personal' AND thread_id=$1`, threadID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO mira_thread_actions(store_id,thread_id,action,operation_id,generation) VALUES('personal',$1,'delete','d079b7da-4072-4c02-b012-1eb9001fc211',1)`, threadID); err != nil {
			t.Fatal(err)
		}
		deleted, err := service.GetTranscriptImage(ctx, "personal", threadID, 1, 95, 1)
		if err != nil || deleted.Status != 404 {
			t.Fatal("deleted thread image remained accessible")
		}

	})

}

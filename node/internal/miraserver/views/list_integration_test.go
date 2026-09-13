package views

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func TestPostgresListProjectionMetadata(t *testing.T) {
	endpoint := os.Getenv("MIRA_VIEWS_TEST_DATABASE_URL")
	if endpoint == "" {
		t.Skip("set MIRA_VIEWS_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := "list-metadata-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		exec(`DELETE FROM mira_thread_actions WHERE store_id=$1`, store)
		exec(`DELETE FROM codex_thread_projections WHERE store_id IN ($1,$2)`, store, store+"-other")
	})
	insert := func(target, id string, state map[string]any) {
		t.Helper()
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO codex_thread_projections(store_id,thread_id,active_generation,item_count,title,state,through_event_seq)
 VALUES($1,$2,1,0,'Fallback title',$3::jsonb,1)`, target, id, string(raw))
	}
	insert(store, "renamed", map[string]any{
		"name": "Current title",
		"metadata": map[string]any{
			"model": "gpt-6-astra", "token_usage": map[string]any{"input_tokens": 123},
			"updated_at": "2026-09-99T10:00:00Z", "advance_recency_at": "2026-09-06T10:00:00Z",
			"created_at": "2026-09-04T10:00:00Z",
		},
		"createdThread": map[string]any{
			"forked_from_id": "parent", "metadata": map[string]any{"timestamp": "2026-09-03T10:00:00Z"},
			// Runtime instructions make real projections large enough to be toasted.
			"developerInstructions": strings.Repeat("large runtime instructions; ", 10000),
		},
	})
	insert(store, "empty-name", map[string]any{
		"name": "", "createdThread": nil,
		"metadata": map[string]any{
			"updated_at": "2026-09-05T10:00:00Z", "advance_recency_at": "2026-09-08T10:00:00Z",
			"created_at": "2026-09-04T10:00:00Z",
		},
	})
	insert(store, "missing-fields", map[string]any{})
	insert(store, "archived", map[string]any{"metadata": map[string]any{"created_at": "2026-09-10T10:00:00Z"}})
	insert(store+"-other", "renamed", map[string]any{"name": "Other store"})
	exec(`INSERT INTO mira_thread_actions(store_id,thread_id,action,operation_id,generation,item_count)
 VALUES($1,'archived','archive','91b4f242-e57a-4cc4-869b-2e8e0742e00c',1,0)`, store)
	service := New(pool)
	archived := false
	threads, err := service.ListThreads(ctx, store, 2, nil, &archived)
	if err != nil || len(threads) != 2 {
		t.Fatalf("list: %#v %v", threads, err)
	}
	first, second := threads[0], threads[1]
	if first.ThreadID != "renamed" || second.ThreadID != "empty-name" {
		t.Fatalf("recency, archive or limit filtering changed: %#v", threads)
	}
	if first.Title == nil || *first.Title != "Current title" || first.Name == nil || *first.Name != "Current title" || first.ForkedFromID == nil || *first.ForkedFromID != "parent" || first.CreatedAt == nil || *first.CreatedAt != "2026-09-03T10:00:00Z" {
		t.Fatalf("created metadata: %#v", first)
	}
	if first.Model == nil || *first.Model != "gpt-6-astra" || first.TokenUsage["inputTokens"] != int64(123) || first.UpdatedAt == nil || !strings.HasPrefix(*first.UpdatedAt, "2026-09-06T10:00:00") {
		t.Fatalf("current metadata: %#v", first)
	}
	if second.Title == nil || *second.Title != "Fallback title" || second.Name == nil || *second.Name != "" || second.CreatedAt == nil || *second.CreatedAt != "2026-09-04T10:00:00Z" {
		t.Fatalf("fallback metadata: %#v", second)
	}
	id := "missing-fields"
	missing, err := service.ListThreads(ctx, store, 1, &id, nil)
	if err != nil || len(missing) != 1 || missing[0].ThreadID != id || missing[0].Name != nil || missing[0].CreatedAt != nil || missing[0].UpdatedAt != nil || missing[0].Model != nil || missing[0].TokenUsage != nil {
		t.Fatalf("missing metadata or targeted lookup: %#v %v", missing, err)
	}
	archived = true
	archivedThreads, err := service.ListThreads(ctx, store, 1, nil, &archived)
	if err != nil || len(archivedThreads) != 1 || archivedThreads[0].ThreadID != "archived" || !archivedThreads[0].Archived {
		t.Fatalf("archive lookup: %#v %v", archivedThreads, err)
	}
}

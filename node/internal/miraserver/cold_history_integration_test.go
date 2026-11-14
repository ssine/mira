package miraserver

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/coldstore"
	serverviews "github.com/ssine/mira/node/internal/miraserver/views"
)

type coldTestObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (store *coldTestObjects) Put(_ context.Context, key string, body []byte) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.objects[key] = append([]byte(nil), body...)
	return nil
}

func (store *coldTestObjects) Get(_ context.Context, key string) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	body, ok := store.objects[key]
	if !ok {
		return nil, coldstore.ErrObjectNotFound
	}
	return append([]byte(nil), body...), nil
}

func (store *coldTestObjects) Delete(_ context.Context, key string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.objects, key)
	return nil
}

func (store *coldTestObjects) count() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.objects)
}

func randomText(t *testing.T, size int) string {
	t.Helper()
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// Freezing moves only large display bodies, and every reader returns exactly
// the canonical history afterwards; metadata views never see a placeholder.
func TestColdHistoryRoundTrip(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	uuid := func() string {
		id, err := randomUUID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	storeID, threadID := "cold-"+uuid(), uuid()
	image := "data:image/png;base64," + randomText(t, 48<<10)
	shared := randomText(t, 40<<10)
	history := []any{
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": threadID, "cwd": "/work", "instructions": randomText(t, 30<<10)}},
		map[string]any{"type": "turn_context", "payload": map[string]any{"model": "gpt-test", "cwd": "/work", "turn_id": "turn-1"}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "turn-1"}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "请截图"}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "call_id": "call-1", "name": "exec", "input": "screenshot()"}},
		map[string]any{"timestamp": "2026-09-01T00:00:00.000Z", "type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "call-1", "output": []any{
			map[string]any{"type": "input_text", "text": "Script completed\n"},
			map[string]any{"type": "input_image", "image_url": image, "detail": "high"},
			map[string]any{"type": "input_text", "text": "nul \u0000 and ü"},
		}}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "call-2", "output": shared}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "call-2", "output": shared}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "call-3", "output": randomText(t, 30<<10), "echo": map[string]any{"type": "task_complete"}}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12}}}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "完成 " + randomText(t, 30<<10)}}}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "agent_message", "message": "完成"}},
		map[string]any{"type": "compacted", "payload": map[string]any{"message": "summary", "replacement_history": []any{map[string]any{"type": "message", "role": "user", "content": randomText(t, 30<<10)}}}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": "turn-1"}},
	}
	created, err := PutSnapshot(ctx, pool, storeID, map[string]any{"expectedVersion": 0, "snapshot": map[string]any{
		"created_threads": map[string]any{threadID: map[string]any{"thread_id": threadID}},
		"histories":       map[string]any{threadID: history},
	}}, http.Header{"X-Codex-Operation-Id": []string{uuid()}})
	if err != nil || created.Status != 200 {
		t.Fatalf("create: %+v %v", created, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE codex_thread_events SET created_at=now()-interval '40 days' WHERE store_id=$1`, storeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE codex_thread_projections SET updated_at=now()-interval '40 days' WHERE store_id=$1`, storeID); err != nil {
		t.Fatal(err)
	}
	views := serverviews.New(pool)
	server := &Server{pool: pool}
	tool := true
	capture := func() map[string]string {
		result := map[string]string{}
		encode := func(name string, value any) {
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			result[name] = string(raw)
		}
		history, err := GetThreadHistory(ctx, pool, storeID, threadID, nil, nil)
		if err != nil || history.Status != 200 {
			t.Fatalf("history: %+v %v", history, err)
		}
		encode("history", history.Body)
		events, err := ListThreadEvents(ctx, pool, storeID, threadID, nil, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		encode("events", events)
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/history", nil)
		if err := server.streamThreadHistory(ctx, recorder, request, storeID, threadID, nil, nil); err != nil {
			t.Fatal(err)
		}
		result["stream"] = recorder.Body.String()
		for name, options := range map[string]serverviews.TranscriptOptions{
			"legacy": {Limit: 100, ToolDetails: &tool}, "tail": {Tail: true, Limit: 100, ToolDetails: &tool},
		} {
			transcript, err := views.GetTranscript(ctx, storeID, threadID, options)
			if err != nil || transcript.Status != 200 {
				t.Fatalf("%s transcript: %+v %v", name, transcript.Status, err)
			}
			encode(name, transcript.Body)
		}
		picture, err := views.GetTranscriptImage(ctx, storeID, threadID, 1, 6, 1)
		if err != nil || picture.Status != 200 {
			t.Fatalf("image: %+v %v", picture, err)
		}
		encode("image", picture.Body)
		threads, err := views.ListThreads(ctx, storeID, 10, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		encode("threads", threads)
		if _, err := pool.Exec(ctx, `DELETE FROM mira_conversation_index_state WHERE store_id=$1`, storeID); err != nil {
			t.Fatal(err)
		}
		if complete, err := views.IndexCodexThread(ctx, storeID, threadID, time.Time{}); err != nil || !complete {
			t.Fatalf("index: %v %v", complete, err)
		}
		var records string
		if err := pool.QueryRow(ctx, `SELECT json_agg(json_build_array(item_seq,kind,record::text) ORDER BY item_seq)::text
 FROM mira_conversation_records WHERE store_id=$1`, storeID).Scan(&records); err != nil {
			t.Fatal(err)
		}
		result["index"] = records
		return result
	}
	before := capture()
	if !strings.Contains(before["image"], image[:64]) {
		t.Fatal("fixture image missing")
	}

	objects := &coldTestObjects{objects: map[string][]byte{}}
	store, err := coldstore.New(objects, coldstore.Options{CacheDir: t.TempDir(), CacheBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	coldstore.SetDefault(store)
	defer coldstore.SetDefault(nil)
	freezer := coldstore.NewFreezer(pool, objects, coldstore.FreezerOptions{FreezeAfter: 24 * time.Hour, MinBytes: 4 << 10, OrphanGrace: time.Hour, UploadConcurrency: 8})
	// Until the conversation index covers the conversation, nothing moves.
	if _, err := pool.Exec(ctx, `DELETE FROM mira_conversation_index_state WHERE store_id=$1`, storeID); err != nil {
		t.Fatal(err)
	}
	if early, err := freezer.FreezePass(ctx, 10); err != nil || early.Threads != 0 {
		t.Fatalf("froze before indexing: %+v %v", early, err)
	}
	if complete, err := views.IndexCodexThread(ctx, storeID, threadID, time.Time{}); err != nil || !complete {
		t.Fatalf("index: %v %v", complete, err)
	}
	stats, err := freezer.FreezePass(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	frozen := map[int64]bool{}
	rows, err := pool.Query(ctx, `SELECT item_seq FROM codex_thread_events WHERE store_id=$1 AND cold_sha256 IS NOT NULL`, storeID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		frozen[seq] = true
	}
	rows.Close()
	// Tool output with the image, both copies of the shared output and the
	// compaction move; session metadata, the assistant message and the output
	// that mentions a lifecycle type stay hot.
	if len(frozen) != 4 || !frozen[6] || !frozen[7] || !frozen[8] || !frozen[13] || stats.Rows != 4 {
		t.Fatalf("frozen rows %v stats %+v", frozen, stats)
	}
	if objects.count() != 3 {
		t.Fatalf("objects: %d (identical payloads must share one)", objects.count())
	}
	var placeholderBytes int
	if err := pool.QueryRow(ctx, `SELECT max(octet_length(payload::text)) FROM codex_thread_events WHERE store_id=$1 AND cold_sha256 IS NOT NULL`, storeID).Scan(&placeholderBytes); err != nil || placeholderBytes > 400 {
		t.Fatalf("placeholder size %d %v", placeholderBytes, err)
	}
	if again, err := freezer.FreezePass(ctx, 10); err != nil || again.Threads != 0 {
		t.Fatalf("unchanged conversation rescanned: %+v %v", again, err)
	}

	after := capture()
	for name, value := range before {
		if after[name] != value {
			t.Fatalf("%s differs after freezing", name)
		}
	}
	// A cold read with an empty cache downloads again.
	cold, err := coldstore.New(objects, coldstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	coldstore.SetDefault(cold)
	if history, err := GetThreadHistory(ctx, pool, storeID, threadID, nil, nil); err != nil || history.Status != 200 {
		t.Fatalf("uncached history: %v", err)
	}
	// Without a configured bucket a frozen read fails rather than returning
	// a placeholder, and the stream never starts.
	coldstore.SetDefault(nil)
	if _, err := GetThreadHistory(ctx, pool, storeID, threadID, nil, nil); !errors.Is(err, coldstore.ErrUnavailable) {
		t.Fatalf("history without store: %v", err)
	}
	recorder := httptest.NewRecorder()
	if err := server.streamThreadHistory(ctx, recorder, httptest.NewRequest(http.MethodGet, "/history", nil), storeID, threadID, nil, nil); !errors.Is(err, coldstore.ErrUnavailable) || recorder.Body.Len() != 0 {
		t.Fatalf("stream without store: %v %q", err, recorder.Body.String())
	}
	coldstore.SetDefault(store)

	// Permanent deletion orphans the objects; collection removes them only
	// after the grace period.
	result, err := ManageThread(ctx, pool, storeID, threadID, "delete", map[string]any{"operationId": uuid(), "generation": 1, "itemCount": len(history)})
	if err != nil || result.Status != 200 {
		t.Fatalf("delete: %+v %v", result, err)
	}
	for range 8 {
		if _, err := ProcessThreadErasureBatch(ctx, pool, ErasureBatchOptions{StoreID: &storeID}); err != nil {
			t.Fatal(err)
		}
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM codex_thread_events WHERE store_id=$1`, storeID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("erasure left %d rows: %v", remaining, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE mira_cold_objects SET created_at=now()-interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	if deleted, err := freezer.Collect(ctx); err != nil || deleted != 0 || objects.count() != 3 {
		t.Fatalf("collected before grace: %d %v", deleted, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE mira_cold_objects SET orphaned_at=now()-interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	if deleted, err := freezer.Collect(ctx); err != nil || deleted != 3 || objects.count() != 0 {
		t.Fatalf("collect: %d %v objects=%d", deleted, err, objects.count())
	}
	var tracked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mira_cold_objects`).Scan(&tracked); err != nil || tracked != 0 {
		t.Fatalf("tracked objects: %d %v", tracked, err)
	}
}

// Parallel freezing across many conversations uploads each distinct body
// once, freezes every eligible row and leaves bodies byte-identical.
func TestColdHistoryParallelFreeze(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	uuid := func() string {
		id, err := randomUUID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	storeID := "cold-parallel-" + uuid()
	shared := randomText(t, 24<<10)
	threads := map[string]any{}
	histories := map[string]any{}
	for range 12 {
		id := uuid()
		threads[id] = map[string]any{"thread_id": id}
		items := []any{map[string]any{"type": "session_meta", "payload": map[string]any{"id": id}}}
		for range 6 {
			items = append(items, map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "output": randomText(t, 24<<10)}})
		}
		items = append(items, map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "output": shared}})
		histories[id] = items
	}
	created, err := PutSnapshot(ctx, pool, storeID, map[string]any{"expectedVersion": 0, "snapshot": map[string]any{
		"created_threads": threads, "histories": histories,
	}}, http.Header{"X-Codex-Operation-Id": []string{uuid()}})
	if err != nil || created.Status != 200 {
		t.Fatalf("create: %+v %v", created, err)
	}
	for _, sql := range []string{
		`UPDATE codex_thread_events SET created_at=now()-interval '40 days' WHERE store_id=$1`,
		`UPDATE codex_thread_projections SET updated_at=now()-interval '40 days' WHERE store_id=$1`,
	} {
		if _, err := pool.Exec(ctx, sql, storeID); err != nil {
			t.Fatal(err)
		}
	}
	views := serverviews.New(pool)
	before := map[string]string{}
	for id := range threads {
		if complete, err := views.IndexCodexThread(ctx, storeID, id, time.Time{}); err != nil || !complete {
			t.Fatalf("index: %v %v", complete, err)
		}
		history, err := GetThreadHistory(ctx, pool, storeID, id, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(history.Body)
		before[id] = string(raw)
	}
	objects := &coldTestObjects{objects: map[string][]byte{}}
	store, err := coldstore.New(objects, coldstore.Options{CacheDir: t.TempDir(), CacheBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	coldstore.SetDefault(store)
	defer coldstore.SetDefault(nil)
	stats, err := coldstore.NewFreezer(pool, objects, coldstore.FreezerOptions{FreezeAfter: 24 * time.Hour, MinBytes: 4 << 10, UploadConcurrency: 16}).FreezePass(ctx, 32)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Threads != 12 || stats.Rows != 12*7 || objects.count() != 12*6+1 {
		t.Fatalf("stats %+v objects %d", stats, objects.count())
	}
	var scanned int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mira_cold_thread_scans WHERE store_id=$1`, storeID).Scan(&scanned); err != nil || scanned != 12 {
		t.Fatalf("scan marks: %d %v", scanned, err)
	}
	for id, want := range before {
		history, err := GetThreadHistory(ctx, pool, storeID, id, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(history.Body)
		if string(raw) != want {
			t.Fatalf("history %s differs after parallel freezing", id)
		}
	}
}

type flakyObjects struct {
	coldTestObjects
	failures atomic.Int32
}

func (store *flakyObjects) Put(ctx context.Context, key string, body []byte) error {
	if store.failures.Add(-1) >= 0 {
		return errors.New("connection reset by peer")
	}
	return store.coldTestObjects.Put(ctx, key, body)
}

// A failed upload defers only its row: the pass continues, the conversation
// stays unscanned, and the next pass freezes the deferred rows.
func TestColdHistoryDefersFailedUploads(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	uuid := func() string {
		id, err := randomUUID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	storeID := "cold-defer-" + uuid()
	threads, histories := map[string]any{}, map[string]any{}
	for range 3 {
		id := uuid()
		threads[id] = map[string]any{"thread_id": id}
		items := []any{map[string]any{"type": "session_meta", "payload": map[string]any{"id": id}}}
		for range 3 {
			items = append(items, map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "output": randomText(t, 24<<10)}})
		}
		histories[id] = items
	}
	created, err := PutSnapshot(ctx, pool, storeID, map[string]any{"expectedVersion": 0, "snapshot": map[string]any{
		"created_threads": threads, "histories": histories,
	}}, http.Header{"X-Codex-Operation-Id": []string{uuid()}})
	if err != nil || created.Status != 200 {
		t.Fatalf("create: %+v %v", created, err)
	}
	for _, sql := range []string{
		`UPDATE codex_thread_events SET created_at=now()-interval '40 days' WHERE store_id=$1`,
		`UPDATE codex_thread_projections SET updated_at=now()-interval '40 days' WHERE store_id=$1`,
	} {
		if _, err := pool.Exec(ctx, sql, storeID); err != nil {
			t.Fatal(err)
		}
	}
	views := serverviews.New(pool)
	for id := range threads {
		if complete, err := views.IndexCodexThread(ctx, storeID, id, time.Time{}); err != nil || !complete {
			t.Fatalf("index: %v %v", complete, err)
		}
	}
	objects := &flakyObjects{coldTestObjects: coldTestObjects{objects: map[string][]byte{}}}
	objects.failures.Store(2)
	freezer := coldstore.NewFreezer(pool, objects, coldstore.FreezerOptions{FreezeAfter: 24 * time.Hour, MinBytes: 4 << 10, UploadConcurrency: 1})
	first, err := freezer.FreezePass(ctx, 10)
	if err != nil {
		t.Fatalf("a failed upload aborted the pass: %v", err)
	}
	if first.Skipped != 2 || first.Rows != 7 {
		t.Fatalf("first pass %+v", first)
	}
	second, err := freezer.FreezePass(ctx, 10)
	if err != nil || second.Rows != 2 || second.Skipped != 0 {
		t.Fatalf("second pass %+v %v", second, err)
	}
	var hot int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM codex_thread_events WHERE store_id=$1 AND cold_sha256 IS NULL AND pg_column_size(payload)>=4096`, storeID).Scan(&hot); err != nil || hot != 0 {
		t.Fatalf("rows left hot: %d %v", hot, err)
	}
	if third, err := freezer.FreezePass(ctx, 10); err != nil || third.Threads != 0 {
		t.Fatalf("finished conversations rescanned: %+v %v", third, err)
	}
}

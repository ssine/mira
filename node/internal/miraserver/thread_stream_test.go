package miraserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func streamFixture(t *testing.T) (*pgxpool.Pool, string, string, int64) {
	t.Helper()
	pool := accountTestDatabase(t)
	thread, _ := randomUUID()
	store := "stream-test"
	result, err := PutSnapshot(context.Background(), pool, store, map[string]any{"expectedVersion": 0, "snapshot": map[string]any{
		"created_threads": map[string]any{thread: map[string]any{"thread_id": thread, "future": strings.Repeat("x", 128<<10)}},
		"metadata":        map[string]any{thread: map[string]any{"title": "before"}},
		"future_scalar":   []any{"preserve", true, nil},
		"histories": map[string]any{thread: []any{
			map[string]any{"type": "session_meta", "payload": map[string]any{"id": thread}},
			map[string]any{"type": "future", "payload": strings.Repeat("y", 128<<10)},
		}},
	}}, http.Header{})
	if err != nil || result.Status != 200 {
		t.Fatalf("create fixture: %v %v", result, err)
	}
	version, _ := integer(result.Body["version"])
	return pool, store, thread, version
}

func decodeStream(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

type streamHookWriter struct {
	*httptest.ResponseRecorder
	onHeader func()
	onWrite  func()
	fail     bool
}

func (w *streamHookWriter) WriteHeader(status int) {
	w.ResponseRecorder.WriteHeader(status)
	if w.onHeader != nil {
		w.onHeader()
	}
}

func (w *streamHookWriter) Write(p []byte) (int, error) {
	if w.onWrite != nil {
		hook := w.onWrite
		w.onWrite = nil
		hook()
	}
	if w.fail {
		return 0, errors.New("client disconnected")
	}
	return w.ResponseRecorder.Write(p)
}

func TestStreamStoreHeadCompatibilityAndSnapshot(t *testing.T) {
	pool, store, thread, version := streamFixture(t)
	ctx := context.Background()
	// Exercise future root objects and scalar roots overlapped by child rows.
	for _, field := range []string{"future_object", "future_overwritten"} {
		root := `{"retained":true,"overlap":"old"}`
		if field == "future_overwritten" {
			root = `42`
		}
		if _, err := pool.Exec(ctx, `INSERT INTO codex_store_state_entries(store_id,field,is_root,value) VALUES($1,$2,true,$3::json)`, store, field, root); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO codex_store_state_entries(store_id,field,is_root,entry_key,thread_id,value) VALUES($1,$2,false,'overlap',$3,'"new"')`, store, field, thread); err != nil {
			t.Fatal(err)
		}
	}
	for _, ids := range [][]string{nil, {thread}, {"missing"}, {}} {
		want, err := GetStoreHead(ctx, pool, store, ids)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		if err := streamStoreHead(ctx, pool, response, store, ids); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(want)
		var normalized map[string]any
		_ = json.Unmarshal(encoded, &normalized)
		if !reflect.DeepEqual(decodeStream(t, response), normalized) {
			t.Fatalf("head differs for scope %v", ids)
		}
		if response.Header().Get("Content-Length") != "" || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("stream headers")
		}
	}
	response := &streamHookWriter{ResponseRecorder: httptest.NewRecorder(), onHeader: func() {
		// Commit a concurrent change after the stream's snapshot is selected.
		if _, err := pool.Exec(ctx, `WITH changed AS (UPDATE codex_store_heads SET version=version+1 WHERE store_id=$1)
		 UPDATE codex_store_state_entries SET value='"after"' WHERE store_id=$1 AND field='metadata' AND NOT is_root`, store); err != nil {
			t.Fatal(err)
		}
	}}
	if err := streamStoreHead(ctx, pool, response, store, nil); err != nil {
		t.Fatal(err)
	}
	got := decodeStream(t, response.ResponseRecorder)
	if got["version"] != float64(version) || object(object(object(got["state"])["metadata"])[thread])["title"] != "before" {
		t.Fatal("mixed store versions")
	}
}

func TestStreamHistoryRawAndGeneration(t *testing.T) {
	pool, store, thread, version := streamFixture(t)
	ctx := context.Background()
	server := &Server{pool: pool}
	// JSON columns permit escaped NUL, and numbers must retain all their digits.
	const raw = `{"type":"future","payload":{"nul":"\u0000","integer":9007199254740993,"new":{"field":true}}}`
	if _, err := pool.Exec(ctx, `UPDATE codex_thread_events SET payload=$3::json WHERE store_id=$1 AND thread_id=$2 AND item_seq=2`, store, thread, raw); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/", nil)
	read := func(generation, through *int64) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		if err := server.streamThreadHistory(ctx, response, request, store, thread, generation, through); err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := read(nil, nil)
	if response.Code != 200 || !bytes.Contains(response.Body.Bytes(), []byte(raw)) {
		t.Fatal("raw history was rewritten")
	}
	gen := int64(1)
	result, err := CommitDelta(ctx, pool, store, map[string]any{"expectedVersion": version, "stateChanges": []any{}, "historyChanges": []any{
		map[string]any{"threadId": thread, "mode": "replace", "expectedGeneration": 1, "expectedItemCount": 2, "items": []any{map[string]any{"type": "future", "payload": "replacement"}}},
	}}, http.Header{})
	if err != nil || result.Status != 200 {
		t.Fatalf("replace: %v %v", result, err)
	}
	if read(&gen, &version).Code != 200 || !bytes.Contains(read(&gen, nil).Body.Bytes(), []byte(raw)) {
		t.Fatal("old generation unavailable")
	}
	current := decodeStream(t, read(nil, nil))
	if current["generation"] != float64(2) || current["itemCount"] != float64(1) {
		t.Fatal("generation mixed")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM codex_thread_events WHERE store_id=$1 AND thread_id=$2 AND generation=2`, store, thread); err != nil {
		t.Fatal(err)
	}
	if read(nil, nil).Code != 409 {
		t.Fatal("incomplete history must fail before HTTP 200")
	}
	if _, err := pool.Exec(ctx, `UPDATE codex_store_heads SET history_floor=$2 WHERE store_id=$1`, store, version+1); err != nil {
		t.Fatal(err)
	}
	if read(&gen, &version).Code != 410 {
		t.Fatal("retired version should return 410")
	}
}

func TestStreamCancellationReleasesSnapshot(t *testing.T) {
	pool, store, thread, _ := streamFixture(t)
	for _, history := range []bool{false, true} {
		for _, failWrite := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			response := &streamHookWriter{ResponseRecorder: httptest.NewRecorder(), onWrite: cancel, fail: failWrite}
			func() {
				defer func() {
					if got := recover(); got != http.ErrAbortHandler {
						t.Fatalf("expected aborted transfer, got %v", got)
					}
				}()
				var err error
				if history {
					err = (&Server{pool: pool}).streamThreadHistory(ctx, response, httptest.NewRequest("GET", "/", nil), store, thread, nil, nil)
				} else {
					err = streamStoreHead(ctx, pool, response, store, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
			}()
			cancel()
			if json.Valid(response.Body.Bytes()) {
				t.Fatal("cancelled stream presented a valid document")
			}
			// pgx disposes a cancelled connection asynchronously.
			deadline := time.Now().Add(2 * time.Second)
			for pool.Stat().AcquiredConns() != 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if pool.Stat().AcquiredConns() != 0 {
				t.Fatal("read snapshot retained a pool connection")
			}
		}
	}
}

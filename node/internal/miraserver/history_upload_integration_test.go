package miraserver

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestChunkedHistoryAtomicCommit(t *testing.T) {
	url := os.Getenv("MIRA_HISTORY_UPLOAD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MIRA_HISTORY_UPLOAD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	id, _ := randomUUID()
	store := "chunked-" + id
	thread, _ := randomUUID()
	stage := func(records []string) string {
		t.Helper()
		upload, _ := randomUUID()
		raw := []byte(strings.Join(records, "\n") + "\n")
		tx, e := pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		_, e = tx.Exec(ctx, `INSERT INTO mira_history_uploads(store_id,upload_id,thread_id,item_count,total_bytes,received_bytes) VALUES($1,$2,$3,$4,$5,$5)`, store, upload, thread, len(records), len(raw))
		if e != nil {
			t.Fatal(e)
		}
		for offset := 0; offset < len(raw); offset += historyChunkBytes {
			end := offset + historyChunkBytes
			if end > len(raw) {
				end = len(raw)
			}
			if _, e = tx.Exec(ctx, `INSERT INTO mira_history_upload_chunks VALUES($1,$2,$3,$4)`, store, upload, offset, raw[offset:end]); e != nil {
				t.Fatal(e)
			}
		}
		if e = sealHistoryUpload(ctx, tx, store, upload, int64(len(records)), int64(len(raw))); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, `UPDATE mira_history_uploads SET status='sealed' WHERE store_id=$1 AND upload_id=$2`, store, upload); e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		return upload
	}
	commit := func(upload, mode string, generation, count, version int64, op string, states []any) operationResponse {
		t.Helper()
		result, e := CommitDelta(ctx, pool, store, map[string]any{"expectedVersion": version, "stateChanges": states, "historyChanges": []any{map[string]any{"threadId": thread, "mode": mode, "expectedGeneration": generation, "expectedItemCount": count, "itemsUploadId": upload}}}, http.Header{"X-Codex-Operation-Id": []string{op}})
		if e != nil {
			t.Fatal(e)
		}
		return result
	}
	// >64 MiB in aggregate, and a single record crossing multiple chunk boundaries.
	large := `{"type":"response_item","payload":{"content":"` + strings.Repeat("x", 5*1024*1024) + `","nul":"\u0000","future":9007199254740993}}`
	records := []string{`{"type":"session_meta","payload":{"id":"` + thread + `"}}`}
	for i := 0; i < 14; i++ {
		records = append(records, large)
	}
	upload := stage(records)
	var visible int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM codex_thread_events WHERE store_id=$1`, store).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("staging visible: %d %v", visible, err)
	}
	op, _ := randomUUID()
	states := []any{map[string]any{"path": []any{"created_threads", thread}, "mode": "set", "conflictPolicy": "compareAndSwap", "expected": map[string]any{"exists": false}, "value": map[string]any{"thread_id": thread}}}
	result := commit(upload, "append", 0, 0, 0, op, states)
	if result.Status != 200 {
		t.Fatalf("commit: %+v", result)
	}
	duplicate := commit(upload, "append", 0, 0, 0, op, states)
	if duplicate.Status != 200 || duplicate.Body["duplicate"] != true {
		t.Fatalf("duplicate: %+v", duplicate)
	}
	var saved string
	if err = pool.QueryRow(ctx, `SELECT payload::text FROM codex_thread_events WHERE store_id=$1 AND thread_id=$2 AND item_seq=2`, store, thread).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(saved), []byte(large)) {
		t.Fatal("raw record changed")
	}
	// Rebase deduplicates the common prefix, preserving the existing append rules.
	more := `{"type":"future","payload":"new"}`
	suffix := stage([]string{large, more})
	op, _ = randomUUID()
	result = commit(suffix, "append", 1, 14, 1, op, []any{})
	if result.Status != 200 {
		t.Fatal(result)
	}
	head, e := currentHead(ctx, pool, store, []string{thread})
	if e != nil {
		t.Fatal(e)
	}
	if head.HistoryManifest[thread].ItemCount != 16 {
		t.Fatalf("rebase count %+v", head)
	}
	// Failed generation checks publish neither metadata nor partial history.
	conflict := stage([]string{more})
	op, _ = randomUUID()
	result = commit(conflict, "append", 9, 0, head.Version, op, []any{})
	if result.Status != 409 {
		t.Fatalf("expected conflict: %+v", result)
	}
	same := stage(append(records, more))
	op, _ = randomUUID()
	result = commit(same, "replace", 1, 16, head.Version, op, []any{})
	if result.Status != 200 || result.Body["noChange"] != true {
		t.Fatalf("same replace %+v", result)
	}
	different := stage([]string{more})
	op, _ = randomUUID()
	result = commit(different, "replace", 1, 16, head.Version, op, []any{})
	if result.Status != 200 {
		t.Fatal(result)
	}
	head, e = currentHead(ctx, pool, store, []string{thread})
	if e != nil || head.HistoryManifest[thread] != (historyEntry{Generation: 2, ItemCount: 1}) {
		t.Fatalf("replace %+v %v", head, e)
	}
	// Malformed/short streams roll back the parsed prefix as well.
	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	broken, _ := randomUUID()
	data := []byte("{}\n{")
	_, e = tx.Exec(ctx, `INSERT INTO mira_history_uploads(store_id,upload_id,thread_id,item_count,total_bytes,received_bytes) VALUES($1,$2,$3,2,$4,$4)`, store, broken, thread, len(data))
	if e != nil {
		t.Fatal(e)
	}
	_, e = tx.Exec(ctx, `INSERT INTO mira_history_upload_chunks VALUES($1,$2,0,$3)`, store, broken, data)
	if e != nil {
		t.Fatal(e)
	}
	if e = sealHistoryUpload(ctx, tx, store, broken, 2, int64(len(data))); e == nil {
		t.Fatal("invalid stream accepted")
	}
}

func TestHistoryUploadHTTPRetriesAndCancellation(t *testing.T) {
	url := os.Getenv("MIRA_HISTORY_UPLOAD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MIRA_HISTORY_UPLOAD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	username := "upload-test"
	password := "upload-integration-test-password"
	if _, err = foundation.SetAdminPassword(ctx, pool, username, password); err != nil {
		t.Fatal(err)
	}
	auth := foundation.NewAuthService(pool, foundation.AuthOptions{})
	login, err := auth.Login(ctx, httptest.NewRequest("POST", "http://localhost/v1/admin/login", nil), username, password)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{pool: pool, auth: auth}
	id, _ := randomUUID()
	store := "http-" + id
	thread, _ := randomUUID()
	base := "/v2/stores/" + store + "/history-uploads/" + id
	call := func(method, path string, body []byte, csrf bool) int {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Cookie", strings.Split(login.Cookie, ";")[0])
		if csrf {
			req.Header.Set("X-Mira-Csrf", login.CSRFToken)
		}
		w := httptest.NewRecorder()
		handled, e := server.routeHistoryUpload(ctx, w, req)
		if !handled {
			t.Fatal("not routed")
		}
		if e != nil {
			if typed, ok := e.(*HTTPError); ok {
				return typed.Status
			}
			t.Fatal(e)
		}
		return w.Code
	}
	data := []byte(`{"value":"中文\u0000"}` + "\n")
	descriptor, _ := json.Marshal(map[string]any{"threadId": thread, "itemCount": 1, "totalBytes": len(data)})
	if status := call("POST", base, descriptor, false); status != 403 {
		t.Fatalf("CSRF %d", status)
	}
	for i := 0; i < 2; i++ {
		if status := call("POST", base, descriptor, true); status != 200 {
			t.Fatalf("begin %d", status)
		}
	}
	if status := call("POST", base, []byte(`{"threadId":"other","itemCount":1,"totalBytes":10}`), true); status != 409 {
		t.Fatalf("descriptor retry %d", status)
	}
	if status := call("PUT", base+"?offset=1", data, true); status != 409 {
		t.Fatalf("gap %d", status)
	}
	if status := call("PUT", base+"?offset=0", bytes.Repeat([]byte("x"), historyChunkBytes+1), true); status != 413 {
		t.Fatalf("chunk bound %d", status)
	}
	split := bytes.Index(data, []byte("中")) + 1
	for i := 0; i < 2; i++ {
		if status := call("PUT", base+"?offset=0", data[:split], true); status != 200 {
			t.Fatalf("retry %d", status)
		}
	}
	if status := call("PUT", base+"?offset=0", []byte("wrong"), true); status != 409 {
		t.Fatalf("changed chunk %d", status)
	}
	if status := call("POST", base+"/seal", nil, true); status != 409 {
		t.Fatalf("premature seal %d", status)
	}
	if status := call("PUT", base+"?offset="+strconv.Itoa(split), data[split:], true); status != 200 {
		t.Fatalf("suffix %d", status)
	}
	for i := 0; i < 2; i++ {
		if status := call("POST", base+"/seal", nil, true); status != 200 {
			t.Fatalf("seal %d", status)
		}
	}
	if status := call("GET", strings.Replace(base, store, "different-store", 1), nil, true); status != 404 {
		t.Fatalf("store isolation %d", status)
	}
	if status := call("DELETE", base, nil, true); status != 200 {
		t.Fatalf("cancel %d", status)
	}
	if status := call("POST", base+"/seal", nil, true); status != 410 {
		t.Fatalf("cancelled seal %d", status)
	}
	op, _ := randomUUID()
	_, err = CommitDelta(ctx, pool, store, map[string]any{"expectedVersion": 0, "stateChanges": []any{}, "historyChanges": []any{map[string]any{"threadId": thread, "mode": "append", "expectedGeneration": 0, "expectedItemCount": 0, "itemsUploadId": id}}}, http.Header{"X-Codex-Operation-Id": []string{op}})
	if err == nil {
		t.Fatal("cancelled upload committed")
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM codex_thread_events WHERE store_id=$1`, store).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cancel leaked history %d %v", count, err)
	}
}

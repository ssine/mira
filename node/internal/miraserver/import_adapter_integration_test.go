package miraserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	serverimports "github.com/ssine/mira/node/internal/miraserver/imports"
)

func TestImportAdapterCommitsRawHistoryAtomically(t *testing.T) {
	databaseURL := os.Getenv("MIRA_IMPORT_ADAPTER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("MIRA_IMPORT_ADAPTER_TEST_DATABASE_URL is not set")
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
	nodeID, _ := randomUUID()
	threadID, _ := randomUUID()
	if _, err := pool.Exec(ctx, `INSERT INTO codex_nodes
		(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status)
		VALUES($1::uuid,$1::text,'import-adapter-test','linux','amd64','linux','test','{}','[]','approved')`, nodeID); err != nil {
		t.Fatal(err)
	}
	storeID := "adapter-" + strings.ReplaceAll(threadID, "-", "")
	first := json.RawMessage(`{"type":"session_meta","payload":{"id":"` + threadID + `","history_mode":"paginated","base_instructions":"hello"}}`)
	second := json.RawMessage(`{"type":"event_msg","payload":{"message":"\ud800","future":9007199254740991}}`)
	third := json.RawMessage(`{"type":"event_msg","payload":{"message":"third"}}`)
	insertImport := func(items []json.RawMessage) string {
		t.Helper()
		importID, _ := randomUUID()
		if _, err := pool.Exec(ctx, `INSERT INTO mira_codex_session_imports
			(import_id,store_id,thread_id,source_node_id,source_path,source_sha256,source_size_bytes,source_item_count,status)
			VALUES($1::uuid,$2,$3,$4,$5,$1::text,1,$6,'staged')`, importID, storeID, threadID, nodeID, "/test/"+importID, len(items)); err != nil {
			t.Fatal(err)
		}
		for index, item := range items {
			if _, err := pool.Exec(ctx, `INSERT INTO mira_codex_session_import_records(import_id,line_seq,raw_record,raw_sha256)
				VALUES($1,$2,$3::json,$4)`, importID, index+1, []byte(item), "hash"); err != nil {
				t.Fatal(err)
			}
		}
		return importID
	}
	normalize := func(raw json.RawMessage) (json.RawMessage, error) {
		return serverimports.CanonicalRolloutItem(raw, false, threadID)
	}
	commit := func(importID string, count int64) (serverimports.CommitResult, error) {
		return (&importThreadStore{pool: pool}).CommitImported(ctx, storeID, serverimports.ImportCommit{
			ThreadID: threadID, ImportID: importID, Count: count,
			Created:  map[string]any{"thread_id": threadID, "history_mode": "legacy"},
			Metadata: map[string]any{"title": "adapter test"}, Normalize: normalize, CodexVersion: "test",
			RuntimeNodeID: &nodeID,
		}, serverimports.Context{})
	}
	firstImport := insertImport([]json.RawMessage{first, second})
	result, err := commit(firstImport, 2)
	if err != nil || result.Version != 1 || result.NoChange {
		t.Fatalf("initial import result=%+v err=%v", result, err)
	}
	var raw string
	if err := pool.QueryRow(ctx, `SELECT payload::text FROM codex_thread_events
		WHERE store_id=$1 AND thread_id=$2 AND item_seq=2`, storeID, threadID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(raw), `\ud800`) || !strings.Contains(raw, "9007199254740991") {
		t.Fatalf("lossless JSON was changed: %s", raw)
	}
	var boundNode string
	if err := pool.QueryRow(ctx, `SELECT node_id::text FROM mira_codex_thread_runtimes WHERE store_id=$1 AND thread_id=$2`, storeID, threadID).Scan(&boundNode); err != nil || boundNode != nodeID {
		t.Fatalf("runtime binding was not committed with history: node=%q err=%v", boundNode, err)
	}
	result, err = commit(firstImport, 2)
	if err != nil || result.Version != 1 || !result.NoChange {
		t.Fatalf("duplicate import result=%+v err=%v", result, err)
	}
	secondImport := insertImport([]json.RawMessage{first, second, third})
	result, err = commit(secondImport, 3)
	if err != nil || result.Version != 2 || result.NoChange {
		t.Fatalf("append import result=%+v err=%v", result, err)
	}
	head, err := currentHead(ctx, pool, storeID, []string{threadID})
	if err != nil || head.HistoryManifest[threadID] != (historyEntry{Generation: 1, ItemCount: 3}) {
		t.Fatalf("append manifest=%+v err=%v", head.HistoryManifest, err)
	}
	divergent := insertImport([]json.RawMessage{json.RawMessage(`{"type":"event_msg","payload":{"message":"different"}}`)})
	_, err = commit(divergent, 1)
	var failure *HTTPError
	if !errors.As(err, &failure) || failure.Code != "history_diverged" {
		t.Fatalf("divergent import error=%v", err)
	}
	head, err = currentHead(ctx, pool, storeID, []string{threadID})
	if err != nil || head.Version != 2 || head.HistoryManifest[threadID].ItemCount != 3 {
		t.Fatalf("divergence changed canonical history: head=%+v err=%v", head, err)
	}
}

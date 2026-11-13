package miraserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	serverviews "github.com/ssine/mira/node/internal/miraserver/views"
)

// Permanent deletion must also remove the derived prose copies used by
// search and outlines, in the same transaction as the projection.
func TestDeleteThreadRemovesConversationIndex(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	uuid := func() string {
		id, err := randomUUID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	storeID, threadID := "index-delete-"+uuid(), uuid()
	created, err := PutSnapshot(ctx, pool, storeID, map[string]any{"expectedVersion": 0, "snapshot": map[string]any{
		"created_threads": map[string]any{threadID: map[string]any{"thread_id": threadID}},
		"histories": map[string]any{threadID: []any{
			map[string]any{"type": "session_meta", "payload": map[string]any{"id": threadID}},
			map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "需要被彻底删除的秘密内容"}},
		}},
	}}, http.Header{"X-Codex-Operation-Id": []string{uuid()}})
	if err != nil || created.Status != 200 {
		t.Fatalf("create: %+v %v", created, err)
	}
	views := serverviews.New(pool)
	if complete, err := views.IndexCodexThread(ctx, storeID, threadID, time.Time{}); err != nil || !complete {
		t.Fatalf("index: %v %v", complete, err)
	}
	count := func() int {
		var total int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM mira_conversation_records WHERE store_id=$1 AND thread_id=$2)+
		  (SELECT count(*) FROM mira_conversation_messages WHERE store_id=$1 AND thread_id=$2)+
		  (SELECT count(*) FROM mira_conversation_index_state WHERE store_id=$1 AND thread_id=$2)`, storeID, threadID).Scan(&total); err != nil {
			t.Fatal(err)
		}
		return total
	}
	if count() != 3 {
		t.Fatalf("indexed rows: %d", count())
	}
	result, err := ManageThread(ctx, pool, storeID, threadID, "delete", map[string]any{"operationId": uuid(), "generation": 1, "itemCount": 2})
	if err != nil || result.Status != 200 {
		t.Fatalf("delete: %+v %v", result, err)
	}
	if remaining := count(); remaining != 0 {
		t.Fatalf("deletion left %d derived rows", remaining)
	}
	// A later index pass must not resurrect them.
	if complete, err := views.IndexCodexThread(ctx, storeID, threadID, time.Time{}); err != nil || !complete || count() != 0 {
		t.Fatalf("reindex after delete: %v %v %d", complete, err, count())
	}
}

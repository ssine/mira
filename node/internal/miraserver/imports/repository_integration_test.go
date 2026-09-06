package imports

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func TestImportProvenanceNeverAliasesAcrossStores(t *testing.T) {
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
	suffix := time.Now().UnixNano()
	nodeID := "55555555-5555-4555-8555-555555555555"
	if _, err := pool.Exec(ctx, `INSERT INTO codex_nodes
		(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status)
		VALUES($1::uuid,$1::text,'repository-import-test','linux','amd64','linux','test','{}','[]','approved')
		ON CONFLICT(node_id) DO NOTHING`, nodeID); err != nil {
		t.Fatal(err)
	}
	stores := []string{fmt.Sprintf("store-a-%d", suffix), fmt.Sprintf("store-b-%d", suffix)}
	for _, storeID := range stores {
		if _, err := pool.Exec(ctx, `INSERT INTO codex_store_heads(store_id,version) VALUES($1,0)`, storeID); err != nil {
			t.Fatal(err)
		}
		defer pool.Exec(context.Background(), `DELETE FROM codex_store_heads WHERE store_id=$1`, storeID) //nolint:errcheck
	}
	repository := &PostgresRepository{Pool: pool}
	path := fmt.Sprintf("/same/source-%d.jsonl", suffix)
	finish := func(storeID string) (string, bool) {
		t.Helper()
		transfer, err := repository.BeginTransfer(ctx, TransferInput{
			StoreID: storeID, ThreadID: "66666666-6666-4666-8666-666666666666", NodeID: nodeID, Path: path,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer transfer.Rollback(ctx) //nolint:errcheck
		if err := transfer.Add(ctx, []RawRecord{{LineSequence: 1, Raw: json.RawMessage(`{"same":true}`), SHA256: "record"}}); err != nil {
			t.Fatal(err)
		}
		id, duplicate, err := transfer.Finish(ctx, TransferFinal{
			ThreadID: "66666666-6666-4666-8666-666666666666", SHA256: "same-source", SizeBytes: 13, ItemCount: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id, duplicate
	}
	firstID, firstDuplicate := finish(stores[0])
	_, _, crossStoreErr := func() (string, bool, error) {
		transfer, err := repository.BeginTransfer(ctx, TransferInput{
			StoreID: stores[1], ThreadID: "66666666-6666-4666-8666-666666666666", NodeID: nodeID, Path: path,
		})
		if err != nil {
			return "", false, err
		}
		defer transfer.Rollback(ctx) //nolint:errcheck
		if err := transfer.Add(ctx, []RawRecord{{LineSequence: 1, Raw: json.RawMessage(`{"same":true}`), SHA256: "record"}}); err != nil {
			return "", false, err
		}
		return transfer.Finish(ctx, TransferFinal{
			ThreadID: "66666666-6666-4666-8666-666666666666", SHA256: "same-source", SizeBytes: 13, ItemCount: 1,
		})
	}()
	if firstDuplicate || errorCode(crossStoreErr) != "store_scope_conflict" {
		t.Fatalf("cross-store source was aliased: first=%s/%v cross-store error=%v", firstID, firstDuplicate, crossStoreErr)
	}
	repeatedID, repeatedDuplicate := finish(stores[0])
	if !repeatedDuplicate || repeatedID != firstID {
		t.Fatalf("same-store retry was not idempotent: first=%s repeated=%s/%v", firstID, repeatedID, repeatedDuplicate)
	}
}

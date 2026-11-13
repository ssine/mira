package views

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestConversationIndexEquivalenceOnDatabase compares, for real threads, the
// outline projected from slim index records with the projection of complete
// canonical history. It never writes: the session is read-only.
//
//	MIRA_INDEX_EQUIVALENCE_DATABASE_URL=postgres://... MIRA_INDEX_EQUIVALENCE_THREADS=200 go test -run Equivalence
func TestConversationIndexEquivalenceOnDatabase(t *testing.T) {
	databaseURL := os.Getenv("MIRA_INDEX_EQUIVALENCE_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set MIRA_INDEX_EQUIVALENCE_DATABASE_URL to compare outlines with real history")
	}
	limit, _ := strconv.Atoi(os.Getenv("MIRA_INDEX_EQUIVALENCE_THREADS"))
	if limit <= 0 {
		limit = 50
	}
	maxItems, _ := strconv.Atoi(os.Getenv("MIRA_INDEX_EQUIVALENCE_MAX_ITEMS"))
	if maxItems <= 0 {
		maxItems = 20000
	}
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	service := New(pool)
	rows, err := pool.Query(ctx, `SELECT store_id,thread_id,active_generation,item_count FROM codex_thread_projections
	  WHERE item_count BETWEEN 1 AND $2 ORDER BY updated_at DESC LIMIT $1`, limit, maxItems)
	if err != nil {
		t.Fatal(err)
	}
	type thread struct {
		store, id         string
		generation, count int64
	}
	threads := []thread{}
	for rows.Next() {
		var item thread
		if err := rows.Scan(&item.store, &item.id, &item.generation, &item.count); err != nil {
			t.Fatal(err)
		}
		threads = append(threads, item)
	}
	rows.Close()
	var compared, records, rawBytes, slimBytes, entries int64
	byType := map[string]int64{}
	started := time.Now()
	for _, item := range threads {
		history, err := service.queryTranscriptRows(ctx, item.store, item.id, item.generation, item.count+1, int(item.count))
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(history)) != item.count {
			t.Logf("skip %s: incomplete history (%d of %d)", item.id, len(history), item.count)
			continue
		}
		recordedAt, err := service.restoreImportedTimestamps(ctx, item.store, item.id, history)
		if err != nil {
			t.Fatal(err)
		}
		raw := make([]map[string]any, len(history))
		for index, row := range history {
			raw[index] = row.payload
			encoded, _ := json.Marshal(row.payload)
			rawBytes += int64(len(encoded))
		}
		full := outlineSkeleton(ProjectCodexTranscript(raw, ProjectionOptions{Fragments: true, RecordedAt: recordedAt}))
		indexed, slim := projectFromIndex(raw, recordedAt)
		encoded, _ := json.Marshal(slim)
		slimBytes += int64(len(encoded))
		for _, record := range slim {
			one, _ := json.Marshal(record)
			payload := object(record["payload"])
			key := stringValue(record["type"]) + ":" + stringValue(payload["type"])
			if item := object(payload["item"]); item != nil {
				key += ":" + stringValue(item["type"])
			}
			byType[key] += int64(len(one))
		}
		right := outlineSkeleton(indexed)
		if !reflect.DeepEqual(full, right) {
			for index := 0; index < max(len(full), len(right)); index++ {
				var a, b map[string]any
				if index < len(full) {
					a = full[index]
				}
				if index < len(right) {
					b = right[index]
				}
				if !reflect.DeepEqual(a, b) {
					left, _ := json.Marshal(a)
					other, _ := json.Marshal(b)
					t.Errorf("thread %s differs at entry %d of %d/%d\nfull:  %.1500s\nindex: %.1500s", item.id, index, len(full), len(right), left, other)
					break
				}
			}
		}
		compared++
		records += item.count
		entries += int64(len(full))
	}
	for key, size := range byType {
		if size > 1<<20 {
			t.Logf("slim %-60s %6d MiB", key, size>>20)
		}
	}
	t.Logf("compared %d threads, %d records, %d entries in %s; raw %d MiB, slim %d MiB",
		compared, records, entries, time.Since(started).Round(time.Millisecond), rawBytes>>20, slimBytes>>20)
}

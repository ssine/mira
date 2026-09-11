package miraserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const eventFormatVersion = 1

func randomUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func integer(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(string(number), 10, 64)
		return parsed, err == nil
	case float64:
		parsed := int64(number)
		return parsed, float64(parsed) == number
	case int:
		return int64(number), true
	case int64:
		return number, true
	default:
		return 0, false
	}
}

func historiesFromSnapshot(snapshot map[string]any) map[string]any {
	switch histories := snapshot["histories"].(type) {
	case map[string]any:
		return histories
	case map[string][]any:
		// canonicalStore uses this stronger internal type. Preserve it when a
		// v1 compatibility snapshot is compared with the persisted prefix.
		result := make(map[string]any, len(histories))
		for threadID, items := range histories {
			result[threadID] = items
		}
		return result
	default:
		return map[string]any{}
	}
}

func stateFromSnapshot(snapshot map[string]any) map[string]any {
	state := make(map[string]any, len(snapshot))
	for key, value := range snapshot {
		if key != "histories" {
			state[key] = value
		}
	}
	return state
}

func assertThreadsNotDeleted(ctx context.Context, query dbtx, storeID string, threadIDs []string) error {
	if len(threadIDs) == 0 {
		return nil
	}
	var threadID string
	err := query.QueryRow(ctx, `SELECT thread_id FROM mira_thread_actions
      WHERE store_id=$1 AND action='delete' AND thread_id=ANY($2::text[]) LIMIT 1`, storeID, threadIDs).Scan(&threadID)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return &HTTPError{Status: http.StatusGone, Code: "thread_deleted", Message: "此会话已永久删除，不能继续写入或恢复。"}
}

func snapshotThreadIDs(snapshot map[string]any) []string {
	ids := []string{}
	for field, raw := range snapshot {
		values := object(raw)
		if field == "rollout_paths" {
			for _, value := range values {
				if id, ok := value.(string); ok {
					ids = append(ids, id)
				}
			}
			continue
		}
		for id := range values {
			ids = append(ids, id)
		}
	}
	return ids
}

func historyPrefix(previous, next []any) bool {
	if len(previous) > len(next) {
		return false
	}
	for index := range previous {
		if !jsonEqual(previous[index], next[index]) {
			return false
		}
	}
	return true
}

type historyAppend struct {
	ThreadID   string
	Generation int64
	ItemSeq    int64
	Payload    any
}

func historyArray(value any) ([]any, bool) {
	result, ok := value.([]any)
	return result, ok
}

func buildHistoryPlan(previousSnapshot, nextSnapshot map[string]any, previousManifest map[string]historyEntry) (map[string]historyEntry, []historyAppend, error) {
	previousHistories, nextHistories := historiesFromSnapshot(previousSnapshot), historiesFromSnapshot(nextSnapshot)
	manifest := map[string]historyEntry{}
	appends := []historyAppend{}
	for threadID, raw := range nextHistories {
		next, ok := historyArray(raw)
		if !ok {
			return nil, nil, fmt.Errorf("history for thread %s must be an array", threadID)
		}
		previous, hadPrevious := historyArray(previousHistories[threadID])
		entry := previousManifest[threadID]
		appendOnly := hadPrevious && historyPrefix(previous, next)
		generation, start := entry.Generation+1, 0
		if appendOnly {
			generation = entry.Generation
			if generation < 1 {
				generation = 1
			}
			start = len(previous)
		}
		manifest[threadID] = historyEntry{Generation: generation, ItemCount: int64(len(next))}
		for index := start; index < len(next); index++ {
			appends = append(appends, historyAppend{ThreadID: threadID, Generation: generation, ItemSeq: int64(index + 1), Payload: next[index]})
		}
	}
	return manifest, appends, nil
}

func projection(snapshot map[string]any, threadID string, entry historyEntry, eventSeq int64) map[string]any {
	created := object(object(snapshot["created_threads"])[threadID])
	metadata := object(object(snapshot["metadata_updates"])[threadID])
	parent, _ := created["parent_thread_id"].(string)
	source, _ := created["source"].(string)
	if source == "" {
		source, _ = metadata["source"].(string)
	}
	title, _ := metadata["title"].(string)
	cwd, _ := metadata["cwd"].(string)
	if cwd == "" {
		cwd, _ = object(created["metadata"])["cwd"].(string)
	}
	createdValue, metadataValue := any(nil), any(nil)
	if len(created) > 0 {
		createdValue = created
	}
	if len(metadata) > 0 {
		metadataValue = metadata
	}
	return map[string]any{
		"threadId": threadID, "activeGeneration": entry.Generation, "itemCount": entry.ItemCount,
		"parentThreadId": nullableString(parent), "sourceKind": nullableString(source), "title": nullableString(title), "cwd": nullableString(cwd),
		"state":           map[string]any{"createdThread": createdValue, "metadata": metadataValue, "name": object(snapshot["names"])[threadID], "section": object(snapshot["sections"])[threadID]},
		"throughEventSeq": eventSeq,
	}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func replaceProjections(ctx context.Context, tx pgx.Tx, storeID string, state map[string]any, manifest map[string]historyEntry, eventSeq int64, affected, metadataAffected map[string]bool) error {
	for threadID, entry := range manifest {
		if !affected[threadID] {
			continue
		}
		if !metadataAffected[threadID] {
			command, err := tx.Exec(ctx, `UPDATE codex_thread_projections SET active_generation=$3,item_count=$4,updated_at=NOW()
          WHERE store_id=$1 AND thread_id=$2`, storeID, threadID, entry.Generation, entry.ItemCount)
			if err != nil {
				return err
			}
			if command.RowsAffected() > 0 {
				continue
			}
		}
		value := projection(state, threadID, entry, eventSeq)
		encoded, err := json.Marshal(value["state"])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO codex_thread_projections (
          store_id, thread_id, active_generation, item_count, parent_thread_id,
          source_kind, title, cwd, state, through_event_seq
        ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10)
        ON CONFLICT(store_id,thread_id) DO UPDATE SET active_generation=EXCLUDED.active_generation,
          item_count=EXCLUDED.item_count,parent_thread_id=EXCLUDED.parent_thread_id,source_kind=EXCLUDED.source_kind,
          title=EXCLUDED.title,cwd=EXCLUDED.cwd,state=EXCLUDED.state,through_event_seq=EXCLUDED.through_event_seq,updated_at=NOW()`,
			storeID, threadID, entry.Generation, entry.ItemCount, value["parentThreadId"], value["sourceKind"], value["title"], value["cwd"], encoded, eventSeq); err != nil {
			return err
		}
	}
	return nil
}

func canonicalStore(ctx context.Context, query dbtx, storeID string) (storeHead, map[string][]any, error) {
	head, err := currentHead(ctx, query, storeID, nil)
	if err != nil {
		return storeHead{}, nil, err
	}
	histories := map[string][]any{}
	for threadID, entry := range head.HistoryManifest {
		rows, err := query.Query(ctx, `SELECT payload FROM codex_thread_events_versioned
        WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND store_event_seq<=$4 ORDER BY item_seq`, storeID, threadID, entry.Generation, head.Version)
		if err != nil {
			return storeHead{}, nil, err
		}
		items := []any{}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return storeHead{}, nil, err
			}
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				rows.Close()
				return storeHead{}, nil, err
			}
			items = append(items, value)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return storeHead{}, nil, err
		}
		if int64(len(items)) != entry.ItemCount {
			return storeHead{}, nil, fmt.Errorf("incomplete history for %s", threadID)
		}
		histories[threadID] = items
	}
	return head, histories, nil
}

func GetStoreHead(ctx context.Context, pool *pgxpool.Pool, storeID string, threadIDs []string) (map[string]any, error) {
	head, err := currentHead(ctx, pool, storeID, threadIDs)
	if err != nil {
		return nil, err
	}
	return headView(head), nil
}

func headView(head storeHead) map[string]any {
	updated := any(nil)
	if head.UpdatedAt != nil {
		updated = head.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return map[string]any{"version": head.Version, "historyFloor": head.HistoryFloor, "state": head.State, "historyManifest": head.HistoryManifest, "updatedAt": updated}
}

func GetSnapshot(ctx context.Context, pool *pgxpool.Pool, storeID string) (map[string]any, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	head, histories, err := canonicalStore(ctx, tx, storeID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	var snapshot any
	if head.Version > 0 {
		snapshot, err = cloneObject(head.State)
		if err != nil {
			return nil, err
		}
		snapshot.(map[string]any)["histories"] = histories
	}
	updated := any(nil)
	if head.UpdatedAt != nil {
		updated = head.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return map[string]any{"version": head.Version, "snapshot": snapshot, "updatedAt": updated}, nil
}

func repairNewGenerations(ctx context.Context, tx pgx.Tx, storeID string, before, after map[string]historyEntry, appends []historyAppend) error {
	for id, entry := range after {
		if _, exists := before[id]; exists {
			continue
		}
		var events, revisions int64
		if err := tx.QueryRow(ctx, "SELECT COALESCE(MAX(generation),0) FROM codex_thread_events WHERE store_id=$1 AND thread_id=$2", storeID, id).Scan(&events); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT COALESCE(MAX(generation),0) FROM codex_thread_revisions WHERE store_id=$1 AND thread_id=$2", storeID, id).Scan(&revisions); err != nil {
			return err
		}
		generation := events
		if revisions > generation {
			generation = revisions
		}
		generation++
		entry.Generation = generation
		after[id] = entry
		for index := range appends {
			if appends[index].ThreadID == id {
				appends[index].Generation = generation
			}
		}
	}
	return nil
}

func insertHistoryAppends(ctx context.Context, tx pgx.Tx, storeID, operationID string, codexVersion *string, appends []historyAppend) error {
	for _, item := range appends {
		payload, err := json.Marshal(item.Payload)
		if err != nil {
			return err
		}
		hash, err := digestJSON(item.Payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO codex_thread_events (
          store_id,thread_id,generation,item_seq,operation_id,event_format_version,codex_version,payload,payload_sha256
        ) VALUES($1,$2,$3,$4,$5,$6,$7,$8::json,$9)`, storeID, item.ThreadID, item.Generation, item.ItemSeq, operationID, eventFormatVersion, codexVersion, payload, hash); err != nil {
			return err
		}
	}
	return nil
}

func PutSnapshot(ctx context.Context, pool *pgxpool.Pool, storeID string, body map[string]any, headers http.Header) (operationResponse, error) {
	expected, ok := integer(body["expectedVersion"])
	snapshot, snapshotOK := body["snapshot"].(map[string]any)
	if !ok || expected < 0 || !snapshotOK || snapshot == nil {
		return operationResponse{Status: 400, Body: map[string]any{"error": "invalid compatibility snapshot"}}, nil
	}
	operationID := headers.Get("X-Codex-Operation-Id")
	if operationID == "" {
		var err error
		operationID, err = randomUUID()
		if err != nil {
			return operationResponse{}, err
		}
	}
	var codexVersion *string
	if value := headers.Get("X-Codex-Version"); value != "" {
		codexVersion = &value
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return operationResponse{}, err
	}
	defer tx.Rollback(ctx)
	duplicate, err := beginReceipt(ctx, tx, storeID, operationID, body, codexVersion)
	if err != nil {
		return operationResponse{}, err
	}
	if duplicate != nil {
		return *duplicate, nil
	}
	if err := lockScope(ctx, tx, storeID, nil); err != nil {
		return operationResponse{}, err
	}
	head, histories, err := canonicalStore(ctx, tx, storeID)
	if err != nil {
		return operationResponse{}, err
	}
	if expected != head.Version {
		return operationResponse{Status: 409, Body: map[string]any{"error": "snapshot version conflict", "currentVersion": head.Version}}, nil
	}
	if err := assertThreadsNotDeleted(ctx, tx, storeID, snapshotThreadIDs(snapshot)); err != nil {
		return operationResponse{}, err
	}
	previous, err := cloneObject(head.State)
	if err != nil {
		return operationResponse{}, err
	}
	previous["histories"] = histories
	manifest, appends, err := buildHistoryPlan(previous, snapshot, head.HistoryManifest)
	if err != nil {
		return operationResponse{}, err
	}
	changedHistories := []string{}
	for id, entry := range manifest {
		if head.HistoryManifest[id] != entry {
			changedHistories = append(changedHistories, id)
		}
	}
	for id := range head.HistoryManifest {
		if _, exists := manifest[id]; !exists {
			changedHistories = append(changedHistories, id)
		}
	}
	if err := checkAccountHistoryWriter(ctx, tx, storeID, changedHistories); err != nil {
		return operationResponse{}, err
	}
	if err := repairNewGenerations(ctx, tx, storeID, head.HistoryManifest, manifest, appends); err != nil {
		return operationResponse{}, err
	}
	state := stateFromSnapshot(snapshot)
	affected, err := persistState(ctx, tx, storeID, operationID, head.State, state)
	if err != nil {
		return operationResponse{}, err
	}
	boundaries, err := persistBoundaries(ctx, tx, storeID, operationID, head.HistoryManifest, manifest)
	if err != nil {
		return operationResponse{}, err
	}
	for id := range boundaries {
		affected[id] = true
	}
	if err := insertHistoryAppends(ctx, tx, storeID, operationID, codexVersion, appends); err != nil {
		return operationResponse{}, err
	}
	if err := replaceProjections(ctx, tx, storeID, state, manifest, 1, affected, affected); err != nil {
		return operationResponse{}, err
	}
	version, err := publishReceipt(ctx, tx, storeID, operationID, affected, int64(len(appends)), nil)
	if err != nil {
		return operationResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return operationResponse{}, err
	}
	return operationResponse{Status: 200, Body: map[string]any{"version": version, "operationId": operationID, "appendedItemCount": len(appends), "updatedAt": time.Now().UTC().Format(time.RFC3339Nano)}}, nil
}

func sortedUnique(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

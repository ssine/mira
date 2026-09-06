package miraserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type dbtx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type historyEntry struct {
	Generation int64 `json:"generation"`
	ItemCount  int64 `json:"itemCount"`
}

type storeHead struct {
	Version         int64
	HistoryFloor    int64
	State           map[string]any
	HistoryManifest map[string]historyEntry
	UpdatedAt       *time.Time
}

type stateEntry struct {
	Field    string          `json:"field"`
	EntryKey string          `json:"entry_key"`
	IsRoot   bool            `json:"is_root"`
	ThreadID *string         `json:"thread_id"`
	Value    json.RawMessage `json:"value"`
}

func stateFromEntries(entries []stateEntry) (map[string]any, error) {
	state := map[string]any{}
	for _, entry := range entries {
		if !entry.IsRoot {
			continue
		}
		var value any
		decoder := json.RawMessage(entry.Value)
		if err := json.Unmarshal(decoder, &value); err != nil {
			return nil, err
		}
		state[entry.Field] = value
	}
	for _, entry := range entries {
		if entry.IsRoot {
			continue
		}
		field, ok := state[entry.Field].(map[string]any)
		if !ok {
			field = map[string]any{}
			state[entry.Field] = field
		}
		var value any
		if err := json.Unmarshal(entry.Value, &value); err != nil {
			return nil, err
		}
		field[entry.EntryKey] = value
	}
	return state, nil
}

func currentHead(ctx context.Context, query dbtx, storeID string, threadIDs []string) (storeHead, error) {
	var version, floor string
	var updatedAt *time.Time
	var entriesRaw, manifestRaw []byte
	err := query.QueryRow(ctx, `SELECT h.version::text,h.history_floor::text,h.updated_at,
    COALESCE((SELECT json_agg(s) FROM codex_store_state_entries s WHERE s.store_id=h.store_id
      AND ($2::text[] IS NULL OR s.is_root OR s.thread_id=ANY($2))), '[]'::json) AS entries,
    COALESCE((SELECT json_object_agg(thread_id,json_build_object('generation',active_generation,'itemCount',item_count))
      FROM codex_thread_projections p WHERE p.store_id=h.store_id AND ($2::text[] IS NULL OR p.thread_id=ANY($2))), '{}'::json) AS manifest
    FROM codex_store_heads h WHERE h.store_id=$1`, storeID, nullableStrings(threadIDs)).Scan(
		&version, &floor, &updatedAt, &entriesRaw, &manifestRaw,
	)
	if err == pgx.ErrNoRows {
		return storeHead{State: map[string]any{}, HistoryManifest: map[string]historyEntry{}}, nil
	}
	if err != nil {
		return storeHead{}, err
	}
	var entries []stateEntry
	if err := json.Unmarshal(entriesRaw, &entries); err != nil {
		return storeHead{}, fmt.Errorf("decode store entries: %w", err)
	}
	state, err := stateFromEntries(entries)
	if err != nil {
		return storeHead{}, fmt.Errorf("assemble store state: %w", err)
	}
	manifest := map[string]historyEntry{}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return storeHead{}, fmt.Errorf("decode history manifest: %w", err)
	}
	parsedVersion, err := strconv.ParseInt(version, 10, 64)
	if err != nil {
		return storeHead{}, err
	}
	parsedFloor, err := strconv.ParseInt(floor, 10, 64)
	if err != nil {
		return storeHead{}, err
	}
	return storeHead{Version: parsedVersion, HistoryFloor: parsedFloor, State: state, HistoryManifest: manifest, UpdatedAt: updatedAt}, nil
}

func nullableStrings(values []string) any {
	if values == nil {
		return nil
	}
	return values
}

func historicalManifest(ctx context.Context, query dbtx, storeID string, version int64, threadIDs []string) (map[string]historyEntry, error) {
	rows, err := query.Query(ctx, `SELECT DISTINCT ON(r.thread_id) r.thread_id,r.generation::text,r.item_count::text,r.active
    FROM codex_thread_revisions r JOIN codex_store_events c USING(store_id,operation_id)
    WHERE r.store_id=$1 AND c.event_seq<=$2 AND ($3::text[] IS NULL OR r.thread_id=ANY($3))
      AND NOT EXISTS(SELECT 1 FROM mira_thread_actions a WHERE a.store_id=r.store_id AND a.thread_id=r.thread_id AND a.action='delete')
    ORDER BY r.thread_id,c.event_seq DESC`, storeID, version, nullableStrings(threadIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]historyEntry{}
	for rows.Next() {
		var threadID, generation, count string
		var active bool
		if err := rows.Scan(&threadID, &generation, &count, &active); err != nil {
			return nil, err
		}
		if !active {
			continue
		}
		parsedGeneration, err := strconv.ParseInt(generation, 10, 64)
		if err != nil {
			return nil, err
		}
		parsedCount, err := strconv.ParseInt(count, 10, 64)
		if err != nil {
			return nil, err
		}
		result[threadID] = historyEntry{Generation: parsedGeneration, ItemCount: parsedCount}
	}
	return result, rows.Err()
}

func lockScope(ctx context.Context, tx pgx.Tx, storeID string, threadIDs []string) error {
	gate, _ := json.Marshal([]string{"mira-store", storeID})
	lock := "SELECT pg_advisory_xact_lock(hashtextextended($1,0))"
	if threadIDs != nil {
		lock = "SELECT pg_advisory_xact_lock_shared(hashtextextended($1,0))"
	}
	if _, err := tx.Exec(ctx, lock, string(gate)); err != nil {
		return err
	}
	if threadIDs != nil {
		ids := append([]string(nil), threadIDs...)
		sort.Strings(ids)
		last := ""
		for _, id := range ids {
			if id == last {
				continue
			}
			last = id
			key, _ := json.Marshal([]string{"mira-thread", storeID, id})
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", string(key)); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO codex_store_heads(store_id,version) VALUES($1,0) ON CONFLICT DO NOTHING`, storeID)
	return err
}

type operationResponse struct {
	Status int
	Body   map[string]any
}

func beginReceipt(ctx context.Context, tx pgx.Tx, storeID, operationID string, body any, codexVersion *string) (*operationResponse, error) {
	digest, err := digestJSON(body)
	if err != nil {
		return nil, err
	}
	command, err := tx.Exec(ctx, `INSERT INTO codex_store_events(store_id,operation_id,request_sha256,codex_version)
    VALUES($1,$2,$3,$4) ON CONFLICT(store_id,operation_id) DO NOTHING`, storeID, operationID, digest, codexVersion)
	if err != nil {
		return nil, err
	}
	if command.RowsAffected() > 0 {
		return nil, nil
	}
	var previousDigest string
	var resultVersion, appended string
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `SELECT request_sha256,result_version::text,appended_item_count::text,created_at
      FROM codex_store_events WHERE store_id=$1 AND operation_id=$2`, storeID, operationID).Scan(
		&previousDigest, &resultVersion, &appended, &createdAt,
	); err != nil {
		return nil, err
	}
	if previousDigest != digest {
		return &operationResponse{Status: 409, Body: map[string]any{
			"error": "operation UUID was already used for a different request", "code": "operation_conflict",
		}}, nil
	}
	version, err := strconv.ParseInt(resultVersion, 10, 64)
	if err != nil {
		return nil, err
	}
	count, err := strconv.ParseInt(appended, 10, 64)
	if err != nil {
		return nil, err
	}
	manifest, err := historicalManifest(ctx, tx, storeID, version, nil)
	if err != nil {
		return nil, err
	}
	return &operationResponse{Status: 200, Body: map[string]any{
		"version": version, "operationId": operationID, "duplicate": true, "rebased": true,
		"historyManifest": manifest, "appendedItemCount": count, "updatedAt": createdAt.UTC().Format(time.RFC3339Nano),
	}}, nil
}

type materializedEntry struct {
	Field    string
	EntryKey string
	IsRoot   bool
	ThreadID *string
	Value    any
}

func stateEntries(state map[string]any) map[string]materializedEntry {
	result := map[string]materializedEntry{}
	for field, value := range state {
		rootValue := value
		if _, ok := value.(map[string]any); ok {
			rootValue = map[string]any{}
		}
		root := materializedEntry{Field: field, IsRoot: true, Value: rootValue}
		result[entryKey(root)] = root
		entries, ok := value.(map[string]any)
		if !ok {
			continue
		}
		for key, entryValue := range entries {
			var threadID *string
			if field == "rollout_paths" {
				if id, ok := entryValue.(string); ok {
					threadID = &id
				}
			} else {
				id := key
				threadID = &id
			}
			entry := materializedEntry{Field: field, EntryKey: key, ThreadID: threadID, Value: entryValue}
			result[entryKey(entry)] = entry
		}
	}
	return result
}

func entryKey(entry materializedEntry) string {
	payload, _ := json.Marshal([]any{entry.Field, entry.IsRoot, entry.EntryKey})
	return string(payload)
}

type stateChange struct {
	Path  []string
	Mode  string
	Value any
}

func changesBetween(before any, beforeExists bool, after any, afterExists bool, path []string, output *[]stateChange) {
	if beforeExists == afterExists && (!beforeExists || jsonEqual(before, after)) {
		return
	}
	beforeObject, beforeOK := before.(map[string]any)
	afterObject, afterOK := after.(map[string]any)
	if beforeExists && afterExists && beforeOK && afterOK {
		keys := map[string]bool{}
		for key := range beforeObject {
			keys[key] = true
		}
		for key := range afterObject {
			keys[key] = true
		}
		names := make([]string, 0, len(keys))
		for key := range keys {
			names = append(names, key)
		}
		sort.Strings(names)
		for _, key := range names {
			left, leftOK := beforeObject[key]
			right, rightOK := afterObject[key]
			changesBetween(left, leftOK, right, rightOK, append(append([]string{}, path...), key), output)
		}
		return
	}
	if afterExists && afterOK {
		*output = append(*output, stateChange{Path: append([]string{}, path...), Mode: "set", Value: map[string]any{}})
		keys := make([]string, 0, len(afterObject))
		for key := range afterObject {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			changesBetween(nil, false, afterObject[key], true, append(append([]string{}, path...), key), output)
		}
		return
	}
	change := stateChange{Path: append([]string{}, path...), Mode: "remove"}
	if afterExists {
		change.Mode, change.Value = "set", after
	}
	*output = append(*output, change)
}

func stateDelta(before, after map[string]any) []stateChange {
	changes := []stateChange{}
	changesBetween(before, true, after, true, nil, &changes)
	return changes
}

func rebuildStateEntries(ctx context.Context, tx pgx.Tx, storeID string, state map[string]any) error {
	if _, err := tx.Exec(ctx, "DELETE FROM codex_store_state_entries WHERE store_id=$1", storeID); err != nil {
		return err
	}
	for _, entry := range stateEntries(state) {
		payload, err := json.Marshal(entry.Value)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO codex_store_state_entries(store_id,field,is_root,entry_key,thread_id,value)
      VALUES($1,$2,$3,$4,$5,$6::jsonb)`, storeID, entry.Field, entry.IsRoot, entry.EntryKey, entry.ThreadID, payload); err != nil {
			return err
		}
	}
	return nil
}

func persistState(ctx context.Context, tx pgx.Tx, storeID, operationID string, before, after map[string]any) (map[string]bool, error) {
	oldEntries, nextEntries := stateEntries(before), stateEntries(after)
	keys := map[string]bool{}
	for key := range oldEntries {
		keys[key] = true
	}
	for key := range nextEntries {
		keys[key] = true
	}
	affected := map[string]bool{}
	for key := range keys {
		old, hadOld := oldEntries[key]
		next, hasNext := nextEntries[key]
		if hadOld && hasNext && jsonEqual(old.Value, next.Value) {
			continue
		}
		if hadOld && old.ThreadID != nil {
			affected[*old.ThreadID] = true
		}
		if hasNext && next.ThreadID != nil {
			affected[*next.ThreadID] = true
		}
		row := old
		if hasNext {
			row = next
		}
		if !hasNext {
			if _, err := tx.Exec(ctx, "DELETE FROM codex_store_state_entries WHERE store_id=$1 AND field=$2 AND is_root=$3 AND entry_key=$4", storeID, row.Field, row.IsRoot, row.EntryKey); err != nil {
				return nil, err
			}
		} else {
			payload, err := json.Marshal(row.Value)
			if err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO codex_store_state_entries(store_id,field,is_root,entry_key,thread_id,value)
        VALUES($1,$2,$3,$4,$5,$6::jsonb) ON CONFLICT(store_id,field,is_root,entry_key) DO UPDATE SET thread_id=EXCLUDED.thread_id,value=EXCLUDED.value
        WHERE codex_store_state_entries.value IS DISTINCT FROM EXCLUDED.value`, storeID, row.Field, row.IsRoot, row.EntryKey, row.ThreadID, payload); err != nil {
				return nil, err
			}
		}
	}
	for index, change := range stateDelta(before, after) {
		var threadID *string
		if len(change.Path) >= 2 {
			id := change.Path[1]
			if change.Path[0] == "rollout_paths" {
				if value, ok := change.Value.(string); ok {
					id = value
				} else {
					id = ""
				}
			}
			if id != "" {
				threadID = &id
			}
		}
		var payload any
		if change.Mode == "set" {
			raw, err := json.Marshal(change.Value)
			if err != nil {
				return nil, err
			}
			payload = raw
		}
		if _, err := tx.Exec(ctx, `INSERT INTO codex_store_state_changes(store_id,operation_id,change_seq,thread_id,path,mode,value)
      VALUES($1,$2,$3,$4,$5,$6,$7::json)`, storeID, operationID, index+1, threadID, change.Path, change.Mode, payload); err != nil {
			return nil, err
		}
	}
	return affected, nil
}

func persistBoundaries(ctx context.Context, tx pgx.Tx, storeID, operationID string, before, after map[string]historyEntry) (map[string]bool, error) {
	ids := map[string]bool{}
	for id := range before {
		ids[id] = true
	}
	for id := range after {
		ids[id] = true
	}
	affected := map[string]bool{}
	for id := range ids {
		old, hadOld := before[id]
		next, hasNext := after[id]
		if hadOld == hasNext && (!hadOld || old == next) {
			continue
		}
		affected[id] = true
		entry := old
		if hasNext {
			entry = next
		}
		if _, err := tx.Exec(ctx, `INSERT INTO codex_thread_revisions(store_id,thread_id,operation_id,generation,item_count,active)
      VALUES($1,$2,$3,$4,$5,$6)`, storeID, id, operationID, entry.Generation, entry.ItemCount, hasNext); err != nil {
			return nil, err
		}
		if !hasNext {
			if _, err := tx.Exec(ctx, "DELETE FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2", storeID, id); err != nil {
				return nil, err
			}
		}
	}
	return affected, nil
}

func publishReceipt(ctx context.Context, tx pgx.Tx, storeID, operationID string, affected map[string]bool, appended int64, noChangeVersion *int64) (int64, error) {
	if noChangeVersion != nil {
		_, err := tx.Exec(ctx, "UPDATE codex_store_events SET result_version=$3 WHERE store_id=$1 AND operation_id=$2", storeID, operationID, *noChangeVersion)
		return *noChangeVersion, err
	}
	var version int64
	if err := tx.QueryRow(ctx, "UPDATE codex_store_heads SET version=version+1,updated_at=NOW() WHERE store_id=$1 RETURNING version", storeID).Scan(&version); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE codex_store_events SET event_seq=$3::bigint,previous_event_seq=$3::bigint-1,result_version=$3::bigint,appended_item_count=$4
    WHERE store_id=$1 AND operation_id=$2`, storeID, operationID, version, appended); err != nil {
		return 0, err
	}
	if len(affected) > 0 {
		ids := make([]string, 0, len(affected))
		for id := range affected {
			ids = append(ids, id)
		}
		if _, err := tx.Exec(ctx, "UPDATE codex_thread_projections SET through_event_seq=$3 WHERE store_id=$1 AND thread_id=ANY($2)", storeID, ids, version); err != nil {
			return 0, err
		}
	}
	return version, nil
}

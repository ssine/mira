package miraserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func GetThreadHistory(ctx context.Context, pool *pgxpool.Pool, storeID, threadID string, generation, throughVersion *int64) (operationResponse, error) {
	var currentVersion, historyFloor int64
	err := pool.QueryRow(ctx, "SELECT version,history_floor FROM codex_store_heads WHERE store_id=$1", storeID).Scan(&currentVersion, &historyFloor)
	if err == pgx.ErrNoRows {
		currentVersion, historyFloor = 0, 0
	} else if err != nil {
		return operationResponse{}, err
	}
	version := currentVersion
	if throughVersion != nil {
		version = *throughVersion
	}
	if version < historyFloor {
		return operationResponse{Status: http.StatusGone, Body: map[string]any{"error": "requested version predates the storage migration", "code": "store_version_retired"}}, nil
	}
	var deleted int
	err = pool.QueryRow(ctx, "SELECT 1 FROM mira_thread_actions WHERE store_id=$1 AND thread_id=$2 AND action='delete'", storeID, threadID).Scan(&deleted)
	if err != nil && err != pgx.ErrNoRows {
		return operationResponse{}, err
	}
	if err == nil {
		return operationResponse{Status: 404, Body: map[string]any{"error": "thread history was permanently deleted", "code": "thread_deleted"}}, nil
	}
	manifest, err := historicalManifest(ctx, pool, storeID, version, []string{threadID})
	if err != nil {
		return operationResponse{}, err
	}
	entry, active := manifest[threadID]
	selected := int64(0)
	if generation != nil {
		selected = *generation
	} else if active {
		selected = entry.Generation
	}
	if selected == 0 || (throughVersion != nil && (!active || entry.Generation != selected)) {
		return operationResponse{Status: 404, Body: map[string]any{"error": "thread history not found"}}, nil
	}
	rows, err := pool.Query(ctx, `SELECT payload FROM codex_thread_events_versioned WHERE store_id=$1 AND thread_id=$2 AND generation=$3
      AND store_event_seq<=$4 ORDER BY item_seq`, storeID, threadID, selected, version)
	if err != nil {
		return operationResponse{}, err
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return operationResponse{}, err
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return operationResponse{}, err
		}
		items = append(items, value)
	}
	if err := rows.Err(); err != nil {
		return operationResponse{}, err
	}
	if active && entry.Generation == selected && int64(len(items)) != entry.ItemCount {
		return operationResponse{Status: 409, Body: map[string]any{"error": "thread history is incomplete"}}, nil
	}
	if len(items) == 0 && (!active || entry.Generation != selected) {
		return operationResponse{Status: 404, Body: map[string]any{"error": "thread history not found"}}, nil
	}
	return operationResponse{Status: 200, Body: map[string]any{"threadId": threadID, "generation": selected, "itemCount": len(items), "items": items}}, nil
}

func ListStoreEvents(ctx context.Context, pool *pgxpool.Pool, storeID string, after int64, limit int) ([]map[string]any, error) {
	rows, err := pool.Query(ctx, `SELECT event_seq,previous_event_seq,operation_id,event_format_version,codex_version,created_at
      FROM codex_store_events WHERE store_id=$1 AND event_seq>$2 ORDER BY event_seq LIMIT $3`, storeID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type rowValue struct {
		seq, previous int64
		operation     string
		format        int
		codex         *string
		created       time.Time
	}
	values := []rowValue{}
	for rows.Next() {
		var value rowValue
		if err := rows.Scan(&value.seq, &value.previous, &value.operation, &value.format, &value.codex, &value.created); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		manifest, err := historicalManifest(ctx, pool, storeID, value.seq, nil)
		if err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"eventSeq": value.seq, "previousEventSeq": value.previous, "operationId": value.operation, "eventFormatVersion": value.format, "codexVersion": value.codex, "createdAt": value.created.UTC().Format(time.RFC3339Nano), "historyManifest": manifest})
	}
	return result, nil
}

func ListThreadEvents(ctx context.Context, pool *pgxpool.Pool, storeID, threadID string, generation *int64, after int64, limit int) ([]map[string]any, error) {
	arguments := []any{storeID, threadID, after, limit}
	clause := ""
	if generation != nil {
		arguments = append(arguments, *generation)
		clause = "AND generation=$5"
	}
	rows, err := pool.Query(ctx, `SELECT events.generation,events.item_seq,events.store_event_seq,events.event_format_version,
      events.codex_version,events.payload,events.payload_sha256,events.created_at
      FROM codex_thread_events_versioned AS events
      WHERE events.store_id=$1 AND events.thread_id=$2 AND events.item_seq>$3
        AND NOT EXISTS(SELECT 1 FROM mira_thread_actions WHERE store_id=$1 AND thread_id=$2 AND action='delete') `+clause+`
      ORDER BY events.generation ASC,events.item_seq ASC LIMIT $4`, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var generationValue, itemSeq, eventSeq int64
		var format int
		var codex *string
		var raw []byte
		var hash string
		var created time.Time
		if err := rows.Scan(&generationValue, &itemSeq, &eventSeq, &format, &codex, &raw, &hash, &created); err != nil {
			return nil, err
		}
		var payload any
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"generation": generationValue, "itemSeq": itemSeq, "storeEventSeq": eventSeq, "eventFormatVersion": format, "codexVersion": codex, "payload": payload, "payloadSha256": hash, "createdAt": created.UTC().Format(time.RFC3339Nano)})
	}
	return result, rows.Err()
}

func RebuildSnapshot(ctx context.Context, pool *pgxpool.Pool, storeID string) (operationResponse, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return operationResponse{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockScope(ctx, tx, storeID, nil); err != nil {
		return operationResponse{}, err
	}
	rows, err := tx.Query(ctx, `SELECT s.path,s.mode,s.value FROM codex_store_state_changes s JOIN codex_store_events c USING(store_id,operation_id)
      WHERE s.store_id=$1 AND c.event_seq IS NOT NULL AND (s.thread_id IS NULL OR NOT EXISTS(
        SELECT 1 FROM mira_thread_actions a WHERE a.store_id=s.store_id AND a.thread_id=s.thread_id AND a.action='delete')) ORDER BY c.event_seq,s.change_seq`, storeID)
	if err != nil {
		return operationResponse{}, err
	}
	state := map[string]any{}
	for rows.Next() {
		var path []string
		var mode string
		var raw []byte
		if err := rows.Scan(&path, &mode, &raw); err != nil {
			rows.Close()
			return operationResponse{}, err
		}
		var value any
		if mode == "set" {
			if err := json.Unmarshal(raw, &value); err != nil {
				rows.Close()
				return operationResponse{}, err
			}
		}
		writePath(state, requestedStateChange{Path: path, Mode: mode, Value: value})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return operationResponse{}, err
	}
	head, err := currentHead(ctx, tx, storeID, nil)
	if err != nil {
		return operationResponse{}, err
	}
	if head.Version == 0 {
		return operationResponse{Status: 404, Body: map[string]any{"error": "store has no canonical events"}}, nil
	}
	manifest, err := historicalManifest(ctx, tx, storeID, head.Version, nil)
	if err != nil {
		return operationResponse{}, err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM codex_thread_projections WHERE store_id=$1", storeID); err != nil {
		return operationResponse{}, err
	}
	if err := rebuildStateEntries(ctx, tx, storeID, state); err != nil {
		return operationResponse{}, err
	}
	affected := map[string]bool{}
	for id := range manifest {
		affected[id] = true
	}
	if err := replaceProjections(ctx, tx, storeID, state, manifest, head.Version, affected, affected); err != nil {
		return operationResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return operationResponse{}, err
	}
	return operationResponse{Status: 200, Body: map[string]any{"storeId": storeID, "version": head.Version, "rebuilt": true}}, nil
}

func parseOptionalInt(value string, minimum int64) (*int64, bool) {
	if value == "" {
		return nil, true
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < minimum {
		return nil, false
	}
	return &parsed, true
}

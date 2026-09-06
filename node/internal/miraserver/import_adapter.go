package miraserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	serverimports "github.com/ssine/mira/node/internal/miraserver/imports"
)

// importThreadStore keeps the import package independent of the HTTP package
// while preserving ThreadStore's locks, generations and operation receipts.
type importThreadStore struct {
	pool *pgxpool.Pool
}

func (store *importThreadStore) AssertNotDeleted(ctx context.Context, storeID string, threadIDs []string) error {
	return assertThreadsNotDeleted(ctx, store.pool, storeID, threadIDs)
}

func (store *importThreadStore) Head(ctx context.Context, storeID string) (serverimports.Head, error) {
	head, err := currentHead(ctx, store.pool, storeID, nil)
	if err != nil {
		return serverimports.Head{}, err
	}
	manifest := make(map[string]serverimports.HistoryEntry, len(head.HistoryManifest))
	for threadID, entry := range head.HistoryManifest {
		manifest[threadID] = serverimports.HistoryEntry{Generation: entry.Generation, ItemCount: entry.ItemCount}
	}
	return serverimports.Head{Version: head.Version, State: head.State, HistoryManifest: manifest}, nil
}

func (store *importThreadStore) History(ctx context.Context, storeID, threadID string, generation, version int64) ([]json.RawMessage, error) {
	head, err := currentHead(ctx, store.pool, storeID, []string{threadID})
	if err != nil {
		return nil, err
	}
	if version < head.HistoryFloor {
		return nil, &HTTPError{Status: http.StatusGone, Code: "store_version_retired", Message: "requested version predates the storage migration"}
	}
	manifest, err := historicalManifest(ctx, store.pool, storeID, version, []string{threadID})
	if err != nil {
		return nil, err
	}
	entry, found := manifest[threadID]
	if !found || entry.Generation != generation {
		return nil, &HTTPError{Status: http.StatusNotFound, Code: "not_found", Message: "thread history not found"}
	}
	rows, err := store.pool.Query(ctx, `SELECT payload FROM codex_thread_events_versioned
		WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND store_event_seq<=$4 ORDER BY item_seq`, storeID, threadID, generation, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []json.RawMessage{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		items = append(items, append(json.RawMessage(nil), raw...))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if int64(len(items)) != entry.ItemCount {
		return nil, &HTTPError{Status: http.StatusConflict, Code: "incomplete_history", Message: "thread history is incomplete"}
	}
	return items, nil
}

func (store *importThreadStore) CommitDelta(ctx context.Context, storeID string, delta serverimports.Delta, headers http.Header) (serverimports.CommitResult, error) {
	stateChanges := make([]any, len(delta.StateChanges))
	for index, change := range delta.StateChanges {
		stateChanges[index] = change
	}
	historyChanges := make([]any, len(delta.HistoryChanges))
	for index, original := range delta.HistoryChanges {
		change := make(map[string]any, len(original))
		for key, value := range original {
			change[key] = value
		}
		if rawItems, ok := change["items"].([]json.RawMessage); ok {
			items := make([]any, len(rawItems))
			for itemIndex, raw := range rawItems {
				// Keep the exact JSON spelling. In particular, decoding here would
				// replace unpaired UTF-16 surrogates in otherwise valid provenance.
				items[itemIndex] = append(json.RawMessage(nil), raw...)
			}
			change["items"] = items
		}
		historyChanges[index] = change
	}
	result, err := CommitDelta(ctx, store.pool, storeID, map[string]any{
		"expectedVersion": delta.ExpectedVersion, "stateChanges": stateChanges, "historyChanges": historyChanges,
	}, headers)
	if err != nil {
		return serverimports.CommitResult{}, err
	}
	if result.Status != http.StatusOK {
		return serverimports.CommitResult{}, resultError(result.Status, result.Body)
	}
	version, ok := integer(result.Body["version"])
	if !ok {
		return serverimports.CommitResult{}, fmt.Errorf("ThreadStore commit returned an invalid version")
	}
	noChange, _ := result.Body["noChange"].(bool)
	return serverimports.CommitResult{Version: version, NoChange: noChange}, nil
}

func resultError(status int, body map[string]any) error {
	message, _ := body["error"].(string)
	if message == "" {
		message = http.StatusText(status)
	}
	code, _ := body["code"].(string)
	return &HTTPError{Status: status, Code: code, Message: message}
}

func (store *importThreadStore) CommitImported(ctx context.Context, storeID string, value serverimports.ImportCommit, progress serverimports.Context) (serverimports.CommitResult, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return serverimports.CommitResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockScope(ctx, tx, storeID, []string{value.ThreadID}); err != nil {
		return serverimports.CommitResult{}, err
	}
	if err := assertThreadsNotDeleted(ctx, tx, storeID, []string{value.ThreadID}); err != nil {
		return serverimports.CommitResult{}, err
	}
	head, err := currentHead(ctx, tx, storeID, []string{value.ThreadID})
	if err != nil {
		return serverimports.CommitResult{}, err
	}
	existing, hasExisting := head.HistoryManifest[value.ThreadID]
	previousCount := int64(0)
	if hasExisting {
		previousCount = existing.ItemCount
	}
	segments := value.Segments
	if len(segments) == 0 {
		segments = []serverimports.Segment{{ImportID: value.ImportID, FirstLine: 1, Count: value.Count}}
	}
	if err := saveImportSegments(ctx, tx, value.ImportID, segments); err != nil {
		return serverimports.CommitResult{}, err
	}
	batchSize := int64(100)
	replace := false
	shared := min(value.Count, previousCount)
	for offset := int64(0); offset < shared; offset += batchSize {
		if err := ctx.Err(); err != nil {
			return serverimports.CommitResult{}, err
		}
		limit := min(batchSize, shared-offset)
		source, err := importSourceBatch(ctx, tx, segments, offset, limit)
		if err != nil {
			return serverimports.CommitResult{}, err
		}
		target, err := importExistingBatch(ctx, tx, storeID, value.ThreadID, existing.Generation, head.Version, offset, limit)
		if err != nil {
			return serverimports.CommitResult{}, err
		}
		if int64(len(source)) != limit || int64(len(target)) != limit {
			return serverimports.CommitResult{}, fmt.Errorf("incomplete canonical import history")
		}
		for index := range source {
			desired, err := value.Normalize(source[index])
			if err != nil {
				return serverimports.CommitResult{}, err
			}
			normalizedTarget, err := value.Normalize(target[index])
			if err != nil {
				return serverimports.CommitResult{}, err
			}
			if !rawJSONEqual(normalizedTarget, desired) {
				return serverimports.CommitResult{}, &HTTPError{Status: http.StatusConflict, Code: "history_diverged", Message: "本地会话与数据库历史已分叉；源记录已保留，未覆盖现有会话"}
			}
			if !rawJSONEqual(target[index], desired) {
				replace = true
			}
		}
		if progress.OnProgress != nil {
			progress.OnProgress(map[string]any{"phase": "validating", "records": offset + limit, "totalRecords": shared})
		}
	}
	state, err := cloneObject(head.State)
	if err != nil {
		return serverimports.CommitResult{}, err
	}
	created := object(state["created_threads"])
	if _, found := created[value.ThreadID]; !found {
		created[value.ThreadID] = value.Created
	} else {
		thread := object(created[value.ThreadID])
		thread["history_mode"] = "legacy"
		created[value.ThreadID] = thread
	}
	state["created_threads"] = created
	metadata := object(state["metadata_updates"])
	if _, found := metadata[value.ThreadID]; !found {
		metadata[value.ThreadID] = value.Metadata
	}
	state["metadata_updates"] = metadata
	if hasExisting && previousCount >= value.Count && !replace && jsonEqual(state, head.State) {
		if err := bindImportedRuntime(ctx, tx, storeID, value.ThreadID, value.RuntimeNodeID); err != nil {
			return serverimports.CommitResult{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE mira_codex_session_imports SET status='imported',store_event_seq=$2,error_code=NULL,updated_at=NOW() WHERE import_id=$1`, value.ImportID, head.Version); err != nil {
			return serverimports.CommitResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return serverimports.CommitResult{}, err
		}
		return serverimports.CommitResult{Version: head.Version, NoChange: true}, nil
	}
	manifest := make(map[string]historyEntry, len(head.HistoryManifest)+1)
	for threadID, entry := range head.HistoryManifest {
		manifest[threadID] = entry
	}
	generation := int64(1)
	if hasExisting {
		generation = existing.Generation
		if replace {
			generation++
		}
	}
	total := max(value.Count, previousCount)
	manifest[value.ThreadID] = historyEntry{Generation: generation, ItemCount: total}
	operationID, err := randomUUID()
	if err != nil {
		return serverimports.CommitResult{}, err
	}
	codexVersion := value.CodexVersion
	if _, err := beginReceipt(ctx, tx, storeID, operationID, map[string]any{
		"importId": value.ImportID, "threadId": value.ThreadID, "count": value.Count,
	}, &codexVersion); err != nil {
		return serverimports.CommitResult{}, err
	}
	if !hasExisting {
		if err := repairNewGenerations(ctx, tx, storeID, head.HistoryManifest, manifest, nil); err != nil {
			return serverimports.CommitResult{}, err
		}
	}
	affected, err := persistState(ctx, tx, storeID, operationID, head.State, state)
	if err != nil {
		return serverimports.CommitResult{}, err
	}
	metadataAffected := make(map[string]bool, len(affected))
	for threadID := range affected {
		metadataAffected[threadID] = true
	}
	boundaries, err := persistBoundaries(ctx, tx, storeID, operationID, head.HistoryManifest, manifest)
	if err != nil {
		return serverimports.CommitResult{}, err
	}
	for threadID := range boundaries {
		affected[threadID] = true
	}
	writeGeneration := manifest[value.ThreadID].Generation
	start := previousCount
	if replace {
		start = 0
	}
	for offset := start; offset < total; {
		if err := ctx.Err(); err != nil {
			return serverimports.CommitResult{}, err
		}
		fromSource := offset < value.Count
		limit := min(batchSize, total-offset)
		if fromSource {
			limit = min(limit, value.Count-offset)
		}
		var rows []json.RawMessage
		if fromSource {
			rows, err = importSourceBatch(ctx, tx, segments, offset, limit)
		} else {
			rows, err = importExistingBatch(ctx, tx, storeID, value.ThreadID, existing.Generation, head.Version, offset, limit)
		}
		if err != nil {
			return serverimports.CommitResult{}, err
		}
		if int64(len(rows)) != limit {
			return serverimports.CommitResult{}, fmt.Errorf("incomplete staged import history")
		}
		appends := make([]historyAppend, 0, len(rows))
		for _, raw := range rows {
			normalized, err := value.Normalize(raw)
			if err != nil {
				return serverimports.CommitResult{}, err
			}
			offset++
			appends = append(appends, historyAppend{ThreadID: value.ThreadID, Generation: writeGeneration, ItemSeq: offset, Payload: append(json.RawMessage(nil), normalized...)})
		}
		if err := insertHistoryAppends(ctx, tx, storeID, operationID, &codexVersion, appends); err != nil {
			return serverimports.CommitResult{}, err
		}
		if progress.OnProgress != nil {
			progress.OnProgress(map[string]any{"phase": "publishing", "records": offset, "totalRecords": total})
		}
	}
	if err := replaceProjections(ctx, tx, storeID, state, manifest, 1, affected, metadataAffected); err != nil {
		return serverimports.CommitResult{}, err
	}
	appended := total - start
	version, err := publishReceipt(ctx, tx, storeID, operationID, affected, appended, nil)
	if err != nil {
		return serverimports.CommitResult{}, err
	}
	if err := bindImportedRuntime(ctx, tx, storeID, value.ThreadID, value.RuntimeNodeID); err != nil {
		return serverimports.CommitResult{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE mira_codex_session_imports SET status='imported',store_event_seq=$2,error_code=NULL,updated_at=NOW() WHERE import_id=$1`, value.ImportID, version); err != nil {
		return serverimports.CommitResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return serverimports.CommitResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return serverimports.CommitResult{}, err
	}
	return serverimports.CommitResult{Version: version}, nil
}

func bindImportedRuntime(ctx context.Context, tx pgx.Tx, storeID, threadID string, nodeID *string) error {
	if nodeID == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO mira_codex_thread_runtimes(store_id,thread_id,node_id,bound_at)
		VALUES($1,$2,$3,NOW()) ON CONFLICT(store_id,thread_id)
		DO UPDATE SET node_id=EXCLUDED.node_id,bound_at=EXCLUDED.bound_at`, storeID, threadID, *nodeID)
	return err
}

func saveImportSegments(ctx context.Context, tx pgx.Tx, importID string, segments []serverimports.Segment) error {
	for index, segment := range segments {
		boundary, err := json.Marshal(segment.Boundary)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO mira_codex_session_import_segments
			(import_id,segment_index,source_import_id,first_line_seq,item_count,end_position)
			VALUES($1,$2,$3,$4,$5,$6::jsonb) ON CONFLICT DO NOTHING`,
			importID, index, segment.ImportID, segment.FirstLine, segment.Count, boundary); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT source_import_id::text,first_line_seq::text,item_count::text,end_position
		FROM mira_codex_session_import_segments WHERE import_id=$1 ORDER BY segment_index`, importID)
	if err != nil {
		return err
	}
	defer rows.Close()
	saved := []serverimports.Segment{}
	for rows.Next() {
		var segment serverimports.Segment
		var first, count string
		var boundary []byte
		if err := rows.Scan(&segment.ImportID, &first, &count, &boundary); err != nil {
			return err
		}
		segment.FirstLine, err = strconv.ParseInt(first, 10, 64)
		if err != nil {
			return err
		}
		segment.Count, err = strconv.ParseInt(count, 10, 64)
		if err != nil {
			return err
		}
		if len(boundary) > 0 && string(boundary) != "null" {
			segment.Boundary = &serverimports.Boundary{}
			if err := json.Unmarshal(boundary, segment.Boundary); err != nil {
				return err
			}
		}
		saved = append(saved, segment)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !jsonEqual(saved, segments) {
		return &HTTPError{Status: http.StatusConflict, Code: "history_diverged", Message: "同一源会话的祖先历史发生变化，未覆盖已保存的引用关系"}
	}
	return nil
}

func importSourceBatch(ctx context.Context, query dbtx, segments []serverimports.Segment, after, limit int64) ([]json.RawMessage, error) {
	result := []json.RawMessage{}
	start := int64(0)
	for _, segment := range segments {
		skip := max(int64(0), after-start)
		start += segment.Count
		if skip >= segment.Count {
			continue
		}
		take := min(limit-int64(len(result)), segment.Count-skip)
		rows, err := query.Query(ctx, `SELECT raw_record FROM mira_codex_session_import_records
			WHERE import_id=$1 AND line_seq >= $2 AND line_seq < $3 ORDER BY line_seq`,
			segment.ImportID, segment.FirstLine+skip, segment.FirstLine+skip+take)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return nil, err
			}
			result = append(result, append(json.RawMessage(nil), raw...))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		if int64(len(result)) == limit {
			break
		}
	}
	return result, nil
}

func importExistingBatch(ctx context.Context, query dbtx, storeID, threadID string, generation, version, after, limit int64) ([]json.RawMessage, error) {
	rows, err := query.Query(ctx, `SELECT payload FROM codex_thread_events_versioned
		WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq>$4
		AND store_event_seq<=$5 ORDER BY item_seq LIMIT $6`, storeID, threadID, generation, after, version, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []json.RawMessage{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		result = append(result, append(json.RawMessage(nil), raw...))
	}
	return result, rows.Err()
}

func rawJSONEqual(left, right json.RawMessage) bool {
	a, err := parseLosslessJSON(left)
	if err != nil {
		return false
	}
	b, err := parseLosslessJSON(right)
	return err == nil && reflect.DeepEqual(a, b)
}

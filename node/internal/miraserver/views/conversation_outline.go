package views

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// outlineCatchUpBudget bounds how long an outline read indexes its own thread
// before telling the client to use paged history for now.
const outlineCatchUpBudget = 3 * time.Second

type outlineRecord struct {
	sequence   int64
	record     map[string]any
	recordedAt *time.Time
}

// loadOutlineRecords reads a thread's slim records under one snapshot. It
// returns the indexed count the records cover and the projection head. With
// after (valid only for the same generation, otherwise cleared), it starts at
// the turn containing the first record after that count instead of seq 1.
func (service *Service) loadOutlineRecords(ctx context.Context, storeID, threadID string, after *int64, knownGeneration *int64) (generation, indexed, itemCount, storeVersion int64, records []outlineRecord, found bool, effectiveAfter *int64, err error) {
	err = pgx.BeginTxFunc(ctx, service.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT p.active_generation,p.item_count,p.through_event_seq,s.indexed_count FROM codex_thread_projections p
		  JOIN mira_conversation_index_state s ON s.store_id=p.store_id AND s.thread_id=p.thread_id AND s.generation=p.active_generation AND s.projector_version=$3
		  WHERE p.store_id=$1 AND p.thread_id=$2`, storeID, threadID, ConversationIndexVersion).Scan(&generation, &itemCount, &storeVersion, &indexed); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		found = true
		from := int64(1)
		if after != nil && knownGeneration != nil && *knownGeneration == generation {
			effectiveAfter = after
			if err := tx.QueryRow(ctx, `SELECT COALESCE(max(item_seq),1) FROM mira_conversation_records
			  WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq<=$4
			    AND kind IN ('turn_context','event_msg:task_started','event_msg:turn_started')`, storeID, threadID, generation, *after+1).Scan(&from); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT item_seq,recorded_at,record FROM mira_conversation_records
		  WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq>=$5 AND item_seq<=$4 ORDER BY item_seq`, storeID, threadID, generation, indexed, from)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sequence int64
			var recordedAt *time.Time
			var raw []byte
			if err := rows.Scan(&sequence, &recordedAt, &raw); err != nil {
				return err
			}
			record, err := decodeObject(raw)
			if err != nil {
				return err
			}
			records = append(records, outlineRecord{sequence, record, recordedAt})
		}
		return rows.Err()
	})
	return
}

// GetTranscriptOutline projects a whole Codex conversation from its index:
// every message, reasoning summary and tool skeleton in canonical order, with
// tool bodies left to the existing per-range detail reads. With after, it
// returns only entries that changed since that item count: new records and
// every entry of a turn that new records touched.
// A client's after count is meaningful only within the generation it read;
// a different (or omitted) generation returns the whole outline.
func (service *Service) GetTranscriptOutline(ctx context.Context, storeID, threadID string, after, knownGeneration *int64) (Result, error) {
	complete, err := service.IndexCodexThread(ctx, storeID, threadID, time.Now().Add(outlineCatchUpBudget))
	if err != nil {
		return Result{}, err
	}
	if !complete {
		service.WakeConversationIndexer()
		return errorResult(409, "conversation index is still being built", "outline_pending"), nil
	}
	// Incremental reads project only from the turn that the new records
	// continue; a whole outline of a long thread is read once per client.
	generation, indexed, itemCount, storeVersion, records, found, after, err := service.loadOutlineRecords(ctx, storeID, threadID, after, knownGeneration)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return errorResult(404, "thread history not found", "not_found"), nil
	}
	items := make([]map[string]any, len(records))
	sequences := make([]int64, len(records))
	recordedAt := make(map[int64]string, len(records))
	lastTouch := map[string]int64{}
	touchedTurns := map[string]bool{}
	for index, entry := range records {
		items[index], sequences[index] = entry.record, entry.sequence
		if entry.recordedAt != nil {
			recordedAt[entry.sequence] = formatTime(*entry.recordedAt)
		}
		payload := object(entry.record["payload"])
		for _, id := range []string{stringValue(payload["call_id"]), stringValue(object(payload["item"])["id"])} {
			if id != "" && entry.sequence > lastTouch[id] {
				lastTouch[id] = entry.sequence
			}
		}
		if after != nil && entry.sequence > *after {
			if turn := stringValue(first(payload["turn_id"], object(payload["internal_chat_message_metadata_passthrough"])["turn_id"])); turn != "" {
				touchedTurns[turn] = true
			}
		}
	}
	projected := ProjectCodexTranscript(items, ProjectionOptions{Fragments: true, RecordedAt: recordedAt, Sequences: sequences})
	referenceTranscriptImages(projected, storeID, threadID, generation)
	trace := make([]map[string]any, 0, len(projected))
	for _, item := range projected {
		sequence, _ := safeInteger(item["sourceItemSeq"])
		itemID := stringValue(item["itemId"])
		if after != nil && sequence <= *after && lastTouch[itemID] <= *after && !touchedTurns[stringValue(item["turnId"])] {
			continue
		}
		if stringValue(item["kind"]) != "tool" {
			trace = append(trace, item)
			continue
		}
		end := max(lastTouch[itemID], sequence) + 1
		copy := cloneMap(item)
		copy["toolDetail"] = map[string]any{"pages": []any{map[string]any{
			"cursor": fmt.Sprintf("t2:%d:%d:%d", generation, end, indexed), "from": sequence, "limit": 200, "loaded": false}}}
		copy["body"] = ""
		if fragment := object(copy["toolFragment"]); fragment != nil {
			copy["toolFragment"] = map[string]any{"materialized": fragment["materialized"] == true, "hasInput": fragment["input"] != nil, "hasOutput": fragment["output"] != nil}
		}
		delete(copy, "images")
		trace = append(trace, copy)
	}
	if err := service.attachFileContexts(ctx, storeID, threadID, generation, trace); err != nil {
		return Result{}, err
	}
	body := map[string]any{"storeId": storeID, "threadId": threadID, "generation": generation, "itemCount": indexed,
		"storeVersion": storeVersion, "trace": trace, "projectionVersion": 2, "totalTraceItems": nil,
		"outline": true, "nextCursor": nil, "pageCursor": fmt.Sprintf("t2:%d:%d:%d", generation, indexed+1, indexed)}
	if indexed != itemCount {
		body["storeVersion"] = nil // The head moved while reading; the next sync continues from itemCount.
	}
	if after != nil {
		body["after"] = *after
	} else {
		body["totalTraceItems"] = len(projected)
	}
	return Result{Status: 200, Body: body}, nil
}

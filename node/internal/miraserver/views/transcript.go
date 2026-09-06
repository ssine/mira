package views

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

var tailCursorPattern = regexp.MustCompile(`^t2:(\d+):(\d+):(\d+)$`)

type transcriptRow struct {
	sequence  int64
	payload   map[string]any
	createdAt *time.Time
}

func errorResult(status int, message, code string) Result {
	return Result{Status: status, Body: map[string]any{"error": message, "code": code}}
}

func addTranscriptTiming(options TranscriptOptions, name string, started time.Time) {
	if options.Timings != nil {
		options.Timings[name] += time.Since(started)
	}
}

// GetTranscript serves both the legacy fully projected reader and the bounded
// V2 raw-sequence tail reader.
func (service *Service) GetTranscript(ctx context.Context, storeID, threadID string, options TranscriptOptions) (Result, error) {
	if options.Tail {
		return service.getTranscriptTail(ctx, storeID, threadID, options)
	}
	if options.Limit <= 0 {
		options.Limit = 60
	}
	return service.getTranscriptLegacy(ctx, storeID, threadID, options)
}

func (service *Service) getTranscriptLegacy(ctx context.Context, storeID, threadID string, options TranscriptOptions) (Result, error) {
	var versionText, floorText *string
	var generationText, countText *string
	dbHeadStarted := time.Now()
	err := service.pool.QueryRow(ctx, `SELECT heads.version::text,heads.history_floor::text,
		projections.active_generation::text,projections.item_count::text
		FROM codex_store_heads heads LEFT JOIN codex_thread_projections projections
		ON projections.store_id=heads.store_id AND projections.thread_id=$2 WHERE heads.store_id=$1`, storeID, threadID).
		Scan(&versionText, &floorText, &generationText, &countText)
	addTranscriptTiming(options, "db_head", dbHeadStarted)
	if errors.Is(err, pgx.ErrNoRows) || generationText == nil {
		return errorResult(404, "thread history not found", "not_found"), nil
	}
	if err != nil {
		return Result{}, err
	}
	version, err := postgresTextInt(*versionText)
	if err != nil {
		return Result{}, err
	}
	floor, err := postgresTextInt(*floorText)
	if err != nil {
		return Result{}, err
	}
	if version < floor {
		return errorResult(410, "requested version predates the storage migration", "store_version_retired"), nil
	}
	dbHistoryStarted := time.Now()
	var deleted bool
	err = service.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_thread_actions WHERE store_id=$1 AND thread_id=$2 AND action='delete')`, storeID, threadID).Scan(&deleted)
	if err != nil {
		return Result{}, err
	}
	if deleted {
		return errorResult(404, "thread history was permanently deleted", "thread_deleted"), nil
	}
	generation, err := postgresTextInt(*generationText)
	if err != nil {
		return Result{}, err
	}
	itemCount, err := postgresTextInt(*countText)
	if err != nil {
		return Result{}, err
	}
	rows, err := service.pool.Query(ctx, `SELECT payload FROM codex_thread_events_versioned
		WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND store_event_seq<=$4 ORDER BY item_seq`, storeID, threadID, generation, version)
	if err != nil {
		return Result{}, err
	}
	items := []map[string]any{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return Result{}, err
		}
		record, err := decodeObject(raw)
		if err != nil {
			rows.Close()
			return Result{}, err
		}
		items = append(items, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Result{}, err
	}
	rows.Close()
	addTranscriptTiming(options, "db_history", dbHistoryStarted)
	if int64(len(items)) != itemCount {
		return Result{Status: 409, Body: map[string]any{"error": "thread history is incomplete"}}, nil
	}
	projectionStarted := time.Now()
	projected := ProjectCodexTranscript(items, ProjectionOptions{})
	var cursor *int
	if options.Cursor != nil {
		value, parseErr := strconv.Atoi(*options.Cursor)
		if parseErr != nil || value < 0 {
			return errorResult(400, "invalid transcript cursor", "invalid_request"), nil
		}
		cursor = &value
	}
	page := PaginateCodexTranscript(projected, cursor, options.Limit)
	addTranscriptTiming(options, "projection", projectionStarted)
	body := map[string]any{"storeId": storeID, "threadId": threadID, "storeVersion": version, "generation": generation, "itemCount": itemCount}
	for key, value := range page {
		body[key] = value
	}
	return Result{Status: 200, Body: body}, nil
}

func parseTailCursor(value *string) (generation, end, snapshot int64, valid bool) {
	if value == nil {
		return 0, 0, 0, true
	}
	match := tailCursorPattern.FindStringSubmatch(*value)
	if match == nil {
		return
	}
	values := []*int64{&generation, &end, &snapshot}
	for index, target := range values {
		parsed, err := strconv.ParseInt(match[index+1], 10, 64)
		if err != nil || parsed < 0 || parsed > 9_007_199_254_740_991 {
			return 0, 0, 0, false
		}
		*target = parsed
	}
	return generation, end, snapshot, true
}

func (service *Service) queryTranscriptRows(ctx context.Context, storeID, threadID string, generation, end int64, limit int) ([]transcriptRow, error) {
	rows, err := service.pool.Query(ctx, `SELECT item_seq::text,payload,created_at FROM codex_thread_events
		WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq<$4 ORDER BY item_seq DESC LIMIT $5`, storeID, threadID, generation, end, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []transcriptRow{}
	for rows.Next() {
		var seq string
		var raw []byte
		var created time.Time
		if err := rows.Scan(&seq, &raw, &created); err != nil {
			return nil, err
		}
		sequence, err := postgresTextInt(seq)
		if err != nil {
			return nil, err
		}
		payload, err := decodeObject(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, transcriptRow{sequence, payload, &created})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result, nil
}

func (service *Service) contextBefore(ctx context.Context, storeID, threadID string, generation, before int64) (*transcriptRow, error) {
	for before > 1 {
		var seq string
		var raw []byte
		var created time.Time
		err := service.pool.QueryRow(ctx, `SELECT item_seq::text,payload,created_at FROM codex_thread_events
			WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq<$4
			AND payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(task_started|turn_started|turn_context)"'
			ORDER BY item_seq DESC LIMIT 1`, storeID, threadID, generation, before).Scan(&seq, &raw, &created)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		sequence, err := postgresTextInt(seq)
		if err != nil {
			return nil, err
		}
		record, err := decodeObject(raw)
		if err != nil {
			return nil, err
		}
		valid := stringValue(record["type"]) == "turn_context" || stringValue(record["type"]) == "event_msg" && includes(activityStarts, stringValue(object(record["payload"])["type"]))
		if valid {
			return &transcriptRow{sequence, record, &created}, nil
		}
		before = sequence
	}
	return nil, nil
}

func (service *Service) restoreImportedTimestamps(ctx context.Context, storeID, threadID string, rows []transcriptRow) (map[int64]string, error) {
	recorded := map[int64]string{}
	for _, row := range rows {
		if row.createdAt != nil {
			recorded[row.sequence] = formatTime(*row.createdAt)
		}
	}
	var importID, sourceCount string
	err := service.pool.QueryRow(ctx, `SELECT import_id::text,source_item_count::text FROM mira_codex_session_imports
		WHERE store_id=$1 AND thread_id=$2 AND status='imported' ORDER BY store_event_seq DESC,created_at DESC LIMIT 1`, storeID, threadID).Scan(&importID, &sourceCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return recorded, nil
	}
	if err != nil {
		return nil, err
	}
	type segment struct {
		importID     string
		first, count int64
	}
	segments := []segment{}
	segmentRows, err := service.pool.Query(ctx, `SELECT source_import_id::text,first_line_seq::text,item_count::text
		FROM mira_codex_session_import_segments WHERE import_id=$1 ORDER BY segment_index`, importID)
	if err != nil {
		return nil, err
	}
	for segmentRows.Next() {
		var id, first, count string
		if err := segmentRows.Scan(&id, &first, &count); err != nil {
			segmentRows.Close()
			return nil, err
		}
		firstValue, err := postgresTextInt(first)
		if err != nil {
			segmentRows.Close()
			return nil, err
		}
		countValue, err := postgresTextInt(count)
		if err != nil {
			segmentRows.Close()
			return nil, err
		}
		segments = append(segments, segment{id, firstValue, countValue})
	}
	if err := segmentRows.Err(); err != nil {
		segmentRows.Close()
		return nil, err
	}
	segmentRows.Close()
	if len(segments) == 0 {
		count, err := postgresTextInt(sourceCount)
		if err != nil {
			return nil, err
		}
		segments = []segment{{importID, 1, count}}
	}
	offset := int64(0)
	for _, part := range segments {
		lineBySequence := map[int64]int64{}
		lines := []int64{}
		for _, row := range rows {
			if row.sequence > offset && row.sequence <= offset+part.count {
				line := part.first + row.sequence - offset - 1
				lineBySequence[row.sequence] = line
				lines = append(lines, line)
			}
		}
		if len(lines) > 0 {
			rawRows, err := service.pool.Query(ctx, `SELECT line_seq::text,raw_record FROM mira_codex_session_import_records WHERE import_id=$1 AND line_seq=ANY($2::bigint[])`, part.importID, lines)
			if err != nil {
				return nil, err
			}
			originals := map[int64]map[string]any{}
			for rawRows.Next() {
				var line string
				var raw []byte
				if err := rawRows.Scan(&line, &raw); err != nil {
					rawRows.Close()
					return nil, err
				}
				lineValue, err := postgresTextInt(line)
				if err != nil {
					rawRows.Close()
					return nil, err
				}
				record, err := decodeObject(raw)
				if err != nil {
					rawRows.Close()
					return nil, err
				}
				originals[lineValue] = record
			}
			if err := rawRows.Err(); err != nil {
				rawRows.Close()
				return nil, err
			}
			rawRows.Close()
			for index := range rows {
				line, ok := lineBySequence[rows[index].sequence]
				if !ok {
					continue
				}
				original := originals[line]
				if original == nil || stringValue(original["type"]) != stringValue(rows[index].payload["type"]) || !reflect.DeepEqual(original["payload"], rows[index].payload["payload"]) {
					continue
				}
				delete(recorded, rows[index].sequence)
				timestamp := recordTimestamp(original)
				if timestamp != "" && recordTimestamp(rows[index].payload) == "" {
					replacement := cloneMap(rows[index].payload)
					replacement["timestamp"] = timestamp
					rows[index].payload = replacement
				}
			}
		}
		offset += part.count
	}
	return recorded, nil
}

func transcriptToolDetails(trace []map[string]any, cursor string, limit int, loaded bool) []map[string]any {
	result := make([]map[string]any, len(trace))
	for index, item := range trace {
		if stringValue(item["kind"]) != "tool" {
			result[index] = item
			continue
		}
		copy := cloneMap(item)
		page := map[string]any{"cursor": cursor, "limit": limit, "loaded": loaded}
		copy["toolDetail"] = map[string]any{"pages": []any{page}}
		if loaded {
			result[index] = copy
			continue
		}
		copy["body"] = ""
		if fragment := object(copy["toolFragment"]); fragment != nil {
			copy["toolFragment"] = map[string]any{"materialized": fragment["materialized"] == true, "hasInput": fragment["input"] != nil, "hasOutput": fragment["output"] != nil}
		}
		images := []map[string]any{}
		for _, image := range mapImages(copy["images"]) {
			if path := stringValue(image["path"]); path != "" {
				images = append(images, map[string]any{"path": path})
			}
		}
		if len(images) > 0 {
			copy["images"] = images
		} else {
			delete(copy, "images")
		}
		result[index] = copy
	}
	return result
}

func (service *Service) getTranscriptTail(ctx context.Context, storeID, threadID string, options TranscriptOptions) (Result, error) {
	cursorGeneration, cursorEnd, cursorSnapshot, valid := parseTailCursor(options.Cursor)
	if !valid {
		return errorResult(400, "invalid transcript cursor", "invalid_request"), nil
	}
	var generationText, countText, versionText string
	dbHeadStarted := time.Now()
	err := service.pool.QueryRow(ctx, `SELECT active_generation::text,item_count::text,through_event_seq::text FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2`, storeID, threadID).Scan(&generationText, &countText, &versionText)
	addTranscriptTiming(options, "db_head", dbHeadStarted)
	if errors.Is(err, pgx.ErrNoRows) {
		return errorResult(404, "thread history not found", "not_found"), nil
	}
	if err != nil {
		return Result{}, err
	}
	generation, err := postgresTextInt(generationText)
	if err != nil {
		return Result{}, err
	}
	itemCount, err := postgresTextInt(countText)
	if err != nil {
		return Result{}, err
	}
	storeVersion, err := postgresTextInt(versionText)
	if err != nil {
		return Result{}, err
	}
	if options.Cursor != nil && (cursorGeneration != generation || cursorSnapshot > itemCount) {
		return errorResult(409, "会话历史已更新，请重新加载", "stale_transcript_cursor"), nil
	}
	snapshotCount := itemCount
	end := snapshotCount + 1
	if options.Cursor != nil {
		snapshotCount, end = cursorSnapshot, cursorEnd
	}
	if end < 1 || end > snapshotCount+1 {
		return errorResult(400, "invalid transcript cursor", "invalid_request"), nil
	}
	limit := options.Limit
	if limit <= 0 {
		limit = 60
	}
	if limit < 10 {
		limit = 10
	}
	if limit > 200 {
		limit = 200
	}
	windowSize := limit * 4
	if windowSize < 120 {
		windowSize = 120
	}
	dbTailStarted := time.Now()
	rows, err := service.queryTranscriptRows(ctx, storeID, threadID, generation, end, windowSize)
	addTranscriptTiming(options, "db_tail", dbTailStarted)
	if err != nil {
		return Result{}, err
	}
	start := end
	if len(rows) > 0 {
		start = rows[0].sequence
	}
	expected := windowSize
	if available := int(end - 1); available < expected {
		expected = available
	}
	complete := len(rows) == expected
	for index, row := range rows {
		if row.sequence != end-int64(len(rows))+int64(index) {
			complete = false
		}
	}
	if !complete {
		return errorResult(409, "thread history is incomplete", "history_incomplete"), nil
	}
	contextRow, err := service.contextBefore(ctx, storeID, threadID, generation, start)
	if err != nil {
		return Result{}, err
	}
	provenanceRows := append([]transcriptRow(nil), rows...)
	if contextRow != nil {
		provenanceRows = append([]transcriptRow{*contextRow}, provenanceRows...)
	}
	recordedAt, err := service.restoreImportedTimestamps(ctx, storeID, threadID, provenanceRows)
	if err != nil {
		return Result{}, err
	}
	provenanceOffset := 0
	if contextRow != nil {
		contextRow.payload = provenanceRows[0].payload
		provenanceOffset = 1
	}
	for index := range rows {
		rows[index].payload = provenanceRows[provenanceOffset+index].payload
	}
	projectionOptions := ProjectionOptions{ItemOffset: start - 1, Fragments: true, RecordedAt: recordedAt}
	if contextRow != nil {
		context := contextRow.payload
		projectionOptions.InitialTurnID = stringValue(object(context["payload"])["turn_id"])
		projectionOptions.InitialTurnStartedAt = turnStartedAt(context)
		projectionOptions.InitialTurnStartedApproximate = projectionOptions.InitialTurnStartedAt == ""
		if projectionOptions.InitialTurnStartedAt == "" {
			projectionOptions.InitialTurnStartedAt = recordTimestamp(context)
		}
		if projectionOptions.InitialTurnStartedAt == "" {
			projectionOptions.InitialTurnStartedAt = recordedAt[contextRow.sequence]
		}
	}
	items := make([]map[string]any, len(rows))
	for index := range rows {
		items[index] = rows[index].payload
	}
	projectionStarted := time.Now()
	projected := ProjectCodexTranscript(items, projectionOptions)
	needsCompletion := false
	if end <= snapshotCount {
		for _, item := range projected {
			if stringValue(item["kind"]) == "assistant" && item["turnCompletedAt"] == nil && item["turnElapsedMs"] == nil {
				needsCompletion = true
			}
		}
	}
	if needsCompletion {
		after := end - 1
		for after < snapshotCount {
			var sequenceText string
			var raw []byte
			err := service.pool.QueryRow(ctx, `SELECT item_seq::text,payload FROM codex_thread_events WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq>$4 AND item_seq<=$5 AND payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(task_complete|turn_complete|turn_aborted)"' ORDER BY item_seq LIMIT 1`, storeID, threadID, generation, after, snapshotCount).Scan(&sequenceText, &raw)
			if errors.Is(err, pgx.ErrNoRows) {
				break
			}
			if err != nil {
				return Result{}, err
			}
			sequence, parseErr := postgresTextInt(sequenceText)
			if parseErr != nil {
				return Result{}, parseErr
			}
			record, decodeErr := decodeObject(raw)
			if decodeErr != nil {
				return Result{}, decodeErr
			}
			if stringValue(record["type"]) == "event_msg" && includes(activityEnds, stringValue(object(record["payload"])["type"])) {
				if recordTimestamp(record) == "" {
					completionRows := []transcriptRow{{sequence, record, nil}}
					_, _ = service.restoreImportedTimestamps(ctx, storeID, threadID, completionRows)
					record = completionRows[0].payload
				}
				projectionOptions.TimingRecords = []map[string]any{record}
				projected = ProjectCodexTranscript(items, projectionOptions)
				break
			}
			after = sequence
		}
	}
	addTranscriptTiming(options, "projection", projectionStarted)
	pageCursor := fmt.Sprintf("t2:%d:%d:%d", generation, end, snapshotCount)
	startIndex := len(projected) - limit
	if startIndex < 0 {
		startIndex = 0
	}
	loaded := options.ToolDetails == nil || *options.ToolDetails
	trace := transcriptToolDetails(projected[startIndex:], pageCursor, limit, loaded)
	before := start
	if len(projected) > limit && len(trace) > 0 {
		sequences := make([]int64, 0, len(trace))
		for _, item := range trace {
			if sequence, ok := safeInteger(item["sourceItemSeq"]); ok {
				sequences = append(sequences, sequence)
			}
		}
		if len(sequences) > 0 {
			sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
			before = sequences[0]
		}
	}
	var next any
	if before > 1 {
		next = fmt.Sprintf("t2:%d:%d:%d", generation, before, snapshotCount)
	}
	return Result{Status: 200, Body: map[string]any{"storeId": storeID, "threadId": threadID, "generation": generation, "itemCount": itemCount, "storeVersion": storeVersion, "trace": trace, "projectionVersion": 2, "totalTraceItems": nil, "pageCursor": pageCursor, "nextCursor": next}}, nil
}

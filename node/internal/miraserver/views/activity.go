package views

import (
	"context"
	"math"
	"time"
)

var activityStarts = []string{"task_started", "turn_started"}
var activityEnds = []string{"task_complete", "turn_complete", "turn_aborted"}

type ActivityRow struct {
	ItemSeq   *int64
	Payload   map[string]any
	CreatedAt *time.Time
}

func eventTime(value any) any {
	var parsed time.Time
	switch value := value.(type) {
	case time.Time:
		parsed = value
	case *time.Time:
		if value == nil {
			return nil
		}
		parsed = *value
	case string:
		var err error
		parsed, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil
		}
	default:
		numeric, ok := number(value)
		if !ok {
			return nil
		}
		seconds, fraction := math.Modf(numeric)
		parsed = time.Unix(int64(seconds), int64(fraction*float64(time.Second)))
	}
	return formatTime(parsed)
}

func terminalError(payload map[string]any) bool {
	if stringValue(payload["type"]) != "error" {
		return false
	}
	if retry, _ := first(payload["will_retry"], payload["willRetry"]).(bool); retry {
		return false
	}
	info := payload["codex_error_info"]
	typeName, ok := info.(string)
	if !ok {
		for key := range object(info) {
			typeName = key
			break
		}
	}
	typeName = normalizedType(typeName)
	return typeName != "threadrollbackfailed" && typeName != "activeturnnotsteerable"
}

// ProjectThreadActivity projects newest-first lifecycle candidates.
func ProjectThreadActivity(rows []ActivityRow, exhausted bool) map[string]any {
	markers := make([]ActivityRow, 0, len(rows))
	for _, row := range rows {
		if stringValue(row.Payload["type"]) != "event_msg" {
			continue
		}
		payload := object(row.Payload["payload"])
		if includes(activityStarts, stringValue(payload["type"])) || includes(activityEnds, stringValue(payload["type"])) || terminalError(payload) {
			markers = append(markers, row)
		}
	}
	startIndex := -1
	for index, row := range markers {
		if includes(activityStarts, stringValue(object(row.Payload["payload"])["type"])) {
			startIndex = index
			break
		}
	}
	if startIndex < 0 && !exhausted {
		return map[string]any{"state": "unknown", "turnId": nil, "reason": "history"}
	}
	var start *ActivityRow
	var turnID string
	if startIndex >= 0 {
		start = &markers[startIndex]
		turnID = stringValue(object(start.Payload["payload"])["turn_id"])
	}
	search := markers
	if start != nil {
		search = markers[:startIndex]
	}
	var end, failure *ActivityRow
	for index := range search {
		payload := object(search[index].Payload["payload"])
		candidateTurn := stringValue(payload["turn_id"])
		if end == nil && includes(activityEnds, stringValue(payload["type"])) && (turnID == "" || candidateTurn == "" || candidateTurn == turnID) {
			end = &search[index]
		}
		if start != nil && failure == nil && terminalError(payload) && (candidateTurn == "" || candidateTurn == turnID) {
			failure = &search[index]
		}
	}
	marker := start
	if failure != nil {
		marker = failure
	}
	if end != nil {
		marker = end
	}
	if marker == nil {
		if exhausted {
			return map[string]any{"state": "idle", "turnId": nil, "reason": nil}
		}
		return map[string]any{"state": "unknown", "turnId": nil, "reason": "history"}
	}
	payload := object(marker.Payload["payload"])
	state := "running"
	if end != nil {
		if stringValue(payload["type"]) == "turn_aborted" {
			state = "interrupted"
		} else if payload["error"] != nil || failure != nil {
			state = "failed"
		} else {
			state = "idle"
		}
	} else if failure != nil {
		state = "failed"
	}
	var startedItemSeq any
	var startedAt any
	if start != nil {
		if start.ItemSeq != nil {
			startedItemSeq = *start.ItemSeq
		}
		startPayload := object(start.Payload["payload"])
		startedAt = first(eventTime(startPayload["started_at"]), eventTime(start.Payload["timestamp"]), eventTime(start.CreatedAt))
	}
	if turnID == "" {
		turnID = stringValue(payload["turn_id"])
	}
	var turn any
	if turnID != "" {
		turn = turnID
	}
	return map[string]any{
		"state": state, "turnId": turn, "startedItemSeq": startedItemSeq, "startedAt": startedAt,
		"updatedAt": first(eventTime(first(payload["completed_at"], payload["started_at"])), eventTime(marker.Payload["timestamp"]), eventTime(marker.CreatedAt)),
	}
}

func (service *Service) addActivities(ctx context.Context, storeID string, threads []Thread) ([]Thread, error) {
	if len(threads) == 0 {
		return threads, nil
	}
	ids, generations, counts := threadCoordinates(threads)
	rows, err := service.pool.Query(ctx, `
      SELECT selected.thread_id, events.item_seq::text, events.payload, events.created_at
      FROM unnest($2::text[], $3::bigint[], $4::bigint[]) AS selected(thread_id, generation, item_count)
      LEFT JOIN LATERAL (
        SELECT item_seq, payload, created_at FROM codex_thread_events
        WHERE store_id=$1 AND thread_id=selected.thread_id AND generation=selected.generation
          AND item_seq<=selected.item_count
          AND payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(task_started|turn_started|task_complete|turn_complete|turn_aborted|error)"'
        ORDER BY item_seq DESC LIMIT 32
      ) events ON TRUE
      ORDER BY selected.thread_id, events.item_seq DESC`, storeID, ids, generations, counts)
	if err != nil {
		return nil, err
	}
	byThread := map[string][]ActivityRow{}
	for rows.Next() {
		var threadID string
		var sequence *string
		var raw []byte
		var createdAt *time.Time
		if err := rows.Scan(&threadID, &sequence, &raw, &createdAt); err != nil {
			rows.Close()
			return nil, err
		}
		if sequence == nil {
			continue
		}
		itemSeq, err := postgresTextInt(*sequence)
		if err != nil {
			rows.Close()
			return nil, err
		}
		payload, err := decodeObject(raw)
		if err != nil {
			rows.Close()
			return nil, err
		}
		byThread[threadID] = append(byThread[threadID], ActivityRow{ItemSeq: &itemSeq, Payload: payload, CreatedAt: createdAt})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for index := range threads {
		candidates := byThread[threads[index].ThreadID]
		threads[index].Activity = ProjectThreadActivity(candidates, len(candidates) < 32)
		threads[index].Activity["generation"] = threads[index].Generation
		threads[index].Activity["itemCount"] = threads[index].ItemCount
	}

	nodeIDs := []string{}
	for _, thread := range threads {
		if thread.RuntimeNodeID != nil {
			nodeIDs = appendUnique(nodeIDs, *thread.RuntimeNodeID)
		} else if thread.SourceNodeID != nil {
			nodeIDs = appendUnique(nodeIDs, *thread.SourceNodeID)
		}
	}
	type liveNode struct {
		approval string
		lastSeen time.Time
		channel  map[string]any
		reported map[string]any
	}
	byNode := map[string]liveNode{}
	if len(nodeIDs) > 0 {
		nodeRows, err := service.pool.Query(ctx, `SELECT node_id::text, approval_status, last_seen_at,
      channel_status, reported_app_server FROM codex_nodes WHERE node_id=ANY($1::uuid[])`, nodeIDs)
		if err != nil {
			return nil, err
		}
		for nodeRows.Next() {
			var id, approval string
			var lastSeen time.Time
			var channelRaw, reportedRaw []byte
			if err := nodeRows.Scan(&id, &approval, &lastSeen, &channelRaw, &reportedRaw); err != nil {
				nodeRows.Close()
				return nil, err
			}
			channel, _ := decodeObject(channelRaw)
			reported, _ := decodeObject(reportedRaw)
			byNode[id] = liveNode{approval: approval, lastSeen: lastSeen, channel: channel, reported: reported}
		}
		if err := nodeRows.Err(); err != nil {
			nodeRows.Close()
			return nil, err
		}
		nodeRows.Close()
	}
	now := service.now()
	for index := range threads {
		thread := &threads[index]
		if thread.Activity["state"] != "running" {
			continue
		}
		nodeID := thread.SourceNodeID
		if thread.RuntimeNodeID != nil {
			nodeID = thread.RuntimeNodeID
		}
		reason := ""
		startedSeq, hasStartedSeq := safeInteger(thread.Activity["startedItemSeq"])
		importedHistory := thread.ImportedAt != nil && thread.ImportedItemCount != nil && *thread.ImportedItemCount >= 0 && hasStartedSeq && startedSeq <= *thread.ImportedItemCount
		beforeBySecond := func(boundary *string) bool {
			started, startOK := parseISO(stringValue(thread.Activity["startedAt"]))
			end, endOK := parseISOPointer(boundary)
			return startOK && endOK && started.Add(time.Second).Before(end)
		}
		legacyImported := thread.ImportedAt != nil && thread.ImportedItemCount == nil && beforeBySecond(thread.ImportedAt)
		copied := thread.ImportedAt == nil && thread.CreatedAt != nil && beforeBySecond(thread.CreatedAt)
		if importedHistory || legacyImported || copied {
			reason = "history"
		} else if nodeID == nil {
			reason = "unbound"
		} else if node, ok := byNode[*nodeID]; !ok {
			reason = "unbound"
		} else if connected, _ := node.channel["connected"].(bool); node.approval != "approved" || !connected || now.Sub(node.lastSeen) >= 15*time.Second {
			reason = "offline"
		} else if thread.RuntimeNodeID != nil {
			started, startOK := parseISO(stringValue(thread.Activity["startedAt"]))
			runtimeStarted, runtimeOK := parseISO(stringValue(node.reported["startedAt"]))
			if stringValue(node.reported["status"]) != "running" || (startOK && runtimeOK && runtimeStarted.After(started)) {
				reason = "runtime"
			}
		}
		if reason != "" {
			thread.Activity["state"] = "unknown"
			thread.Activity["reason"] = reason
		}
	}
	return threads, nil
}

func threadCoordinates(threads []Thread) ([]string, []int64, []int64) {
	ids := make([]string, len(threads))
	generations := make([]int64, len(threads))
	counts := make([]int64, len(threads))
	for index, thread := range threads {
		ids[index], generations[index], counts[index] = thread.ThreadID, thread.Generation, thread.ItemCount
	}
	return ids, generations, counts
}

func parseISO(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil
}

func parseISOPointer(value *string) (time.Time, bool) {
	if value == nil {
		return time.Time{}, false
	}
	return parseISO(*value)
}

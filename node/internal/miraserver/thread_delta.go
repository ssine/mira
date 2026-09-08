package miraserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var operationIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type expectedState struct {
	Exists bool
	Value  any
}

type requestedStateChange struct {
	Path           []string
	Mode           string
	Value          any
	Expected       expectedState
	ConflictPolicy string
}

type requestedHistoryChange struct {
	ThreadID           string
	Mode               string
	ExpectedGeneration int64
	ExpectedItemCount  int64
	Items              []any
	UploadID           string
}

type pathValue struct {
	Exists bool
	Value  any
}

func readPath(state map[string]any, path []string) pathValue {
	var parent any = state
	for _, key := range path {
		object, ok := parent.(map[string]any)
		if !ok {
			return pathValue{}
		}
		value, exists := object[key]
		if !exists {
			return pathValue{}
		}
		parent = value
	}
	return pathValue{Exists: true, Value: parent}
}

func writePath(state map[string]any, change requestedStateChange) {
	parent := state
	for _, key := range change.Path[:len(change.Path)-1] {
		next, ok := parent[key].(map[string]any)
		if !ok {
			if change.Mode == "remove" {
				return
			}
			next = map[string]any{}
			parent[key] = next
		}
		parent = next
	}
	key := change.Path[len(change.Path)-1]
	if change.Mode == "remove" {
		delete(parent, key)
	} else {
		parent[key] = change.Value
	}
}

func requiredConflictPolicy(path []string) string {
	if len(path) >= 3 && path[0] == "metadata_updates" && (path[2] == "updated_at" || path[2] == "advance_recency_at" || path[2] == "token_usage") {
		return "lastWriteWins"
	}
	return "compareAndSwap"
}

func samePathValue(actual pathValue, expected expectedState) bool {
	return actual.Exists == expected.Exists && (!actual.Exists || jsonEqual(actual.Value, expected.Value))
}

func parseStateChanges(value any) ([]requestedStateChange, error) {
	items, ok := value.([]any)
	if !ok || len(items) > 100000 {
		return nil, fmt.Errorf("stateChanges must be an array")
	}
	result := make([]requestedStateChange, 0, len(items))
	paths := map[string]bool{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid state change")
		}
		pathRaw, ok := item["path"].([]any)
		if !ok || len(pathRaw) == 0 || len(pathRaw) > 16 {
			return nil, fmt.Errorf("invalid state change")
		}
		path := make([]string, len(pathRaw))
		for index, segment := range pathRaw {
			value, ok := segment.(string)
			if !ok || value == "" || len(value) > 256 {
				return nil, fmt.Errorf("invalid state change")
			}
			path[index] = value
		}
		if path[0] == "histories" {
			return nil, fmt.Errorf("invalid state change")
		}
		mode, modeOK := item["mode"].(string)
		policy, policyOK := item["conflictPolicy"].(string)
		if !modeOK || (mode != "set" && mode != "remove") || !policyOK || policy != requiredConflictPolicy(path) {
			return nil, fmt.Errorf("invalid state change")
		}
		value, hasValue := item["value"]
		if mode == "set" && !hasValue {
			return nil, fmt.Errorf("invalid state change")
		}
		expectedRaw, ok := item["expected"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid state change")
		}
		exists, ok := expectedRaw["exists"].(bool)
		if !ok {
			return nil, fmt.Errorf("invalid state change")
		}
		expectedValue, hasExpectedValue := expectedRaw["value"]
		if exists && !hasExpectedValue {
			return nil, fmt.Errorf("invalid state change")
		}
		keyRaw, _ := json.Marshal(path)
		key := string(keyRaw)
		if paths[key] {
			return nil, fmt.Errorf("invalid state change")
		}
		paths[key] = true
		result = append(result, requestedStateChange{Path: path, Mode: mode, Value: value, Expected: expectedState{Exists: exists, Value: expectedValue}, ConflictPolicy: policy})
	}
	return result, nil
}

func parseHistoryChanges(value any) ([]requestedHistoryChange, error) {
	items, ok := value.([]any)
	if !ok || len(items) > 10000 {
		return nil, fmt.Errorf("historyChanges must be an array")
	}
	result := make([]requestedHistoryChange, 0, len(items))
	ids := map[string]bool{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid history change")
		}
		threadID, idOK := item["threadId"].(string)
		mode, modeOK := item["mode"].(string)
		generation, genOK := integer(item["expectedGeneration"])
		count, countOK := integer(item["expectedItemCount"])
		if !idOK || threadID == "" || len(threadID) > 256 || ids[threadID] || !modeOK || (mode != "append" && mode != "replace" && mode != "delete") || !genOK || generation < 0 || !countOK || count < 0 {
			return nil, fmt.Errorf("invalid history change")
		}
		var values []any
		uploadID, _ := item["itemsUploadId"].(string)
		if _, hasItems := item["items"]; uploadID != "" && hasItems {
			return nil, fmt.Errorf("items and itemsUploadId are mutually exclusive")
		}
		if uploadID != "" && (!operationIDPattern.MatchString(uploadID) || mode == "delete") {
			return nil, fmt.Errorf("invalid history upload reference")
		}
		if mode != "delete" && uploadID == "" {
			var ok bool
			values, ok = item["items"].([]any)
			if !ok {
				return nil, fmt.Errorf("invalid history change")
			}
		}
		ids[threadID] = true
		result = append(result, requestedHistoryChange{ThreadID: threadID, Mode: mode, ExpectedGeneration: generation, ExpectedItemCount: count, Items: values, UploadID: uploadID})
	}
	return result, nil
}

func activeHistoryItems(ctx context.Context, query dbtx, storeID, threadID string, generation, version, after, limit int64) ([]any, error) {
	if limit == 0 {
		return []any{}, nil
	}
	rows, err := query.Query(ctx, `SELECT payload FROM codex_thread_events_versioned
      WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND store_event_seq<=$4 AND item_seq>$5
      ORDER BY item_seq ASC LIMIT $6`, storeID, threadID, generation, version, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		items = append(items, value)
	}
	return items, rows.Err()
}

type historyPlan struct {
	Changed                              bool
	Next                                 *historyEntry
	Appends                              []historyAppend
	Conflict                             map[string]any
	UploadID                             string
	UploadStart, UploadBase, UploadCount int64
}

func planHistoryDelta(ctx context.Context, query dbtx, storeID string, version int64, entry *historyEntry, change requestedHistoryChange) (historyPlan, error) {
	var generation, count int64
	if entry != nil {
		generation, count = entry.Generation, entry.ItemCount
	}
	appendCanRebase := change.Mode == "append" && change.ExpectedGeneration == generation && change.ExpectedItemCount <= count
	exact := change.ExpectedGeneration == generation && change.ExpectedItemCount == count
	if !appendCanRebase && !exact {
		return historyPlan{Conflict: map[string]any{"error": "thread generation conflict", "threadId": change.ThreadID, "currentGeneration": generation, "currentItemCount": count}}, nil
	}
	if change.Mode == "delete" {
		return historyPlan{Changed: entry != nil}, nil
	}
	if change.Mode == "append" {
		overlapLimit := int64(len(change.Items))
		if remaining := count - change.ExpectedItemCount; overlapLimit > remaining {
			overlapLimit = remaining
		}
		if overlapLimit < 0 {
			overlapLimit = 0
		}
		overlapping := []any{}
		var err error
		if entry != nil {
			overlapping, err = activeHistoryItems(ctx, query, storeID, change.ThreadID, generation, version, change.ExpectedItemCount, overlapLimit)
			if err != nil {
				return historyPlan{}, err
			}
		}
		if int64(len(overlapping)) != overlapLimit {
			return historyPlan{}, fmt.Errorf("canonical history %s expected %d overlapping items, found %d", change.ThreadID, overlapLimit, len(overlapping))
		}
		overlap := 0
		for overlap < len(overlapping) && jsonEqual(overlapping[overlap], change.Items[overlap]) {
			overlap++
		}
		remaining := change.Items[overlap:]
		nextGeneration := generation
		if entry == nil {
			nextGeneration = 1
		}
		next := historyEntry{Generation: nextGeneration, ItemCount: count + int64(len(remaining))}
		appends := make([]historyAppend, 0, len(remaining))
		for index, payload := range remaining {
			appends = append(appends, historyAppend{ThreadID: change.ThreadID, Generation: nextGeneration, ItemSeq: count + int64(index) + 1, Payload: payload})
		}
		return historyPlan{Changed: entry == nil || len(remaining) > 0, Next: &next, Appends: appends}, nil
	}
	var previous []any
	if entry != nil {
		var err error
		previous, err = activeHistoryItems(ctx, query, storeID, change.ThreadID, generation, version, 0, count)
		if err != nil {
			return historyPlan{}, err
		}
		if int64(len(previous)) != count {
			return historyPlan{}, fmt.Errorf("canonical history %s expected %d items, found %d", change.ThreadID, count, len(previous))
		}
	}
	if entry != nil && jsonEqual(previous, change.Items) {
		copy := *entry
		return historyPlan{Next: &copy}, nil
	}
	appendOnly := entry != nil && historyPrefix(previous, change.Items)
	nextGeneration, start := generation+1, 0
	if appendOnly {
		nextGeneration = generation
		if nextGeneration < 1 {
			nextGeneration = 1
		}
		start = len(previous)
	}
	next := historyEntry{Generation: nextGeneration, ItemCount: int64(len(change.Items))}
	appends := []historyAppend{}
	for index, payload := range change.Items[start:] {
		appends = append(appends, historyAppend{ThreadID: change.ThreadID, Generation: nextGeneration, ItemSeq: int64(start + index + 1), Payload: payload})
	}
	return historyPlan{Changed: true, Next: &next, Appends: appends}, nil
}

func CommitDelta(ctx context.Context, pool *pgxpool.Pool, storeID string, body map[string]any, headers http.Header) (operationResponse, error) {
	return commitDeltaWithIdentity(ctx, pool, storeID, body, headers, body)
}

type txBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

func commitDeltaWithIdentity(ctx context.Context, beginner txBeginner, storeID string, body map[string]any, headers http.Header, requestIdentity any) (operationResponse, error) {
	expectedVersion, ok := integer(body["expectedVersion"])
	if !ok || expectedVersion < 0 {
		return operationResponse{Status: 400, Body: map[string]any{"error": "expectedVersion must be a non-negative integer"}}, nil
	}
	stateChanges, err := parseStateChanges(body["stateChanges"])
	if err != nil {
		return operationResponse{Status: 400, Body: map[string]any{"error": err.Error()}}, nil
	}
	historyChanges, err := parseHistoryChanges(body["historyChanges"])
	if err != nil {
		return operationResponse{Status: 400, Body: map[string]any{"error": err.Error()}}, nil
	}
	operationID := headers.Get("X-Codex-Operation-Id")
	if !operationIDPattern.MatchString(operationID) {
		operationID, err = randomUUID()
		if err != nil {
			return operationResponse{}, err
		}
	}
	var codexVersion *string
	if value := headers.Get("X-Codex-Version"); value != "" {
		codexVersion = &value
	}
	known := map[string]bool{"created_threads": true, "metadata_updates": true, "names": true, "sections": true, "section_positions": true, "section_entered_at": true}
	scoped := true
	ids := []string{}
	for _, change := range stateChanges {
		if len(change.Path) < 2 || !known[change.Path[0]] {
			scoped = false
		}
		if len(change.Path) > 1 {
			ids = append(ids, change.Path[1])
		}
	}
	for _, change := range historyChanges {
		ids = append(ids, change.ThreadID)
	}
	ids = sortedUnique(ids)
	compactScope := headers.Get("X-Mira-Thread-Scope")
	if compactScope != "" {
		if !scoped {
			return operationResponse{Status: 400, Body: map[string]any{"error": "invalid thread-scoped commit"}}, nil
		}
		for _, id := range ids {
			if id != compactScope {
				return operationResponse{Status: 400, Body: map[string]any{"error": "invalid thread-scoped commit"}}, nil
			}
		}
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return operationResponse{}, err
	}
	defer tx.Rollback(ctx)
	duplicate, err := beginReceipt(ctx, tx, storeID, operationID, requestIdentity, codexVersion)
	if err != nil {
		return operationResponse{}, err
	}
	if duplicate != nil {
		if compactScope != "" {
			if manifest, ok := duplicate.Body["historyManifest"].(map[string]historyEntry); ok {
				filtered := map[string]historyEntry{}
				if entry, found := manifest[compactScope]; found {
					filtered[compactScope] = entry
				}
				duplicate.Body["historyManifest"] = filtered
			}
		}
		return *duplicate, nil
	}
	var lockIDs []string
	if scoped {
		lockIDs = ids
	}
	if err := lockScope(ctx, tx, storeID, lockIDs); err != nil {
		return operationResponse{}, err
	}
	writtenIDs := []string{}
	for _, change := range historyChanges {
		if change.Mode != "delete" {
			writtenIDs = append(writtenIDs, change.ThreadID)
		}
	}
	for _, change := range stateChanges {
		if change.Mode == "remove" {
			continue
		}
		if len(change.Path) > 1 {
			if change.Path[0] == "rollout_paths" {
				if id, ok := change.Value.(string); ok {
					writtenIDs = append(writtenIDs, id)
				}
			} else {
				writtenIDs = append(writtenIDs, change.Path[1])
			}
		} else if value, ok := change.Value.(map[string]any); ok {
			writtenIDs = append(writtenIDs, snapshotThreadIDs(map[string]any{change.Path[0]: value})...)
		}
	}
	if err := assertThreadsNotDeleted(ctx, tx, storeID, sortedUnique(writtenIDs)); err != nil {
		return operationResponse{}, err
	}
	head, err := currentHead(ctx, tx, storeID, lockIDs)
	if err != nil {
		return operationResponse{}, err
	}
	if expectedVersion > head.Version {
		return operationResponse{Status: 409, Body: map[string]any{"error": "expected version is ahead of store", "currentVersion": head.Version}}, nil
	}
	if expectedVersion < head.HistoryFloor {
		return operationResponse{Status: 409, Body: map[string]any{"error": "reload after storage migration", "code": "store_version_retired", "currentVersion": head.Version}}, nil
	}
	state, err := cloneObject(head.State)
	if err != nil {
		return operationResponse{}, err
	}
	for _, change := range stateChanges {
		actual := readPath(state, change.Path)
		desired := expectedState{Exists: change.Mode != "remove", Value: change.Value}
		if !samePathValue(actual, change.Expected) && !samePathValue(actual, desired) && change.ConflictPolicy != "lastWriteWins" {
			return operationResponse{Status: 409, Body: map[string]any{"error": "state path conflict", "path": change.Path, "currentExists": actual.Exists}}, nil
		}
		writePath(state, change)
	}
	manifest := make(map[string]historyEntry, len(head.HistoryManifest))
	for id, entry := range head.HistoryManifest {
		manifest[id] = entry
	}
	appends := []historyAppend{}
	uploaded := map[string]historyPlan{}
	var uploadedCount int64
	changed := false
	for _, change := range historyChanges {
		var entry *historyEntry
		if value, found := head.HistoryManifest[change.ThreadID]; found {
			copy := value
			entry = &copy
		}
		var plan historyPlan
		var err error
		if change.UploadID != "" {
			plan, err = planUploadedHistory(ctx, tx, storeID, entry, change)
			uploaded[change.ThreadID] = plan
			uploadedCount += plan.UploadCount
		} else {
			plan, err = planHistoryDelta(ctx, tx, storeID, head.Version, entry, change)
		}
		if err != nil {
			return operationResponse{}, err
		}
		if plan.Conflict != nil {
			return operationResponse{Status: 409, Body: plan.Conflict}, nil
		}
		if plan.Changed {
			changed = true
			if plan.Next == nil {
				delete(manifest, change.ThreadID)
			} else {
				manifest[change.ThreadID] = *plan.Next
			}
			appends = append(appends, plan.Appends...)
		}
	}
	if err := repairNewGenerations(ctx, tx, storeID, head.HistoryManifest, manifest, appends); err != nil {
		return operationResponse{}, err
	}
	affected, err := persistState(ctx, tx, storeID, operationID, head.State, state)
	if err != nil {
		return operationResponse{}, err
	}
	metadataAffected := map[string]bool{}
	for id := range affected {
		metadataAffected[id] = true
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

	for threadID, plan := range uploaded {
		generation := manifest[threadID].Generation
		if _, err := tx.Exec(ctx, `INSERT INTO codex_thread_events(store_id,thread_id,generation,item_seq,operation_id,event_format_version,codex_version,payload,payload_sha256)
   SELECT store_id,$3,$4,item_seq-$5+$6,$7,$8,$9,payload,payload_sha256 FROM mira_history_upload_items
   WHERE store_id=$1 AND upload_id=$2 AND item_seq>$5 ORDER BY item_seq`, storeID, plan.UploadID, threadID, generation, plan.UploadStart, plan.UploadBase, operationID, eventFormatVersion, codexVersion); err != nil {
			return operationResponse{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE mira_history_uploads SET status='committed',updated_at=NOW() WHERE store_id=$1 AND upload_id=$2`, storeID, plan.UploadID); err != nil {
			return operationResponse{}, err
		}
	}
	if err := replaceProjections(ctx, tx, storeID, state, manifest, 1, affected, metadataAffected); err != nil {
		return operationResponse{}, err
	}
	noChange := !changed && jsonEqual(state, head.State)
	var noChangeVersion *int64
	if noChange {
		noChangeVersion = &head.Version
	}
	version, err := publishReceipt(ctx, tx, storeID, operationID, affected, int64(len(appends))+uploadedCount, noChangeVersion)
	if err != nil {
		return operationResponse{}, err
	}
	responseManifest := manifest
	if compactScope == "" {
		responseManifest, err = historicalManifest(ctx, tx, storeID, version, nil)
		if err != nil {
			return operationResponse{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return operationResponse{}, err
	}
	bodyOut := map[string]any{"version": version, "operationId": operationID, "rebased": version-boolInt64(!noChange) != expectedVersion, "historyManifest": responseManifest, "appendedItemCount": int64(len(appends)) + uploadedCount, "updatedAt": time.Now().UTC().Format(time.RFC3339Nano)}
	if noChange {
		bodyOut["noChange"] = true
	}
	return operationResponse{Status: 200, Body: bodyOut}, nil
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

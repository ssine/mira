package miraserver

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func NextForkTitle(source string, used []string) string {
	base := strings.TrimSpace(regexp.MustCompile(`[\x00-\x1f\x7f]`).ReplaceAllString(source, " "))
	base = regexp.MustCompile(` \([1-9][0-9]*\)$`).ReplaceAllString(base, "")
	if base == "" {
		base = "新会话"
	}
	usedSet := map[string]bool{}
	for _, title := range used {
		usedSet[title] = true
	}
	for number := 1; ; number++ {
		suffix := " (" + strconv.Itoa(number) + ")"
		// The released limit was JavaScript UTF-16 units. Limit runes here, then
		// trim further until the UTF-16 count fits without splitting a pair.
		candidate := base
		for utf16Length(candidate)+len(suffix) > 200 {
			_, size := utf8.DecodeLastRuneInString(candidate)
			if size == 0 {
				break
			}
			candidate = candidate[:len(candidate)-size]
		}
		candidate = strings.TrimRight(candidate, " ") + suffix
		if !usedSet[candidate] {
			return candidate
		}
	}
}

func utf16Length(value string) int {
	count := 0
	for _, r := range value {
		if r > 0xffff {
			count += 2
		} else {
			count++
		}
	}
	return count
}

func RenameThread(ctx context.Context, pool *pgxpool.Pool, storeID, threadID string, body map[string]any) (operationResponse, error) {
	name, _ := body["name"].(string)
	name = strings.TrimSpace(name)
	expectedName, hasExpected := body["expectedName"]
	expectedValid := hasExpected && (expectedName == nil || isString(expectedName))
	generation, genOK := integer(body["generation"])
	operationID, _ := body["operationId"].(string)
	if name == "" || utf16Length(name) > 200 || regexp.MustCompile(`[\x00-\x1f\x7f]`).MatchString(name) || !expectedValid || !genOK || generation < 1 || !uuidPattern.MatchString(operationID) {
		return operationResponse{Status: 400, Body: map[string]any{"error": "请填写 1–200 字的单行标题。", "code": "invalid_request"}}, nil
	}
	head, err := currentHead(ctx, pool, storeID, []string{threadID})
	if err != nil {
		return operationResponse{}, err
	}
	entry, found := head.HistoryManifest[threadID]
	if !found {
		return operationResponse{Status: 404, Body: map[string]any{"error": "会话不存在或已不可访问", "code": "not_found"}}, nil
	}
	names := object(head.State["names"])
	_, nameExists := names[threadID]
	expected := map[string]any{"exists": true, "value": expectedName}
	if expectedName == nil && !nameExists {
		expected = map[string]any{"exists": false}
	}
	requestBody := map[string]any{
		"expectedVersion": head.Version,
		"stateChanges":    []any{map[string]any{"path": []any{"names", threadID}, "mode": "set", "conflictPolicy": "compareAndSwap", "expected": expected, "value": name}},
		"historyChanges":  []any{map[string]any{"threadId": threadID, "mode": "append", "expectedGeneration": generation, "expectedItemCount": entry.ItemCount, "items": []any{}}},
	}
	identity := map[string]any{"action": "rename", "threadId": threadID}
	for key, value := range body {
		identity[key] = value
	}
	identity["name"] = name
	result, err := commitDeltaWithIdentity(ctx, pool, storeID, requestBody, http.Header{"X-Codex-Operation-Id": []string{operationID}, "X-Codex-Version": []string{"mira-web"}}, identity)
	if err == nil && result.Status == 409 {
		return operationResponse{Status: 409, Body: map[string]any{"error": "此会话已在其他窗口更新，请重新打开编辑后再保存。", "code": "thread_changed"}}, nil
	}
	return result, err
}

func isString(value any) bool { _, ok := value.(string); return ok }

func NameForkThread(ctx context.Context, pool *pgxpool.Pool, storeID, threadID string, body map[string]any) (operationResponse, error) {
	sourceID, _ := body["sourceThreadId"].(string)
	operationID, _ := body["operationId"].(string)
	generation, genOK := integer(body["generation"])
	expectedName, hasExpected := body["expectedName"]
	if !uuidPattern.MatchString(sourceID) || sourceID == threadID || !uuidPattern.MatchString(operationID) || !genOK || generation < 1 || !hasExpected || (expectedName != nil && !isString(expectedName)) {
		return operationResponse{Status: 400, Body: map[string]any{"error": "无效的分支标题参数", "code": "invalid_request"}}, nil
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return operationResponse{}, err
	}
	defer connection.Release()
	lockRaw, _ := json.Marshal([]string{"mira-fork-title", storeID})
	lock := string(lockRaw)
	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1,0))", lock); err != nil {
		return operationResponse{}, err
	}
	defer connection.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtextextended($1,0))", lock)
	var previous int
	err = connection.QueryRow(ctx, "SELECT 1 FROM codex_store_events WHERE store_id=$1 AND operation_id=$2", storeID, operationID).Scan(&previous)
	identity := map[string]any{"action": "fork-title", "threadId": threadID}
	for key, value := range body {
		identity[key] = value
	}
	if err == nil {
		return commitDeltaWithIdentity(ctx, connection, storeID, map[string]any{"expectedVersion": 0, "stateChanges": []any{}, "historyChanges": []any{}}, http.Header{"X-Codex-Operation-Id": []string{operationID}, "X-Codex-Version": []string{"mira-web"}}, identity)
	}
	if err != pgx.ErrNoRows {
		return operationResponse{}, err
	}
	head, err := currentHead(ctx, connection, storeID, []string{threadID})
	if err != nil {
		return operationResponse{}, err
	}
	entry, found := head.HistoryManifest[threadID]
	if !found {
		return operationResponse{Status: 404, Body: map[string]any{"error": "分支会话不存在或已删除", "code": "not_found"}}, nil
	}
	rows, err := connection.Query(ctx, "SELECT thread_id,COALESCE(NULLIF(state->>'name',''),title) AS title FROM codex_thread_projections WHERE store_id=$1", storeID)
	if err != nil {
		return operationResponse{}, err
	}
	defer rows.Close()
	titles := []string{}
	sourceTitle := ""
	sourceFound := false
	for rows.Next() {
		var id string
		var title *string
		if err := rows.Scan(&id, &title); err != nil {
			return operationResponse{}, err
		}
		value := ""
		if title != nil {
			value = *title
		}
		if id == sourceID {
			sourceTitle, sourceFound = value, true
		}
		if id != threadID {
			titles = append(titles, value)
		}
	}
	if err := rows.Err(); err != nil {
		return operationResponse{}, err
	}
	if !sourceFound {
		return operationResponse{Status: 404, Body: map[string]any{"error": "原会话不存在或已删除", "code": "not_found"}}, nil
	}
	name := NextForkTitle(sourceTitle, titles)
	names := object(head.State["names"])
	_, exists := names[threadID]
	expected := map[string]any{"exists": true, "value": expectedName}
	if expectedName == nil && !exists {
		expected = map[string]any{"exists": false}
	}
	requestBody := map[string]any{"expectedVersion": head.Version,
		"stateChanges":   []any{map[string]any{"path": []any{"names", threadID}, "mode": "set", "conflictPolicy": "compareAndSwap", "expected": expected, "value": name}},
		"historyChanges": []any{map[string]any{"threadId": threadID, "mode": "append", "expectedGeneration": generation, "expectedItemCount": entry.ItemCount, "items": []any{}}},
	}
	result, err := commitDeltaWithIdentity(ctx, connection, storeID, requestBody, http.Header{"X-Codex-Operation-Id": []string{operationID}, "X-Codex-Version": []string{"mira-web"}}, identity)
	if err == nil && result.Status == 409 {
		return operationResponse{Status: 409, Body: map[string]any{"error": "分支标题已被修改，原标题已保留。", "code": "thread_changed"}}, nil
	}
	return result, err
}

func ManageThread(ctx context.Context, pool *pgxpool.Pool, storeID, threadID, action string, body map[string]any) (operationResponse, error) {
	generation, genOK := integer(body["generation"])
	operationID, _ := body["operationId"].(string)
	itemCount, countOK := integer(body["itemCount"])
	if (action != "archive" && action != "restore" && action != "delete") || !genOK || generation < 1 || !uuidPattern.MatchString(operationID) || (action == "delete" && (!countOK || itemCount < 0)) {
		return operationResponse{Status: 400, Body: map[string]any{"error": "无效的会话操作参数", "code": "invalid_request"}}, nil
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return operationResponse{}, err
	}
	defer connection.Release()
	tx, err := connection.Begin(ctx)
	if err != nil {
		return operationResponse{}, err
	}
	defer tx.Rollback(ctx)
	// Use transaction-local helpers after acquisition so all locks and writes
	// remain on the same backend session.
	replayTx := func() (*operationResponse, error) {
		var oldThread, oldAction string
		var oldGeneration int64
		var oldCount *int64
		var phase *string
		err := tx.QueryRow(ctx, `SELECT actions.thread_id,actions.action,actions.generation,actions.item_count,erasures.phase
          FROM mira_thread_actions actions LEFT JOIN mira_thread_erasures erasures USING(store_id,thread_id)
          WHERE actions.store_id=$1 AND actions.operation_id=$2`, storeID, operationID).Scan(&oldThread, &oldAction, &oldGeneration, &oldCount, &phase)
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if oldThread == threadID && oldAction == action && oldGeneration == generation && (action != "delete" || (oldCount != nil && *oldCount == itemCount)) {
			body := map[string]any{"threadId": threadID, "action": action, "duplicate": true}
			if action == "delete" {
				body["cleanupPending"] = phase != nil && *phase != "complete"
			}
			result := operationResponse{Status: 200, Body: body}
			return &result, nil
		}
		result := operationResponse{Status: 409, Body: map[string]any{"error": "操作标识已被其他操作使用", "code": "operation_conflict"}}
		return &result, nil
	}
	if action == "delete" {
		identity := map[string]any{"threadId": threadID, "action": action}
		for key, value := range body {
			identity[key] = value
		}
		duplicate, err := beginReceipt(ctx, tx, storeID, operationID, identity, stringPointer("mira-web"))
		if err != nil {
			return operationResponse{}, err
		}
		if duplicate != nil {
			if duplicate.Status == 200 {
				if result, err := replayTx(); err != nil {
					return operationResponse{}, err
				} else if result != nil {
					return *result, nil
				}
			}
			return *duplicate, nil
		}
	}
	if err := lockScope(ctx, tx, storeID, []string{threadID}); err != nil {
		return operationResponse{}, err
	}
	if result, err := replayTx(); err != nil {
		return operationResponse{}, err
	} else if result != nil {
		return *result, nil
	}
	head, err := currentHead(ctx, tx, storeID, []string{threadID})
	if err != nil {
		return operationResponse{}, err
	}
	entry, found := head.HistoryManifest[threadID]
	if !found {
		return operationResponse{Status: 404, Body: map[string]any{"error": "会话不存在或已删除", "code": "not_found"}}, nil
	}
	if entry.Generation != generation || (action == "delete" && entry.ItemCount != itemCount) {
		return operationResponse{Status: 409, Body: map[string]any{"error": "会话内容已变化，请等当前运行结束后重新操作。", "code": "thread_changed"}}, nil
	}
	var actionSeq int64
	var count any
	if action == "delete" {
		count = itemCount
	}
	if err := tx.QueryRow(ctx, `INSERT INTO mira_thread_actions(store_id,thread_id,action,operation_id,generation,item_count)
      VALUES($1,$2,$3,$4,$5,$6) RETURNING action_seq`, storeID, threadID, action, operationID, generation, count).Scan(&actionSeq); err != nil {
		return operationResponse{}, err
	}
	if action == "delete" {
		for _, query := range []string{
			"DELETE FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2",
			"DELETE FROM codex_store_state_entries WHERE store_id=$1 AND thread_id=$2",
			"DELETE FROM mira_codex_thread_runtimes WHERE store_id=$1 AND thread_id=$2",
			"DELETE FROM mira_thread_read_positions WHERE store_id=$1 AND thread_id=$2",
			`UPDATE mira_appserver_thread_start_requests SET response='{"deleted":true}'::jsonb WHERE store_id=$1 AND thread_id=$2`,
		} {
			if _, err := tx.Exec(ctx, query, storeID, threadID); err != nil {
				return operationResponse{}, err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO mira_thread_erasures(store_id,thread_id,action_seq,through_event_seq) VALUES($1,$2,$3,$4)`, storeID, threadID, actionSeq, head.Version); err != nil {
			return operationResponse{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO codex_thread_revisions(store_id,thread_id,operation_id,generation,item_count,active) VALUES($1,$2,$3,$4,$5,false)`, storeID, threadID, operationID, generation, itemCount); err != nil {
			return operationResponse{}, err
		}
		if _, err := publishReceipt(ctx, tx, storeID, operationID, map[string]bool{}, 0, nil); err != nil {
			return operationResponse{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return operationResponse{}, err
	}
	resultBody := map[string]any{"threadId": threadID, "action": action}
	if action == "delete" {
		resultBody["cleanupPending"] = true
	}
	return operationResponse{Status: 200, Body: resultBody}, nil
}

func stringPointer(value string) *string { return &value }

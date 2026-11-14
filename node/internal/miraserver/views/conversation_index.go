package views

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/coldstore"
)

// ConversationIndexVersion identifies the record slimming and message
// extraction rules. Changing them requires a new version: every thread is then
// re-indexed from canonical history, which remains the only source of truth.
const ConversationIndexVersion = 1

const (
	// Bodies only appear in expanded tool details, which still read canonical
	// history. A short prefix keeps "has output" and empty-body semantics.
	indexToolBodyPreview = 64
	// Tool inputs feed activity summaries such as apply_patch file statistics.
	indexToolInputLimit = 256 * 1024
	indexRecordLimit    = 1024 * 1024
	indexMessageLimit   = 256 * 1024
	indexReadChunk      = 64
	indexWriteBatch     = 512
)

var indexImagePlaceholder = "data:image/png;base64,AA=="

type codexIndexCarry struct {
	LastInputTokens int64 `json:"lastInputTokens,omitempty"`
}

type indexedMessage struct {
	role string
	body string
}

type indexedRecord struct {
	sequence   int64
	recordedAt *time.Time
	kind       string
	record     []byte
	message    *indexedMessage
}

// recordKind classifies a slim record ("type" or "type:payload.type") so
// incremental outlines can find turn boundaries without parsing JSON in SQL.
func recordKind(record map[string]any) string {
	kind := stringValue(record["type"])
	if inner := stringValue(object(record["payload"])["type"]); inner != "" {
		kind += ":" + inner
	}
	return kind
}

// truncateStrings bounds every string inside value without changing its shape.
func truncateStrings(value any, limit int) any {
	switch typed := value.(type) {
	case string:
		if len(typed) <= limit {
			return typed
		}
		end := limit
		for end > 0 && end < len(typed) && typed[end]&0xC0 == 0x80 {
			end--
		}
		return typed[:end] + "…"
	case []any:
		result := make([]any, len(typed))
		for index, entry := range typed {
			result[index] = truncateStrings(entry, limit)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, entry := range typed {
			result[key] = truncateStrings(entry, limit)
		}
		return result
	}
	return value
}

// slimContent keeps typed images (with a placeholder payload; the image route
// reads the canonical record) and bounds text parts.
func slimContent(value any, textLimit int) any {
	parts := array(value)
	if parts == nil {
		if text, ok := value.(string); ok {
			return truncateStrings(text, textLimit)
		}
		if value == nil {
			return nil
		}
		return truncateStrings(value, textLimit)
	}
	result := make([]any, len(parts))
	for index, part := range parts {
		item := object(part)
		if item == nil {
			result[index] = truncateStrings(part, textLimit)
			continue
		}
		copy := map[string]any{}
		for key, entry := range item {
			if key == "image_url" && imageDataURL(entry) != "" {
				copy[key] = indexImagePlaceholder
			} else {
				copy[key] = truncateStrings(entry, textLimit)
			}
		}
		result[index] = copy
	}
	return result
}

var indexItemProtected = map[string]bool{"type": true, "id": true, "status": true, "exitCode": true, "exit_code": true,
	"durationMs": true, "duration_ms": true, "duration": true, "command": true, "cwd": true, "commandActions": true,
	"parsed_cmd": true, "changes": true, "tool": true, "name": true, "server": true, "namespace": true, "action": true,
	"success": true, "path": true, "phase": true}

func slimMaterializedItem(item map[string]any) map[string]any {
	if item == nil {
		return nil
	}
	switch normalizedType(item["type"]) {
	case "usermessage", "agentmessage", "reasoning", "plan", "contextcompaction":
		return truncateStrings(item, indexRecordLimit).(map[string]any)
	}
	result := make(map[string]any, len(item))
	for key, entry := range item {
		if indexItemProtected[key] {
			result[key] = truncateStrings(entry, indexToolInputLimit)
		} else {
			result[key] = truncateStrings(entry, indexToolBodyPreview)
		}
	}
	return result
}

func withoutKeys(value map[string]any, keys ...string) map[string]any {
	result := cloneMap(value)
	for _, key := range keys {
		delete(result, key)
	}
	return result
}

func messageBody(text string) string {
	text = strings.ReplaceAll(text, "\x00", "")
	if len(text) > indexMessageLimit {
		text = truncateStrings(text, indexMessageLimit).(string)
	}
	return text
}

// slimCodexRecord keeps exactly what ProjectCodexTranscript renders without
// tool details, plus lifecycle/tool timing for the timeline. It returns nil for
// records that never render (token counters, session metadata, world state).
func slimCodexRecord(record map[string]any, carry *codexIndexCarry) (map[string]any, *indexedMessage) {
	recordType := stringValue(record["type"])
	payload := object(record["payload"])
	result := map[string]any{"type": recordType}
	if timestamp, ok := record["timestamp"]; ok {
		result["timestamp"] = timestamp
	}
	switch recordType {
	case "turn_context":
		slim := map[string]any{}
		for _, key := range []string{"turn_id", "cwd"} {
			if value, ok := payload[key]; ok {
				slim[key] = value
			}
		}
		result["payload"] = slim
		return result, nil
	case "compacted":
		slim := map[string]any{}
		if message, ok := payload["message"]; ok {
			slim["message"] = truncateStrings(message, indexRecordLimit)
		}
		if usage, ok := payload["latest_token_usage_record"]; ok {
			slim["latest_token_usage_record"] = usage
		}
		result["payload"] = slim
		if usage, ok := record["latest_token_usage_record"]; ok {
			result["latest_token_usage_record"] = usage
		} else if slim["latest_token_usage_record"] == nil && carry.LastInputTokens > 0 {
			// The projector reads the preceding token counter, which the index omits.
			result["latest_token_usage_record"] = map[string]any{"usage": map[string]any{"input_tokens": carry.LastInputTokens}}
		}
		return result, nil
	case "event_msg":
		kind := stringValue(payload["type"])
		switch {
		case includes(activityStarts, kind) || includes(activityEnds, kind):
			result["payload"] = truncateStrings(withoutKeys(payload, "last_agent_message"), 4096)
			return result, nil
		case kind == "user_message" || kind == "agent_message" || kind == "agent_reasoning":
			slim := withoutKeys(payload, "images")
			result["payload"] = truncateStrings(slim, indexRecordLimit)
			text := stringValue(first(payload["message"], payload["text"]))
			if text == "" && kind != "agent_reasoning" {
				text = valueText(payload)
			}
			switch kind {
			case "user_message":
				return result, &indexedMessage{"user", messageBody(text)}
			case "agent_message":
				return result, &indexedMessage{"assistant", messageBody(text)}
			}
			return result, nil
		case kind == "item_completed":
			slim := withoutKeys(payload, "item")
			slim["item"] = slimMaterializedItem(object(payload["item"]))
			result["payload"] = truncateStrings(slim, indexRecordLimit)
			return result, nil
		case kind == "view_image_tool_call":
			result["payload"] = truncateStrings(payload, indexToolInputLimit)
			return result, nil
		case kind == "token_count":
			usage := object(object(payload["info"])["last_token_usage"])
			input, _ := safeInteger(usage["input_tokens"])
			total, _ := safeInteger(usage["total_tokens"])
			if input > 0 {
				carry.LastInputTokens = input
				return nil, nil
			}
			if total > 0 {
				result["payload"] = map[string]any{"type": kind, "info": map[string]any{"last_token_usage": map[string]any{"input_tokens": input, "total_tokens": total}}}
				return result, nil
			}
		}
		return nil, nil
	case "response_item":
		kind := stringValue(payload["type"])
		switch kind {
		case "message":
			if !visibleResponseMessage(payload) {
				return nil, nil
			}
			slim := cloneMap(payload)
			slim["content"] = slimContent(payload["content"], indexRecordLimit)
			result["payload"] = slim
			return result, nil
		case "reasoning":
			// reasoningParts renders the first non-empty of these fields only;
			// later raw reasoning text never reaches the transcript.
			slim := withoutKeys(payload, "encrypted_content", "summary", "summary_text", "text", "content")
			for _, key := range []string{"summary", "summary_text", "text", "content"} {
				value, ok := payload[key]
				if !ok {
					continue
				}
				slim[key] = value
				if reasoningParts(map[string]any{key: value}) != nil {
					break
				}
			}
			result["payload"] = truncateStrings(slim, indexRecordLimit)
			return result, nil
		case "custom_tool_call", "function_call", "local_shell_call", "tool_search_call", "web_search_call":
			// Only shell commands and apply_patch inputs shape the collapsed
			// activity summary (ResponseToolView); other inputs are detail bodies.
			name := stringValue(first(payload["name"], payload["tool"]))
			if index := strings.LastIndex(name, "."); index >= 0 {
				name = name[index+1:]
			}
			if kind == "local_shell_call" || includes([]string{"exec_command", "shell_command", "shell", "local_shell", "apply_patch"}, name) {
				result["payload"] = truncateStrings(payload, indexToolInputLimit)
			} else {
				slim := cloneMap(payload)
				for _, key := range []string{"arguments", "input"} {
					if value, ok := payload[key]; ok {
						slim[key] = truncateStrings(value, indexToolBodyPreview)
					}
				}
				result["payload"] = truncateStrings(slim, indexToolInputLimit)
			}
			return result, nil
		case "custom_tool_call_output", "function_call_output", "tool_search_output":
			slim := cloneMap(payload)
			for _, key := range []string{"output", "result"} {
				if value, ok := payload[key]; ok {
					if text, isText := value.(string); isText {
						// JSON-encoded content arrays can still carry typed images.
						if parsed := array(parseJSONString(text)); parsed != nil {
							slim[key] = slimContent(parsed, indexToolBodyPreview)
							continue
						}
					}
					slim[key] = slimContent(value, indexToolBodyPreview)
				}
			}
			result["payload"] = truncateStrings(slim, indexToolInputLimit)
			return result, nil
		}
	}
	return nil, nil
}

// IndexCodexThread advances one thread's index until it is current or the
// deadline passes. It returns true when the index covers the thread's current
// generation and item count. Missing projections remove derived rows.
func (service *Service) IndexCodexThread(ctx context.Context, storeID, threadID string, deadline time.Time) (bool, error) {
	for {
		var generation, itemCount int64
		err := service.pool.QueryRow(ctx, `SELECT active_generation,item_count FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2`, storeID, threadID).Scan(&generation, &itemCount)
		if errors.Is(err, pgx.ErrNoRows) {
			return true, service.dropCodexIndex(ctx, storeID, threadID)
		}
		if err != nil {
			return false, err
		}
		var stateGeneration, indexed int64
		var version int
		var carryRaw []byte
		err = service.pool.QueryRow(ctx, `SELECT generation,indexed_count,projector_version,carry FROM mira_conversation_index_state WHERE store_id=$1 AND thread_id=$2`, storeID, threadID).Scan(&stateGeneration, &indexed, &version, &carryRaw)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (stateGeneration != generation || version != ConversationIndexVersion) {
			if err := service.resetCodexIndex(ctx, storeID, threadID, generation); err != nil {
				return false, err
			}
			indexed, carryRaw = 0, []byte("{}")
		} else if err != nil {
			return false, err
		}
		if indexed >= itemCount {
			return true, nil
		}
		carry := codexIndexCarry{}
		_ = json.Unmarshal(carryRaw, &carry)
		progressed, err := service.indexCodexBatch(ctx, storeID, threadID, generation, indexed, itemCount, &carry)
		if err != nil {
			return false, err
		}
		if !progressed {
			return false, nil // Incomplete canonical history or a concurrent indexer; retry later.
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return false, nil
		}
	}
}

func (service *Service) dropCodexIndex(ctx context.Context, storeID, threadID string) error {
	_, err := service.pool.Exec(ctx, `WITH a AS (DELETE FROM mira_conversation_records WHERE store_id=$1 AND thread_id=$2),
	  b AS (DELETE FROM mira_conversation_messages WHERE engine='codex' AND store_id=$1 AND thread_id=$2)
	  DELETE FROM mira_conversation_index_state WHERE store_id=$1 AND thread_id=$2`, storeID, threadID)
	return err
}

func (service *Service) resetCodexIndex(ctx context.Context, storeID, threadID string, generation int64) error {
	return pgx.BeginFunc(ctx, service.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM mira_conversation_records WHERE store_id=$1 AND thread_id=$2`, storeID, threadID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM mira_conversation_messages WHERE engine='codex' AND store_id=$1 AND thread_id=$2`, storeID, threadID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO mira_conversation_index_state(store_id,thread_id,generation,indexed_count,projector_version,carry)
		  VALUES($1,$2,$3,0,$4,'{}') ON CONFLICT(store_id,thread_id) DO UPDATE SET generation=EXCLUDED.generation,
		  indexed_count=0,projector_version=EXCLUDED.projector_version,carry='{}',updated_at=now()`, storeID, threadID, generation, ConversationIndexVersion)
		return err
	})
}

// indexCodexBatch reads canonical rows in small chunks, so one large tool
// output never multiplies memory, then publishes up to indexWriteBatch slim
// records in one transaction guarded by the projection and state rows.
func (service *Service) indexCodexBatch(ctx context.Context, storeID, threadID string, generation, indexed, itemCount int64, carry *codexIndexCarry) (bool, error) {
	var imported bool
	if err := service.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_codex_session_imports WHERE store_id=$1 AND thread_id=$2 AND status='imported')`, storeID, threadID).Scan(&imported); err != nil {
		return false, err
	}
	results := []indexedRecord{}
	next := indexed
	limit := min(itemCount, indexed+indexWriteBatch)
	for next < limit {
		end := min(limit, next+indexReadChunk)
		rows, err := service.pool.Query(ctx, `SELECT item_seq,payload,created_at,cold_sha256 FROM codex_thread_events
		  WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq>$4 AND item_seq<=$5 ORDER BY item_seq`, storeID, threadID, generation, next, end)
		if err != nil {
			return false, err
		}
		chunk := []transcriptRow{}
		for rows.Next() {
			var sequence int64
			var raw []byte
			var created time.Time
			var cold *string
			if err := rows.Scan(&sequence, &raw, &created, &cold); err != nil {
				rows.Close()
				return false, err
			}
			if raw, err = coldstore.Hydrate(ctx, raw, cold); err != nil {
				rows.Close()
				return false, err
			}
			payload, err := decodeObject(raw)
			if err != nil {
				rows.Close()
				return false, fmt.Errorf("decode thread %s item %d: %w", threadID, sequence, err)
			}
			chunk = append(chunk, transcriptRow{sequence, payload, &created})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
		for index, row := range chunk {
			if row.sequence != next+int64(index)+1 {
				chunk = chunk[:index]
				break
			}
		}
		recorded := map[int64]string{}
		if imported && len(chunk) > 0 {
			if recorded, err = service.restoreImportedTimestamps(ctx, storeID, threadID, chunk); err != nil {
				return false, err
			}
		} else {
			for _, row := range chunk {
				recorded[row.sequence] = formatTime(*row.createdAt)
			}
		}
		for _, row := range chunk {
			slim, message := slimCodexRecord(row.payload, carry)
			if slim == nil {
				continue
			}
			encoded, err := json.Marshal(slim)
			if err != nil {
				return false, err
			}
			entry := indexedRecord{sequence: row.sequence, kind: recordKind(slim), record: encoded, message: message}
			if text, ok := recorded[row.sequence]; ok {
				if parsed, ok := parseISO(text); ok {
					entry.recordedAt = &parsed
				}
			} else if parsed, ok := parseISO(recordTimestamp(row.payload)); ok {
				entry.recordedAt = &parsed
			}
			results = append(results, entry)
		}
		if int64(len(chunk)) < end-next {
			next += int64(len(chunk))
			break // A gap in canonical history; publish the contiguous prefix only.
		}
		next = end
	}
	if next == indexed {
		return false, nil
	}
	carryRaw, _ := json.Marshal(carry)
	published := false
	err := pgx.BeginFunc(ctx, service.pool, func(tx pgx.Tx) error {
		var current int64
		// FOR SHARE serializes with permanent deletion and generation changes,
		// which delete or update the projection row before removing index rows.
		if err := tx.QueryRow(ctx, `SELECT active_generation FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2 FOR SHARE`, storeID, threadID).Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		var stateIndexed int64
		if err := tx.QueryRow(ctx, `SELECT indexed_count FROM mira_conversation_index_state WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND projector_version=$4 FOR UPDATE`,
			storeID, threadID, generation, ConversationIndexVersion).Scan(&stateIndexed); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if current != generation || stateIndexed != indexed {
			return nil
		}
		sequences, times, records, kinds := make([]int64, 0, len(results)), make([]*time.Time, 0, len(results)), make([]string, 0, len(results)), make([]string, 0, len(results))
		messageSeqs, roles, bodies, messageTimes := []int64{}, []string{}, []string{}, []*time.Time{}
		for _, result := range results {
			sequences, times, records, kinds = append(sequences, result.sequence), append(times, result.recordedAt), append(records, string(result.record)), append(kinds, result.kind)
			if result.message != nil && strings.TrimSpace(result.message.body) != "" {
				messageSeqs, roles, bodies, messageTimes = append(messageSeqs, result.sequence), append(roles, result.message.role), append(bodies, result.message.body), append(messageTimes, result.recordedAt)
			}
		}
		if len(sequences) > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO mira_conversation_records(store_id,thread_id,generation,item_seq,recorded_at,kind,record)
			  SELECT $1,$2,$3,s,r,k,j::json FROM unnest($4::bigint[],$5::timestamptz[],$6::text[],$7::text[]) AS t(s,r,j,k) ON CONFLICT DO NOTHING`,
				storeID, threadID, generation, sequences, times, records, kinds); err != nil {
				return err
			}
		}
		if len(messageSeqs) > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO mira_conversation_messages(engine,store_id,thread_id,generation,item_seq,role,body,recorded_at)
			  SELECT 'codex',$1,$2,$3,s,r,b,t FROM unnest($4::bigint[],$5::text[],$6::text[],$7::timestamptz[]) AS u(s,r,b,t) ON CONFLICT DO NOTHING`,
				storeID, threadID, generation, messageSeqs, roles, bodies, messageTimes); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE mira_conversation_index_state SET indexed_count=$3,carry=$4,updated_at=now() WHERE store_id=$1 AND thread_id=$2`,
			storeID, threadID, next, string(carryRaw)); err != nil {
			return err
		}
		published = true
		return nil
	})
	return published, err
}

// IndexClaudeTranscript indexes one Claude conversation's prose. The root
// (subpath "") reads durable session events and is addressed by session ID;
// a native subagent transcript reads its own entries and is addressed by its
// transcript thread ID, matching the Web conversation identity.
func (service *Service) IndexClaudeTranscript(ctx context.Context, sessionID, subpath string) error {
	threadID := sessionID
	query := `SELECT seq,payload,cold_sha256 FROM mira_claude_events WHERE session_id=$1 AND seq>$2 AND event_type<>'stream_event' ORDER BY seq LIMIT $3`
	if subpath != "" {
		if err := service.pool.QueryRow(ctx, `SELECT thread_id::text FROM mira_claude_transcripts WHERE session_id=$1 AND subpath=$2`, sessionID, subpath).Scan(&threadID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		query = `SELECT seq,payload,cold_sha256 FROM mira_claude_entries WHERE session_id=$1 AND seq>$2 AND subpath=$4 ORDER BY seq LIMIT $3`
	}
	var indexed int64
	var version int
	err := service.pool.QueryRow(ctx, `SELECT indexed_seq,projector_version FROM mira_claude_index_state WHERE session_id=$1 AND subpath=$2`, sessionID, subpath).Scan(&indexed, &version)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && version != ConversationIndexVersion {
		if _, err := service.pool.Exec(ctx, `WITH d AS (DELETE FROM mira_conversation_messages WHERE engine='claude' AND thread_id=$3)
		  INSERT INTO mira_claude_index_state(session_id,subpath,indexed_seq,projector_version) VALUES($1::uuid,$2,0,$4)
		  ON CONFLICT(session_id,subpath) DO UPDATE SET indexed_seq=0,projector_version=EXCLUDED.projector_version,updated_at=now()`,
			sessionID, subpath, threadID, ConversationIndexVersion); err != nil {
			return err
		}
		indexed = 0
	} else if err != nil {
		return err
	}
	for {
		args := []any{sessionID, indexed, indexWriteBatch}
		if subpath != "" {
			args = append(args, subpath)
		}
		rows, err := service.pool.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		sequences, roles, bodies, times := []int64{}, []string{}, []string{}, []*time.Time{}
		last := indexed
		for rows.Next() {
			var sequence int64
			var raw []byte
			var cold *string
			if err := rows.Scan(&sequence, &raw, &cold); err != nil {
				rows.Close()
				return err
			}
			last = sequence
			if raw, err = coldstore.Hydrate(ctx, raw, cold); err != nil {
				rows.Close()
				return err
			}
			event, err := decodeObject(raw)
			if err != nil {
				continue // Unparseable native records stay authoritative and unindexed.
			}
			role, body := claudeIndexedText(event, subpath != "")
			if strings.TrimSpace(body) == "" {
				continue
			}
			var at *time.Time
			if parsed, ok := parseISO(isoTimestamp(event["timestamp"], 1)); ok {
				at = &parsed
			}
			sequences, roles, bodies, times = append(sequences, sequence), append(roles, role), append(bodies, messageBody(body)), append(times, at)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if last == indexed {
			return nil
		}
		err = pgx.BeginFunc(ctx, service.pool, func(tx pgx.Tx) error {
			var stateIndexed int64
			if err := tx.QueryRow(ctx, `SELECT indexed_seq FROM mira_claude_index_state WHERE session_id=$1 AND subpath=$2 FOR UPDATE`, sessionID, subpath).Scan(&stateIndexed); err != nil {
				return err
			}
			if stateIndexed != indexed {
				return errClaudeIndexRace
			}
			if len(sequences) > 0 {
				if _, err := tx.Exec(ctx, `INSERT INTO mira_conversation_messages(engine,store_id,thread_id,generation,item_seq,role,body,recorded_at)
				  SELECT 'claude','',$1,1,s,r,b,t FROM unnest($2::bigint[],$3::text[],$4::text[],$5::timestamptz[]) AS u(s,r,b,t) ON CONFLICT DO NOTHING`,
					threadID, sequences, roles, bodies, times); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, `UPDATE mira_claude_index_state SET indexed_seq=$3,updated_at=now() WHERE session_id=$1 AND subpath=$2`, sessionID, subpath, last)
			return err
		})
		if errors.Is(err, errClaudeIndexRace) {
			return nil
		}
		if err != nil {
			return err
		}
		indexed = last
	}
}

var errClaudeIndexRace = errors.New("claude index advanced concurrently")

func claudeTextBlocks(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	parts := []string{}
	for _, block := range array(content) {
		item := object(block)
		if stringValue(item["type"]) == "text" {
			parts = append(parts, stringValue(item["text"]))
		}
	}
	return strings.Join(parts, "\n\n")
}

// claudeIndexedText extracts what the Web transcript shows as prose: Mira
// prompts and root assistant text for a session; the prompt and assistant
// text of a native subagent transcript. Tool results are never prose.
func claudeIndexedText(event map[string]any, native bool) (string, string) {
	switch stringValue(event["type"]) {
	case "mira_user":
		if !native {
			return "user", stringValue(event["text"])
		}
	case "user":
		if native && event["isCompactSummary"] != true {
			content := object(event["message"])["content"]
			for _, block := range array(content) {
				if stringValue(object(block)["type"]) == "tool_result" {
					return "", ""
				}
			}
			return "user", claudeTextBlocks(content)
		}
	case "assistant":
		if !native && event["parent_tool_use_id"] != nil {
			return "", "" // Subagent prose is indexed from its own transcript.
		}
		return "assistant", claudeTextBlocks(object(event["message"])["content"])
	}
	return "", ""
}

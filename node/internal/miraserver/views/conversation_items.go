package views

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ssine/mira/node/internal/miraserver/coldstore"
)

const (
	// ToolFieldLimitMax bounds one tool input or output in a conversation read,
	// matching the Web projection's 1 MiB field bound.
	ToolFieldLimitMax       = 1024 * 1024
	ToolFieldLimitDefault   = 8 * 1024
	conversationRecordsPage = 256
	// A page may extend past its record count to finish the current turn, so a
	// tool call and its output stay together, within these bounds.
	conversationRecordsHard = 2048
	conversationPageBytes   = 16 * 1024 * 1024
	conversationReadChunk   = 64
)

type conversationTool struct {
	Key             string `json:"key"`
	Name            string `json:"name"`
	Status          string `json:"status,omitempty"`
	Input           string `json:"input,omitempty"`
	Output          string `json:"output,omitempty"`
	InputBytes      int    `json:"inputBytes"`
	OutputBytes     int    `json:"outputBytes"`
	InputTruncated  bool   `json:"inputTruncated,omitempty"`
	OutputTruncated bool   `json:"outputTruncated,omitempty"`
	HasOutput       bool   `json:"hasOutput"`
}

// conversationItem is one displayed entry of a conversation read: prose
// (user/assistant), a tool call with its output, an error or a notice.
type conversationItem struct {
	Seq  int64  `json:"seq"`
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	// Summary is the compaction summary that replaced the earlier context.
	Summary    string            `json:"summary,omitempty"`
	RecordedAt *string           `json:"recordedAt"`
	Tool       *conversationTool `json:"tool,omitempty"`
}

// clipField truncates text to limit bytes on a UTF-8 boundary.
func clipField(text string, limit int) (string, int, bool) {
	size := len(text)
	if limit <= 0 || limit > ToolFieldLimitMax {
		limit = ToolFieldLimitMax
	}
	if size <= limit {
		return text, size, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], size, true
}

func newConversationTool(key, name, status, input string, output *string, limit int) *conversationTool {
	tool := &conversationTool{Key: key, Name: name, Status: status}
	tool.Input, tool.InputBytes, tool.InputTruncated = clipField(input, limit)
	if output != nil {
		tool.HasOutput = true
		tool.Output, tool.OutputBytes, tool.OutputTruncated = clipField(*output, limit)
	}
	return tool
}

var errHistoryIncomplete = errors.New("thread history is incomplete")

type canonicalRow struct {
	seq     int64
	raw     []byte
	created *time.Time
}

func isTurnStartRecord(record map[string]any) bool {
	return stringValue(record["type"]) == "turn_context" ||
		stringValue(record["type"]) == "event_msg" && includes(activityStarts, stringValue(object(record["payload"])["type"]))
}

// readCodexItems projects canonical Codex history after one item sequence,
// oldest first, with the Web transcript projector. Pages end before a turn
// start when possible and are bounded by records and raw bytes.
func (service *Service) readCodexItems(ctx context.Context, storeID, threadID string, generation, after int64, records, toolLimit int) ([]conversationItem, *int64, error) {
	if records <= 0 || records > 1000 {
		records = conversationRecordsPage
	}
	rows := []transcriptRow{}
	total, last, exhausted := 0, after, false
	var pending *transcriptRow
collect:
	for len(rows) < conversationRecordsHard {
		chunk, err := service.pool.Query(ctx, `SELECT item_seq,payload,created_at,cold_sha256 FROM codex_thread_events
		  WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq>$4 ORDER BY item_seq LIMIT $5`,
			storeID, threadID, generation, last, conversationReadChunk)
		if err != nil {
			return nil, nil, err
		}
		batch := []canonicalRow{}
		for chunk.Next() {
			var row canonicalRow
			var cold *string
			var created time.Time
			if err := chunk.Scan(&row.seq, &row.raw, &created, &cold); err != nil {
				chunk.Close()
				return nil, nil, err
			}
			if row.raw, err = coldstore.Hydrate(ctx, row.raw, cold); err != nil {
				chunk.Close()
				return nil, nil, err
			}
			row.created = &created
			batch = append(batch, row)
		}
		chunk.Close()
		if err := chunk.Err(); err != nil {
			return nil, nil, err
		}
		for _, row := range batch {
			if row.seq != last+1 {
				return nil, nil, errHistoryIncomplete
			}
			record, err := decodeObject(row.raw)
			if err != nil {
				return nil, nil, err
			}
			if len(rows) > 0 && (len(rows) >= records && isTurnStartRecord(record) || total+len(row.raw) > conversationPageBytes) {
				pending = &transcriptRow{row.seq, record, row.created}
				break collect
			}
			rows = append(rows, transcriptRow{row.seq, record, row.created})
			total += len(row.raw)
			last = row.seq
		}
		if len(batch) < conversationReadChunk {
			exhausted = true
			break
		}
	}
	if len(rows) == 0 {
		return []conversationItem{}, nil, nil
	}
	options := ProjectionOptions{ItemOffset: rows[0].sequence - 1, Fragments: true}
	var contextRow *transcriptRow
	if !isTurnStartRecord(rows[0].payload) && rows[0].sequence > 1 {
		var err error
		if contextRow, err = service.contextBefore(ctx, storeID, threadID, generation, rows[0].sequence); err != nil {
			return nil, nil, err
		}
	}
	provenance := append([]transcriptRow(nil), rows...)
	if contextRow != nil {
		provenance = append([]transcriptRow{*contextRow}, provenance...)
	}
	recordedAt, err := service.restoreImportedTimestamps(ctx, storeID, threadID, provenance)
	if err != nil {
		return nil, nil, err
	}
	options.RecordedAt = recordedAt
	// Imported threads restore original timestamps into the payloads.
	offset := len(provenance) - len(rows)
	for index := range rows {
		rows[index].payload = provenance[offset+index].payload
	}
	if contextRow != nil {
		payload := object(contextRow.payload["payload"])
		options.InitialTurnID, options.InitialCwd = stringValue(payload["turn_id"]), stringValue(payload["cwd"])
	}
	items := make([]map[string]any, len(rows))
	for index, row := range rows {
		items[index] = row.payload
	}
	result := []conversationItem{}
	for _, entry := range ProjectCodexTranscript(items, options) {
		sequence, _ := safeInteger(entry["sourceItemSeq"])
		item := conversationItem{Seq: sequence}
		if value, ok := recordedAt[sequence]; ok {
			item.RecordedAt = &value
		}
		switch kind := stringValue(entry["kind"]); kind {
		case "user", "assistant":
			item.Kind, item.Text = kind, stringValue(entry["body"])
		case "tool":
			item.Kind = "tool"
			fragment := object(entry["toolFragment"])
			if fragment == nil || fragment["materialized"] == true {
				body := stringValue(entry["body"])
				item.Tool = newConversationTool(stringValue(entry["key"]), stringValue(entry["title"]), stringValue(entry["status"]), "", &body, toolLimit)
			} else {
				var output *string
				if value, ok := fragment["output"].(string); ok {
					output = &value
				}
				input, _ := fragment["input"].(string)
				item.Tool = newConversationTool(stringValue(entry["key"]), stringValue(entry["title"]), stringValue(entry["status"]), input, output, toolLimit)
			}
		case "error":
			item.Kind, item.Text = "error", stringValue(entry["body"])
		case "compaction":
			item.Kind, item.Text, item.Summary = "compaction", stringValue(entry["body"]), stringValue(entry["compactionSummary"])
		case "image":
			item.Kind, item.Text = "notice", "[图片]"
		default:
			continue // Reasoning and lifecycle entries are not part of a read.
		}
		result = append(result, item)
	}
	if exhausted && pending == nil {
		return result, nil, nil
	}
	return result, &last, nil
}

// claudeCompactionText mirrors the Web transcript's compaction line and returns
// the anchor of the preserved segment, which marks the summary message.
func claudeCompactionText(meta map[string]any) (string, string) {
	manual := stringValue(meta["trigger"]) == "manual"
	before, hasBefore := safeInteger(first(meta["pre_tokens"], meta["preTokens"]))
	after, hasAfter := safeInteger(first(meta["post_tokens"], meta["postTokens"]))
	text := "较早的上下文已自动压缩"
	if manual {
		text = "较早的上下文已压缩"
	}
	switch {
	case hasBefore && hasAfter:
		text += fmt.Sprintf("（%s → %s tokens）", groupedInteger(before), groupedInteger(after))
	case hasBefore:
		text += fmt.Sprintf("（压缩前 %s tokens）", groupedInteger(before))
	}
	anchor := stringValue(first(object(meta["preserved_messages"])["anchor_uuid"], object(meta["preserved_segment"])["anchor_uuid"]))
	return text + "。", anchor
}

// claudeToolText renders a tool_use input or tool_result content as text.
func claudeToolText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		parts := []string{}
		for _, block := range typed {
			item := object(block)
			switch stringValue(item["type"]) {
			case "text":
				parts = append(parts, stringValue(item["text"]))
			case "image":
				parts = append(parts, "[图片]")
			default:
				encoded, _ := json.Marshal(block)
				parts = append(parts, string(encoded))
			}
		}
		return strings.Join(parts, "\n")
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ""
	}
	return string(encoded)
}

// readClaudeItems reads a Claude root session (durable events) or a native
// subagent transcript (entries) after one sequence, pairing each tool_use with
// its tool_result when both are on the page. A result whose call was on an
// earlier page is emitted with the same key so readers can join them.
func (service *Service) readClaudeItems(ctx context.Context, sessionID, subpath string, after int64, records, toolLimit int) ([]conversationItem, *int64, error) {
	if records <= 0 || records > 1000 {
		records = conversationRecordsPage
	}
	query := `SELECT seq,payload,cold_sha256 FROM mira_claude_events WHERE session_id=$1 AND seq>$2 AND event_type<>'stream_event' ORDER BY seq LIMIT $3`
	args := []any{sessionID, after, records + 1}
	if subpath != "" {
		query = `SELECT seq,payload,cold_sha256 FROM mira_claude_entries WHERE session_id=$1 AND seq>$2 AND subpath=$4 ORDER BY seq LIMIT $3`
		args = append(args, subpath)
	}
	rows, err := service.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	type event struct {
		seq   int64
		value map[string]any
	}
	events := []event{}
	total, more := 0, false
	for rows.Next() {
		var sequence int64
		var raw []byte
		var cold *string
		if err := rows.Scan(&sequence, &raw, &cold); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if len(events) >= records || len(events) > 0 && total+len(raw) > conversationPageBytes {
			more = true
			break
		}
		if raw, err = coldstore.Hydrate(ctx, raw, cold); err != nil {
			rows.Close()
			return nil, nil, err
		}
		total += len(raw)
		value, err := decodeObject(raw)
		if err != nil {
			value = map[string]any{} // Unparseable native records stay authoritative and unread here.
		}
		events = append(events, event{sequence, value})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	native := subpath != ""
	result := []conversationItem{}
	pendingTools := map[string]int{}
	// Compaction is recorded as a boundary followed by a synthetic user message
	// holding the summary (the Web transcript's rule); that message is not prose.
	summaryAt, summaryAnchor := -1, ""
	for _, entry := range events {
		value := entry.value
		var recordedAt *string
		if parsed, ok := parseISO(isoTimestamp(value["timestamp"], 1)); ok {
			recordedAt = optionalTime(&parsed)
		}
		push := func(item conversationItem) {
			item.Seq, item.RecordedAt = entry.seq, recordedAt
			result = append(result, item)
		}
		message := object(value["message"])
		if stringValue(value["type"]) == "user" && summaryAt >= 0 && value["parent_tool_use_id"] == nil &&
			(value["isCompactSummary"] == true || summaryAnchor != "" && stringValue(value["uuid"]) == summaryAnchor || summaryAnchor == "" && value["isSynthetic"] == true) {
			result[summaryAt].Summary = claudeTextBlocks(message["content"])
			summaryAt = -1
			continue
		}
		switch stringValue(value["type"]) {
		case "system":
			if stringValue(value["subtype"]) != "compact_boundary" {
				continue
			}
			meta := object(first(value["compact_metadata"], value["compactMetadata"]))
			text, anchor := claudeCompactionText(meta)
			push(conversationItem{Kind: "compaction", Text: text})
			summaryAt, summaryAnchor = len(result)-1, anchor
		case "mira_user":
			if !native {
				push(conversationItem{Kind: "user", Text: stringValue(value["text"])})
			}
		case "assistant":
			if !native && value["parent_tool_use_id"] != nil {
				continue // Subagent activity is read from its own transcript.
			}
			for _, block := range array(message["content"]) {
				item := object(block)
				switch stringValue(item["type"]) {
				case "text":
					if text := stringValue(item["text"]); strings.TrimSpace(text) != "" {
						push(conversationItem{Kind: "assistant", Text: text})
					}
				case "tool_use":
					id := stringValue(item["id"])
					push(conversationItem{Kind: "tool", Tool: newConversationTool(id, stringValue(item["name"]), "", claudeToolText(item["input"]), nil, toolLimit)})
					pendingTools[id] = len(result) - 1
				}
			}
		case "user":
			if !native && value["parent_tool_use_id"] != nil {
				continue
			}
			if value["isCompactSummary"] == true {
				// Its boundary ended the previous page: readers join the two.
				push(conversationItem{Kind: "compaction", Summary: claudeTextBlocks(message["content"])})
				continue
			}
			content := message["content"]
			if text, ok := content.(string); ok {
				if native && strings.TrimSpace(text) != "" {
					push(conversationItem{Kind: "user", Text: text})
				}
				continue
			}
			texts := []string{}
			for _, block := range array(content) {
				item := object(block)
				switch stringValue(item["type"]) {
				case "tool_result":
					id := stringValue(item["tool_use_id"])
					output := claudeToolText(item["content"])
					status := "完成"
					if item["is_error"] == true {
						status = "失败"
					}
					if index, ok := pendingTools[id]; ok {
						tool := result[index].Tool
						tool.Status, tool.HasOutput = status, true
						tool.Output, tool.OutputBytes, tool.OutputTruncated = clipField(output, toolLimit)
						delete(pendingTools, id)
					} else {
						push(conversationItem{Kind: "tool", Tool: newConversationTool(id, "", status, "", &output, toolLimit)})
					}
				case "text":
					texts = append(texts, stringValue(item["text"]))
				}
			}
			if native && len(texts) > 0 && strings.TrimSpace(strings.Join(texts, "")) != "" {
				push(conversationItem{Kind: "user", Text: strings.Join(texts, "\n\n")})
			}
		case "result":
			if value["is_error"] == true {
				text := stringValue(value["result"])
				if text == "" {
					text = claudeToolText(value["errors"])
				}
				push(conversationItem{Kind: "error", Text: text})
			}
		}
	}
	if !more || len(events) == 0 {
		return result, nil, nil
	}
	next := events[len(events)-1].seq
	return result, &next, nil
}

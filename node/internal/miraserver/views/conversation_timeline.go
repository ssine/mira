package views

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/coldstore"
)

const (
	timelineMaxLanes      = 200
	timelineCatchUpBudget = 4 * time.Second
	timelinePreviewRunes  = 120
)

type timelineTurn struct {
	ID     string `json:"id"`
	Start  int64  `json:"start"`
	End    int64  `json:"end"`
	Open   bool   `json:"open,omitempty"`
	Status string `json:"status,omitempty"`
}

type timelineTool struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	Open  bool   `json:"open,omitempty"`
	// Close marks an incremental result for a tool started before the cursor:
	// it only ends that tool (the client already has its start and name).
	Close  bool   `json:"close,omitempty"`
	Kind   string `json:"kind,omitempty"` // tool | wait | agent
	Seq    int64  `json:"seq"`
	TurnID string `json:"turnId,omitempty"`
}

type timelineMessage struct {
	At      int64  `json:"at"`
	Seq     int64  `json:"seq"`
	Role    string `json:"role"`
	Preview string `json:"preview"`
}

type timelineLane struct {
	ThreadID       string  `json:"threadId"`
	ParentThreadID *string `json:"parentThreadId"`
	Title          *string `json:"title"`
	Depth          int     `json:"depth"`
	Partial        bool    `json:"partial,omitempty"`
	// Incremental reads: Unchanged lanes carry no items; Reset lanes replace
	// the client's copy; other lanes are merged by item identity.
	Unchanged bool              `json:"unchanged,omitempty"`
	Reset     bool              `json:"reset,omitempty"`
	Turns     []timelineTurn    `json:"turns"`
	Tools     []timelineTool    `json:"tools"`
	Messages  []timelineMessage `json:"messages"`
}

func previewText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > timelinePreviewRunes {
		return string(runes[:timelinePreviewRunes]) + "…"
	}
	return text
}

// toolKind separates explicit waiting from ordinary tool execution, so the
// timeline does not present a parent waiting on children as its own work.
func toolKind(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "wait"):
		return "wait"
	case strings.Contains(lower, "spawn_agent") || strings.Contains(lower, "send_input") || lower == "task" || lower == "agent":
		return "agent"
	}
	return "tool"
}

func millis(value string) (int64, bool) {
	parsed, ok := parseISO(value)
	if !ok {
		return 0, false
	}
	return parsed.UnixMilli(), true
}

// codexTimelineLane derives lifecycle, tool and message events from one
// thread's index records. Times prefer explicit lifecycle fields and fall
// back to the record's persisted (or restored imported) time.
func codexTimelineLane(records []outlineRecord) ([]timelineTurn, []timelineTool, []timelineMessage) {
	turns := []timelineTurn{}
	turnIndex := map[string]int{}
	tools := []timelineTool{}
	toolIndex := map[string]int{}
	messages := []timelineMessage{}
	currentTurn := ""
	lastAt := int64(0)
	for _, entry := range records {
		record := entry.record
		payload := object(record["payload"])
		at, ok := millis(recordTimestamp(record))
		if !ok && entry.recordedAt != nil {
			at, ok = entry.recordedAt.UnixMilli(), true
		}
		if !ok {
			continue
		}
		lastAt = max(lastAt, at)
		recordType, kind := stringValue(record["type"]), stringValue(payload["type"])
		turnID := stringValue(first(payload["turn_id"], object(payload["internal_chat_message_metadata_passthrough"])["turn_id"]))
		if recordType == "turn_context" && turnID != "" {
			currentTurn = turnID
		}
		if turnID == "" {
			turnID = currentTurn
		}
		ensureTurn := func(id string, start int64) *timelineTurn {
			if index, ok := turnIndex[id]; ok {
				return &turns[index]
			}
			turns = append(turns, timelineTurn{ID: id, Start: start, End: start, Open: true})
			turnIndex[id] = len(turns) - 1
			return &turns[len(turns)-1]
		}
		switch {
		case recordType == "event_msg" && includes(activityStarts, kind):
			start := at
			if explicit, ok := millis(turnStartedAt(record)); ok {
				start = explicit
			}
			if turnID != "" {
				currentTurn = turnID
				turn := ensureTurn(turnID, start)
				turn.Start = min(turn.Start, start)
			}
		case recordType == "event_msg" && includes(activityEnds, kind):
			if turnID != "" {
				turn := ensureTurn(turnID, at)
				if explicit, ok := millis(turnStartedAt(record)); ok {
					turn.Start = min(turn.Start, explicit)
				}
				turn.End, turn.Open = max(turn.End, at), false
				turn.Status = map[string]string{"turn_aborted": "aborted"}[kind]
				if turn.Status == "" {
					turn.Status = "completed"
				}
			}
		case recordType == "event_msg" && (kind == "user_message" || kind == "agent_message"):
			role := "user"
			if kind == "agent_message" {
				role = "assistant"
			}
			messages = append(messages, timelineMessage{At: at, Seq: entry.sequence, Role: role, Preview: previewText(stringValue(payload["message"]))})
		case recordType == "response_item" && includes([]string{"custom_tool_call", "function_call", "local_shell_call", "tool_search_call", "web_search_call"}, kind):
			id := stringValue(first(payload["call_id"], payload["id"]))
			name := normalizedToolTitle(payload)
			if index, exists := toolIndex[id]; exists && id != "" {
				tools[index].Start = min(tools[index].Start, at)
				continue
			}
			tools = append(tools, timelineTool{ID: id, Name: name, Start: at, End: at, Open: kind != "web_search_call", Kind: toolKind(name), Seq: entry.sequence, TurnID: turnID})
			if id != "" {
				toolIndex[id] = len(tools) - 1
			}
		case recordType == "response_item" && includes([]string{"custom_tool_call_output", "function_call_output", "tool_search_output"}, kind):
			if index, exists := toolIndex[stringValue(payload["call_id"])]; exists {
				tools[index].End, tools[index].Open = max(tools[index].End, at), false
			}
		case recordType == "event_msg" && kind == "item_completed":
			item := object(payload["item"])
			view := ToolItemView(item)
			id := stringValue(item["id"])
			if view == nil || id == "" {
				continue
			}
			if index, exists := toolIndex[id]; exists {
				tools[index].End, tools[index].Open = max(tools[index].End, at), false
				continue
			}
			start := at
			if duration, ok := number(durationMS(item)); ok && duration > 0 {
				start = at - int64(duration)
			}
			name := stringValue(view["title"])
			if name == "" {
				name = normalizedToolTitle(item)
			}
			tools = append(tools, timelineTool{ID: id, Name: name, Start: start, End: at, Kind: toolKind(name), Seq: entry.sequence, TurnID: turnID})
			toolIndex[id] = len(tools) - 1
		}
	}
	// Unfinished intervals end at their last evidence, never "now".
	for index := range turns {
		if turns[index].Open {
			turns[index].End = max(turns[index].End, lastAt)
		}
	}
	for index := range tools {
		if tools[index].Open {
			end := tools[index].Start
			for _, turn := range turns {
				if turn.ID == tools[index].TurnID && !turn.Open {
					end = max(end, turn.End)
				}
			}
			tools[index].End = max(end, tools[index].Start)
		}
	}
	return turns, tools, messages
}

// Incremental timeline reads. A response carries an opaque cursor; passing it
// back returns only what changed since. Codex cursors hold each lane's
// generation and indexed record count, so unchanged lanes are skipped without
// reading records and changed lanes are re-derived from their latest turn
// start (the outline's incremental rule). Claude cursors hold the last event
// sequence read. The client merges items by identity: turns and tools by ID
// (tools without one by sequence), messages by sequence.
type codexLaneCursor struct{ generation, indexed int64 }

func parseCodexTimelineCursor(raw string) map[string]codexLaneCursor {
	body, ok := strings.CutPrefix(raw, "x1.")
	if !ok {
		return nil
	}
	lanes := map[string]codexLaneCursor{}
	for _, entry := range strings.Split(body, ",") {
		parts := strings.Split(entry, ":")
		if len(parts) != 3 || len(parts[0]) != 36 {
			return nil
		}
		generation, err1 := strconv.ParseInt(parts[1], 10, 64)
		indexed, err2 := strconv.ParseInt(parts[2], 10, 64)
		if err1 != nil || err2 != nil || generation < 0 || indexed < 0 {
			return nil
		}
		lanes[strings.ToLower(parts[0])] = codexLaneCursor{generation, indexed}
	}
	return lanes
}

func formatCodexTimelineCursor(lanes []*timelineLane, cursors map[string]codexLaneCursor) string {
	parts := make([]string, 0, len(lanes))
	for _, lane := range lanes {
		if value, ok := cursors[lane.ThreadID]; ok {
			parts = append(parts, fmt.Sprintf("%s:%d:%d", lane.ThreadID, value.generation, value.indexed))
		}
	}
	return "x1." + strings.Join(parts, ",")
}

// GetCodexTimeline returns lanes for a root conversation and its subagent
// tree; with a cursor from an earlier response, only what changed since.
func (service *Service) GetCodexTimeline(ctx context.Context, storeID, threadID, cursor string) (Result, error) {
	known := parseCodexTimelineCursor(cursor)
	delta := known != nil
	rows, err := service.pool.Query(ctx, `WITH RECURSIVE tree AS (
	    SELECT p.thread_id,p.parent_thread_id,0 AS depth FROM codex_thread_projections p WHERE p.store_id=$1 AND p.thread_id=$2
	    UNION ALL
	    SELECT p.thread_id,p.parent_thread_id,t.depth+1 FROM tree t JOIN codex_thread_projections p
	      ON p.store_id=$1 AND p.parent_thread_id=t.thread_id AND p.thread_id<>t.thread_id WHERE t.depth<16
	  )
	  SELECT t.thread_id,t.parent_thread_id,t.depth,COALESCE(NULLIF(n.value#>>'{}',''),p.title),p.active_generation,p.item_count FROM tree t
	  JOIN codex_thread_projections p ON p.store_id=$1 AND p.thread_id=t.thread_id
	  LEFT JOIN codex_store_state_entries n ON n.store_id=$1 AND n.field='names' AND NOT n.is_root AND n.entry_key=t.thread_id
	  ORDER BY t.depth,t.thread_id LIMIT $3`, storeID, threadID, timelineMaxLanes)
	if err != nil {
		return Result{}, err
	}
	lanes := []*timelineLane{}
	projected := map[string]codexLaneCursor{}
	for rows.Next() {
		lane := &timelineLane{}
		var current codexLaneCursor
		if err := rows.Scan(&lane.ThreadID, &lane.ParentThreadID, &lane.Depth, &lane.Title, &current.generation, &current.indexed); err != nil {
			rows.Close()
			return Result{}, err
		}
		lane.ThreadID = strings.ToLower(lane.ThreadID)
		projected[lane.ThreadID] = current
		lanes = append(lanes, lane)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Result{}, err
	}
	if len(lanes) == 0 {
		return errorResult(404, "thread history not found", "not_found"), nil
	}
	deadline := time.Now().Add(timelineCatchUpBudget)
	partial := false
	next := map[string]codexLaneCursor{}
	for _, lane := range lanes {
		previous, seen := known[lane.ThreadID]
		// A lane whose projection still matches its cursor has nothing new.
		if seen && previous == projected[lane.ThreadID] {
			lane.Unchanged = true
			next[lane.ThreadID] = previous
			continue
		}
		complete := false
		if time.Now().Before(deadline) {
			if complete, err = service.IndexCodexThread(ctx, storeID, lane.ThreadID, deadline); err != nil {
				return Result{}, err
			}
		}
		var after, generation *int64
		if seen {
			after, generation = &previous.indexed, &previous.generation
		}
		readGeneration, indexed, _, _, records, found, effectiveAfter, err := service.loadOutlineRecords(ctx, storeID, lane.ThreadID, after, generation)
		if err != nil {
			return Result{}, err
		}
		// A new generation (or a lane the client never saw) is read whole.
		lane.Reset = delta && seen && effectiveAfter == nil
		lane.Partial = !complete || !found
		partial = partial || lane.Partial
		lane.Turns, lane.Tools, lane.Messages = codexTimelineLane(records)
		if found {
			next[lane.ThreadID] = codexLaneCursor{readGeneration, indexed}
		}
	}
	if partial {
		service.WakeConversationIndexer()
	}
	ordered := orderedLanes(lanes)
	return Result{Status: 200, Body: map[string]any{"engine": "codex", "rootThreadId": threadID, "lanes": ordered, "partial": partial,
		"truncated": len(lanes) == timelineMaxLanes, "delta": delta, "cursor": formatCodexTimelineCursor(ordered, next)}}, nil
}

// orderedLanes places each child directly beneath its parent (depth-first).
func orderedLanes(lanes []*timelineLane) []*timelineLane {
	children := map[string][]*timelineLane{}
	ids := map[string]bool{}
	for _, lane := range lanes {
		ids[lane.ThreadID] = true
	}
	roots := []*timelineLane{}
	for _, lane := range lanes {
		if lane.ParentThreadID != nil && ids[*lane.ParentThreadID] && lane.Depth > 0 {
			children[*lane.ParentThreadID] = append(children[*lane.ParentThreadID], lane)
		} else {
			roots = append(roots, lane)
		}
	}
	firstActivity := func(lane *timelineLane) int64 {
		if len(lane.Turns) > 0 {
			return lane.Turns[0].Start
		}
		if len(lane.Messages) > 0 {
			return lane.Messages[0].At
		}
		return 0
	}
	result := []*timelineLane{}
	var visit func(*timelineLane)
	visit = func(lane *timelineLane) {
		result = append(result, lane)
		list := children[lane.ThreadID]
		sort.SliceStable(list, func(i, j int) bool { return firstActivity(list[i]) < firstActivity(list[j]) })
		for _, child := range list {
			visit(child)
		}
	}
	for _, root := range roots {
		visit(root)
	}
	return result
}

// GetClaudeTimeline derives lanes from a Claude session's durable events:
// turns from Mira turn records, tools from tool_use/tool_result pairs, and
// one lane per native subagent (grouped by its parent Task tool call). With a
// cursor it reads only later events; root turns are always complete (a few
// small rows), and a result for an earlier tool becomes a Close item.
func (service *Service) GetClaudeTimeline(ctx context.Context, sessionID, cursor string) (Result, error) {
	after, delta := int64(0), false
	if raw, ok := strings.CutPrefix(cursor, "c1."); ok {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value >= 0 {
			after, delta = value, true
		}
	}
	var title string
	if err := service.pool.QueryRow(ctx, `SELECT title FROM mira_claude_sessions WHERE session_id=$1`, sessionID).Scan(&title); err != nil {
		return errorResult(404, "session not found", "not_found"), nil
	}
	root := &timelineLane{ThreadID: sessionID, Title: &title, Turns: []timelineTurn{}, Tools: []timelineTool{}, Messages: []timelineMessage{}}
	turnRows, err := service.pool.Query(ctx, `SELECT turn_id::text,created_at,completed_at,status FROM mira_claude_turns WHERE session_id=$1 ORDER BY created_at`, sessionID)
	if err != nil {
		return Result{}, err
	}
	for turnRows.Next() {
		var id, status string
		var created time.Time
		var completed *time.Time
		if err := turnRows.Scan(&id, &created, &completed, &status); err != nil {
			turnRows.Close()
			return Result{}, err
		}
		turn := timelineTurn{ID: id, Start: created.UnixMilli(), End: created.UnixMilli(), Open: completed == nil, Status: status}
		if completed != nil {
			turn.End = completed.UnixMilli()
		}
		root.Turns = append(root.Turns, turn)
	}
	turnRows.Close()
	if err := turnRows.Err(); err != nil {
		return Result{}, err
	}
	rows, err := service.pool.Query(ctx, `SELECT seq,turn_id::text,payload,cold_sha256 FROM mira_claude_events WHERE session_id=$1
	  AND seq>$2 AND event_type IN ('mira_user','assistant','user') ORDER BY seq`, sessionID, after)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	subagents := map[string]*timelineLane{}
	order := []*timelineLane{}
	toolsByID := map[string]*timelineTool{}
	laneTools := map[*timelineLane][]*timelineTool{}
	turnStart := map[string]int64{}
	for _, turn := range root.Turns {
		turnStart[turn.ID] = turn.Start
	}
	lastAt := int64(0)
	through := after
	closes := map[*timelineLane][]timelineTool{}
	for rows.Next() {
		var sequence int64
		var turnID, cold *string
		var raw []byte
		if err := rows.Scan(&sequence, &turnID, &raw, &cold); err != nil {
			return Result{}, err
		}
		through = max(through, sequence)
		// Frozen tool results carry the tool end time; read their original bytes.
		if raw, err = coldstore.Hydrate(ctx, raw, cold); err != nil {
			return Result{}, err
		}
		event, err := decodeObject(raw)
		if err != nil {
			continue
		}
		at, ok := millis(isoTimestamp(event["timestamp"], 1))
		if !ok && turnID != nil {
			at, ok = turnStart[*turnID]
		}
		if !ok {
			continue
		}
		lastAt = max(lastAt, at)
		lane := root
		if parent := stringValue(event["parent_tool_use_id"]); parent != "" {
			if subagents[parent] == nil {
				// An incremental read may not see the Task call; the client
				// keeps (or derives) the title of a lane without one.
				var title *string
				if tool := toolsByID[parent]; tool != nil && tool.Name != "" {
					title = &tool.Name
				} else if !delta {
					name := "子 Agent"
					title = &name
				}
				id := sessionID + ":" + parent
				subagents[parent] = &timelineLane{ThreadID: id, ParentThreadID: &sessionID, Title: title, Depth: 1, Turns: []timelineTurn{}, Tools: []timelineTool{}, Messages: []timelineMessage{}}
				order = append(order, subagents[parent])
			}
			lane = subagents[parent]
			if len(lane.Turns) == 0 {
				lane.Turns = append(lane.Turns, timelineTurn{ID: parent, Start: at, End: at})
			}
			lane.Turns[0].End = max(lane.Turns[0].End, at)
		}
		currentTurn := ""
		if turnID != nil {
			currentTurn = *turnID
		}
		switch stringValue(event["type"]) {
		case "mira_user":
			root.Messages = append(root.Messages, timelineMessage{At: at, Seq: sequence, Role: "user", Preview: previewText(stringValue(event["text"]))})
		case "assistant":
			for _, block := range array(object(event["message"])["content"]) {
				item := object(block)
				switch stringValue(item["type"]) {
				case "text":
					if lane == root && strings.TrimSpace(stringValue(item["text"])) != "" {
						root.Messages = append(root.Messages, timelineMessage{At: at, Seq: sequence, Role: "assistant", Preview: previewText(stringValue(item["text"]))})
					}
				case "tool_use":
					name := stringValue(item["name"])
					input := object(item["input"])
					if name == "Task" || name == "Agent" {
						if description := stringValue(first(input["description"], input["subagent_type"])); description != "" {
							name = name + " · " + description
						}
					}
					tool := &timelineTool{ID: stringValue(item["id"]), Name: name, Start: at, End: at, Open: true, Kind: toolKind(stringValue(item["name"])), Seq: sequence, TurnID: currentTurn}
					toolsByID[tool.ID] = tool
					laneTools[lane] = append(laneTools[lane], tool)
				}
			}
		case "user":
			for _, block := range array(object(event["message"])["content"]) {
				item := object(block)
				if stringValue(item["type"]) == "tool_result" {
					if tool := toolsByID[stringValue(item["tool_use_id"])]; tool != nil {
						tool.End, tool.Open = max(tool.End, at), false
					} else if delta && stringValue(item["tool_use_id"]) != "" {
						closes[lane] = append(closes[lane], timelineTool{ID: stringValue(item["tool_use_id"]), Start: at, End: at, Close: true, Seq: sequence})
					}
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return Result{}, err
	}
	for index := range root.Turns {
		if root.Turns[index].Open {
			root.Turns[index].End = max(root.Turns[index].End, lastAt)
		}
	}
	lanes := append([]*timelineLane{root}, order...)
	for _, lane := range lanes {
		for _, tool := range laneTools[lane] {
			lane.Tools = append(lane.Tools, *tool)
		}
		lane.Tools = append(lane.Tools, closes[lane]...)
	}
	return Result{Status: 200, Body: map[string]any{"engine": "claude", "rootThreadId": sessionID, "lanes": lanes, "partial": false, "truncated": false,
		"delta": delta, "cursor": "c1." + strconv.FormatInt(through, 10)}}, nil
}

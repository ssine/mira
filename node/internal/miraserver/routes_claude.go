package miraserver

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/clauderuntime"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func claudeError(status int, message string) error {
	return &HTTPError{Status: status, Code: "claude_session", Message: message}
}
func claudeString(body map[string]any, key string) string { v, _ := body[key].(string); return v }
func claudeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

type claudeSession struct {
	ID          string    `json:"sessionId"`
	NodeID      string    `json:"nodeId"`
	Cwd         string    `json:"cwd"`
	Title       string    `json:"title"`
	Model       string    `json:"model"`
	Effort      string    `json:"effort"`
	AccountID   string    `json:"nodeAccountId"`
	Archived    bool      `json:"archived"`
	Revision    int64     `json:"revision"`
	ActiveTurn  *string   `json:"activeTurn"`
	Persistence string    `json:"persistence"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

const claudeColumns = `session_id::text,node_id::text,cwd,title,model,effort,archived,revision,active_turn::text,persistence,created_at,updated_at,COALESCE(node_account_id::text,'')`

func scanClaude(row pgx.Row) (claudeSession, error) {
	var s claudeSession
	err := row.Scan(&s.ID, &s.NodeID, &s.Cwd, &s.Title, &s.Model, &s.Effort, &s.Archived, &s.Revision, &s.ActiveTurn, &s.Persistence, &s.CreatedAt, &s.UpdatedAt, &s.AccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = claudeError(404, "Claude session not found")
	}
	return s, err
}
func claudeCwd(value, platform string) bool {
	if value == "" || len(value) > 4096 {
		return false
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return false
		}
	}
	if platform == "windows" {
		return strings.HasPrefix(value, `\\`) || len(value) > 2 && value[1] == ':' && (value[2] == '/' || value[2] == '\\')
	}
	return strings.HasPrefix(value, "/")
}

func (server *Server) routeClaude(ctx context.Context, w http.ResponseWriter, r *http.Request) (bool, error) {
	if !strings.HasPrefix(r.URL.Path, "/v1/claude/") {
		return false, nil
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/claude/"), "/")
	scope := "admin"
	if len(parts) == 3 && parts[0] == "sessions" && (parts[2] == "entries" || parts[2] == "subkeys" || parts[2] == "events" && r.Method == "POST") {
		scope = "node"
	}
	actor, err := server.authorize(ctx, w, r, scope, authOptions{ClientType: "cli", CSRF: scope == "admin"})
	if err != nil || actor == nil {
		return true, err
	}
	if r.Method == "GET" && len(parts) == 2 && parts[0] == "accounts" && parts[1] == "cost-history" {
		result, err := server.claudeAccountCosts(ctx, r)
		if err != nil {
			return true, err
		}
		return true, writeJSON(w, 200, result)
	}
	if r.Method == "GET" && parts[0] == "conversations" {
		var result map[string]any
		if len(parts) == 1 {
			result, err = server.claudeConversationList(ctx, r)
		} else if len(parts) == 2 && claudeUUID(parts[1]) {
			result, err = server.claudeConversation(ctx, parts[1])
		} else {
			return true, claudeError(404, "Unknown conversation")
		}
		if err != nil {
			return true, err
		}
		return true, writeJSON(w, 200, result)
	}
	if len(parts) > 0 && parts[0] == "accounts" {
		return true, server.routeClaudeAccounts(ctx, w, r, actor, parts)
	}
	if len(parts) == 3 && parts[0] == "runtimes" && claudeUUID(parts[1]) && r.Method == "POST" {
		action := parts[2]
		if action != "prepare" && action != "status" && action != "describe" && action != "cache-status" && action != "cache-configure" {
			return true, claudeError(404, "Unknown runtime action")
		}
		node, err := server.nodes.Get(ctx, parts[1], false)
		if err != nil {
			return true, err
		}
		if node == nil || node.Capabilities["claudeRuntimeV1"] != true {
			return true, claudeError(409, "Node does not support managed Claude")
		}
		body, err := server.readBody(r)
		if err != nil {
			return true, err
		}
		if action == "cache-status" || action == "cache-configure" {
			if node.Capabilities["claudeSessionCacheV1"] != true {
				return true, claudeError(409, "Upgrade this Node to configure the Claude session cache")
			}
			params := map[string]any{"action": action}
			if action == "cache-configure" {
				limit, ok := body["maxBytes"].(float64)
				if !ok || limit < 0 || limit > 9007199254740991 || limit != math.Trunc(limit) {
					return true, claudeError(400, "maxBytes must be a nonnegative safe integer")
				}
				params["maxBytes"] = limit
			}
			result, err := server.channel.Invoke(ctx, parts[1], "claude", params, 20*time.Second)
			if err != nil {
				return true, err
			}
			return true, writeJSON(w, 200, result)
		}
		accountID := claudeString(body, "nodeAccountId")
		if accountID != "" {
			var valid bool
			if !claudeUUID(accountID) {
				return true, claudeError(400, "Invalid Claude account")
			}
			if err = server.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_claude_accounts WHERE node_account_id=$1 AND node_id=$2 AND enabled AND configured)`, accountID, parts[1]).Scan(&valid); err != nil {
				return true, err
			}
			if !valid {
				return true, claudeError(409, "Claude account is unavailable on this Node")
			}
		}
		result, err := server.channel.Invoke(ctx, parts[1], "claude", map[string]any{"action": action, "nodeAccountId": accountID}, 30*time.Second)
		if err != nil {
			return true, err
		}
		return true, writeJSON(w, 200, result)
	}
	if len(parts) == 1 && parts[0] == "sessions" {
		switch r.Method {
		case "GET":
			limit := 50
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			if offset < 0 {
				return true, claudeError(400, "Invalid offset")
			}
			rows, err := server.pool.Query(ctx, `SELECT `+claudeColumns+` FROM mira_claude_sessions WHERE archived=$1 ORDER BY updated_at DESC,session_id LIMIT $2 OFFSET $3`, r.URL.Query().Get("archived") == "1", limit+1, offset)
			if err != nil {
				return true, err
			}
			defer rows.Close()
			list := []claudeSession{}
			for rows.Next() {
				s, err := scanClaude(rows)
				if err != nil {
					return true, err
				}
				list = append(list, s)
			}
			if err = rows.Err(); err != nil {
				return true, err
			}
			var next any
			if len(list) > limit {
				list = list[:limit]
				next = offset + limit
			}
			return true, writeJSON(w, 200, map[string]any{"data": list, "nextOffset": next})
		case "POST":
			body, err := server.readBody(r)
			if err != nil {
				return true, err
			}
			requestID, nodeID, cwd := claudeString(body, "requestId"), claudeString(body, "nodeId"), claudeString(body, "cwd")
			if !claudeUUID(requestID) || !claudeUUID(nodeID) || cwd == "" || strings.ContainsRune(cwd, 0) {
				return true, claudeError(400, "requestId, nodeId and an absolute cwd are required")
			}
			node, err := server.nodes.Get(ctx, nodeID, false)
			if err != nil {
				return true, err
			}
			if node == nil || node.ApprovalStatus != "approved" || node.Capabilities["claudeRuntimeV1"] != true {
				return true, claudeError(409, "Choose an approved Claude-capable Node")
			}
			encoded, _ := json.Marshal(body)
			digest := sha256.Sum256(encoded)
			creationDigest := hex.EncodeToString(digest[:])
			if !claudeCwd(cwd, node.Platform) {
				return true, claudeError(400, "cwd must be an absolute path on the execution Node")
			}
			id, _ := randomUUID()
			accountID := claudeString(body, "nodeAccountId")
			if accountID != "" {
				var valid bool
				if !claudeUUID(accountID) {
					return true, claudeError(400, "Invalid Claude account")
				}
				if err = server.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_claude_accounts WHERE node_account_id=$1 AND node_id=$2)`, accountID, nodeID).Scan(&valid); err != nil {
					return true, err
				}
				if !valid {
					return true, claudeError(409, "Claude account is unavailable on this Node")
				}
			}
			title := strings.ReplaceAll(claudeString(body, "title"), "\x00", "")
			if len(title) > 512 {
				title = string([]rune(title)[:min(128, len([]rune(title)))])
			}
			_, err = server.pool.Exec(ctx, `INSERT INTO mira_claude_sessions(session_id,request_id,node_id,cwd,title,model,create_digest,node_account_id) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,'')::uuid) ON CONFLICT(request_id) DO NOTHING`, id, requestID, nodeID, cwd, title, claudeString(body, "model"), creationDigest, accountID)
			if err != nil {
				return true, err
			}
			s, err := scanClaude(server.pool.QueryRow(ctx, `SELECT `+claudeColumns+` FROM mira_claude_sessions WHERE request_id=$1`, requestID))
			if err != nil {
				return true, err
			}
			var storedDigest string
			if err = server.pool.QueryRow(ctx, `SELECT create_digest FROM mira_claude_sessions WHERE session_id=$1`, s.ID).Scan(&storedDigest); err != nil {
				return true, err
			}
			if storedDigest != creationDigest {
				return true, claudeError(409, "Creation request ID was already used with different parameters")
			}
			return true, writeJSON(w, 200, s)
		}
	}
	if len(parts) < 2 || parts[0] != "sessions" || !claudeUUID(parts[1]) {
		return true, claudeError(404, "Unknown Claude route")
	}
	id := parts[1]
	op := ""
	if len(parts) == 3 {
		op = parts[2]
	} else if len(parts) != 2 {
		return true, claudeError(404, "Unknown Claude route")
	}
	if scope == "node" {
		return true, server.claudeStorage(ctx, w, r, actor, id, op)
	}
	s, err := scanClaude(server.pool.QueryRow(ctx, `SELECT `+claudeColumns+` FROM mira_claude_sessions WHERE session_id=$1`, id))
	if err != nil {
		return true, err
	}
	if op == "" && r.Method == "GET" {
		return true, writeJSON(w, 200, s)
	}
	if op == "" && r.Method == "PATCH" {
		b, err := server.readBody(r)
		if err != nil {
			return true, err
		}
		title := s.Title
		if v, ok := b["title"].(string); ok {
			title = strings.ReplaceAll(v, "\x00", "")
			if len(title) > 1024 {
				return true, claudeError(400, "Title is too long")
			}
		}
		archived := s.Archived
		if v, ok := b["archived"].(bool); ok {
			archived = v
		}
		_, err = server.pool.Exec(ctx, `UPDATE mira_claude_sessions SET title=$2,archived=$3,updated_at=now() WHERE session_id=$1`, id, title, archived)
		if err != nil {
			return true, err
		}
		return true, writeJSON(w, 200, map[string]any{"ok": true})
	}
	if (op == "events" || op == "history") && r.Method == "GET" {
		cursor, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		if cursor < 0 {
			return true, claudeError(400, "Invalid cursor")
		}
		backward := r.URL.Query().Get("view") == "transcript"
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		if before < 0 {
			return true, claudeError(400, "Invalid history cursor")
		}
		query := `SELECT seq,payload,turn_id::text FROM mira_claude_events WHERE session_id=$1 AND seq>$2 ORDER BY seq LIMIT 200`
		args := []any{id, cursor}
		if op == "history" {
			query = `SELECT seq,payload,NULL::text FROM mira_claude_entries WHERE session_id=$1 AND seq>$2 AND subpath=$3 ORDER BY seq LIMIT 200`
			args = append(args, r.URL.Query().Get("subpath"))
		}
		if backward {
			args = []any{id, before}
			query = `SELECT seq,payload,turn_id::text FROM mira_claude_events WHERE session_id=$1 AND ($2::bigint=0 OR seq<$2) AND event_type<>'stream_event' ORDER BY seq DESC LIMIT 200`
			if op == "history" {
				query = `SELECT seq,payload,NULL::text FROM mira_claude_entries WHERE session_id=$1 AND ($2::bigint=0 OR seq<$2) AND subpath=$3 ORDER BY seq DESC LIMIT 200`
				args = append(args, r.URL.Query().Get("subpath"))
			}
		}
		rows, err := server.pool.Query(ctx, query, args...)
		if err != nil {
			return true, err
		}
		defer rows.Close()
		data := []any{}
		pageBytes := 0
		for rows.Next() {
			var seq int64
			var raw json.RawMessage
			var turnID *string
			if err = rows.Scan(&seq, &raw, &turnID); err != nil {
				return true, err
			}
			if len(data) > 0 && pageBytes+len(raw) > 4*1024*1024 {
				break
			}
			pageBytes += len(raw)
			data = append(data, map[string]any{"seq": seq, "payload": raw, "turnId": turnID})
			cursor = seq
		}
		if err = rows.Err(); err != nil {
			return true, err
		}
		var earliest int64
		if backward && len(data) > 0 {
			cursor = data[0].(map[string]any)["seq"].(int64)
			earliest = data[len(data)-1].(map[string]any)["seq"].(int64)
			for i, j := 0, len(data)-1; i < j; i, j = i+1, j-1 {
				data[i], data[j] = data[j], data[i]
			}
		}
		return true, writeJSON(w, 200, map[string]any{"data": data, "cursor": cursor, "earliest": earliest, "session": s, "hasMore": len(data) > 0})
	}
	if op == "costs" && r.Method == "GET" {
		ids := r.URL.Query()["turnId"]
		if len(ids) > 200 {
			return true, claudeError(400, "Too many turn IDs")
		}
		for _, id := range ids {
			if !claudeUUID(id) {
				return true, claudeError(400, "Invalid turn ID")
			}
		}
		rows, err := server.pool.Query(ctx, `SELECT turn_id::text,cost_delta::float8,cost_partial FROM mira_claude_usage_deltas WHERE session_id=$1 AND turn_id=ANY($2::uuid[])`, s.ID, ids)
		if err != nil {
			return true, err
		}
		defer rows.Close()
		estimates := map[string]any{}
		for rows.Next() {
			var id string
			var amount *float64
			var partial bool
			if err = rows.Scan(&id, &amount, &partial); err != nil {
				return true, err
			}
			estimates[id] = claudeEstimate(amount, partial)
		}
		if err = rows.Err(); err != nil {
			return true, err
		}
		if s.ActiveTurn != nil && slices.Contains(ids, *s.ActiveTurn) {
			live, err := server.claudeLiveUsage(ctx, s)
			if err != nil {
				return true, err
			}
			if live != nil {
				estimates[*s.ActiveTurn] = claudeLiveEstimate(&live.amount, live.unpriced)
			}
		}
		return true, writeJSON(w, 200, map[string]any{"generation": 1, "turnCostEstimates": estimates})
	}
	if op == "children" && r.Method == "GET" {
		rows, err := server.pool.Query(ctx, `SELECT thread_id::text,subpath,parent_thread_id::text FROM mira_claude_transcripts WHERE session_id=$1 AND subpath<>'' ORDER BY subpath`, id)
		if err != nil {
			return true, err
		}
		defer rows.Close()
		data := []any{}
		for rows.Next() {
			var child, key string
			var parent *string
			if err = rows.Scan(&child, &key, &parent); err != nil {
				return true, err
			}
			data = append(data, map[string]any{"threadId": child, "subpath": key, "parentThreadId": parent, "rootThreadId": id, "sourceKind": "claude_subagent"})
		}
		if err = rows.Err(); err != nil {
			return true, err
		}
		return true, writeJSON(w, 200, map[string]any{"data": data})
	}
	if op == "turns" && r.Method == "POST" {
		return true, server.claudeStartTurn(ctx, w, r, s)
	}
	if op == "answer" && r.Method == "POST" {
		if s.ActiveTurn == nil {
			return true, claudeError(409, "No active question")
		}
		body, err := server.readBody(r)
		if err != nil {
			return true, err
		}
		questionID := claudeString(body, "questionId")
		if !claudeUUID(questionID) {
			return true, claudeError(400, "questionId is required")
		}
		var pending bool
		if err = server.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_claude_events WHERE event_id=$1 AND session_id=$2 AND turn_id=$3 AND event_type='mira_question')`, questionID, id, *s.ActiveTurn).Scan(&pending); err != nil {
			return true, err
		}
		if !pending {
			return true, claudeError(409, "Question does not belong to the active turn")
		}
		result, err := server.channel.Invoke(ctx, s.NodeID, "claude", map[string]any{"action": "answer", "turnId": *s.ActiveTurn, "questionId": body["questionId"], "answers": body["answers"]}, 30*time.Second)
		if err != nil {
			return true, err
		}
		return true, writeJSON(w, 200, result)
	}
	if op == "steer" && r.Method == "POST" {
		return true, server.claudeSteer(ctx, w, r, s)
	}
	if op == "interrupt" && r.Method == "POST" {
		if s.ActiveTurn == nil {
			return true, writeJSON(w, 200, map[string]any{"stopped": true})
		}
		result, err := server.channel.Invoke(ctx, s.NodeID, "claude", map[string]any{"action": "interrupt", "turnId": *s.ActiveTurn}, 30*time.Second)
		if err != nil {
			return true, err
		}
		return true, writeJSON(w, 200, result)
	}
	if op == "reconcile" && r.Method == "POST" {
		if s.ActiveTurn == nil {
			return true, writeJSON(w, 200, s)
		}
		result, err := server.channel.Invoke(ctx, s.NodeID, "claude", map[string]any{"action": "status", "turnId": *s.ActiveTurn}, 30*time.Second)
		if err != nil {
			return true, err
		}
		state, _ := result.(map[string]any)
		var runtimeID string
		if err = server.pool.QueryRow(ctx, `SELECT runtime_id FROM mira_claude_turns WHERE turn_id=$1`, *s.ActiveTurn).Scan(&runtimeID); err != nil {
			return true, err
		}
		if runtimeID == "" || state["runtimeId"] != runtimeID {
			return true, claudeError(409, "The owning Node runtime restarted; its empty worker list cannot prove the old turn stopped. Execution state remains unknown.")
		}
		if state["active"] == false {
			tx, err := server.pool.Begin(ctx)
			if err != nil {
				return true, err
			}
			defer tx.Rollback(ctx)
			_, err = tx.Exec(ctx, `UPDATE mira_claude_sessions SET active_turn=NULL,persistence='incomplete',updated_at=now() WHERE session_id=$1 AND active_turn=$2`, id, *s.ActiveTurn)
			if err != nil {
				return true, err
			}
			_, err = tx.Exec(ctx, `UPDATE mira_claude_turns SET status='failed',error='Runtime ended without an acknowledged completion',completed_at=now() WHERE turn_id=$1 AND completed_at IS NULL`, *s.ActiveTurn)
			if err != nil {
				return true, err
			}
			if err = tx.Commit(ctx); err != nil {
				return true, err
			}
		}
		return true, writeJSON(w, 200, state)
	}
	return true, claudeError(404, "Unknown Claude operation")
}

// claudeSteer adds a message to the running turn. The request ID is the steer
// ID: the worker stores its mira_steer event under that ID before queueing, and
// a later mira_steer_rejected withdraws it. A lost reply is answered from those
// records once the turn has ended, never by dispatching the message again.
func (server *Server) claudeSteer(ctx context.Context, w http.ResponseWriter, r *http.Request, s claudeSession) error {
	b, err := server.readBody(r)
	if err != nil {
		return err
	}
	steerID := claudeString(b, "requestId")
	expected := claudeString(b, "expectedTurnId")
	text := claudeString(b, "text")
	if !claudeUUID(steerID) || !claudeUUID(expected) || strings.TrimSpace(text) == "" {
		return claudeError(400, "requestId, expectedTurnId and text are required")
	}
	if len(text) > 4*1024*1024 {
		return claudeError(400, "Message exceeds 4 MiB")
	}
	notSteerable := &HTTPError{Status: 409, Code: "turn_not_steerable", Message: "Claude 本轮已结束，请作为新一轮发送"}
	var turn string
	var seq int64
	err = server.pool.QueryRow(ctx, `SELECT turn_id::text,seq FROM mira_claude_events WHERE event_id=$1 AND session_id=$2 AND event_type='mira_steer'`, steerID, s.ID).Scan(&turn, &seq)
	if err == nil {
		var rejected bool
		if err = server.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_claude_events WHERE session_id=$1 AND turn_id=$2 AND seq>$3 AND event_type='mira_steer_rejected' AND payload->>'steerId'=$4)`, s.ID, turn, seq, steerID).Scan(&rejected); err != nil {
			return err
		}
		if rejected {
			return notSteerable
		}
		if s.ActiveTurn == nil || *s.ActiveTurn != turn {
			return writeJSON(w, 200, map[string]any{"accepted": true, "turnId": turn, "replayed": true})
		}
		// The worker still owns the turn and reports the verdict it already reached.
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if s.ActiveTurn == nil || *s.ActiveTurn != expected {
		return notSteerable
	}
	node, err := server.nodes.Get(ctx, s.NodeID, false)
	if err != nil {
		return err
	}
	if node == nil || node.Capabilities["claudeSteerV1"] != true {
		return &HTTPError{Status: 409, Code: "steer_unsupported", Message: "请先升级此执行节点，才能在 Claude 运行时追加消息"}
	}
	result, err := server.channel.Invoke(ctx, s.NodeID, "claude", map[string]any{"action": "steer", "turnId": expected, "steerId": steerID, "text": text, "attachments": b["attachments"]}, 30*time.Second)
	if err != nil {
		return err
	}
	verdict, _ := result.(map[string]any)
	if verdict["accepted"] == true {
		return writeJSON(w, 200, map[string]any{"accepted": true, "turnId": expected})
	}
	switch verdict["reason"] {
	case "turn_finishing":
		return notSteerable
	case "invalid_input":
		message, _ := verdict["message"].(string)
		return claudeError(400, "Claude 无法读取这条消息："+message)
	}
	// The worker keeps this verdict for the request; once the turn ends, a retry
	// finds the recorded rejection and the Web sends the message as a new turn.
	return claudeError(503, "Claude 未能记录这条消息，它不会加入本轮；本轮结束后重试会作为新一轮发送")
}

// Mira collapses tool calls and thinking, so text between tool calls is what
// the user follows during a long turn. The Developer Message file comes later
// and may override this.
const claudeProgressInstructions = "Mira shows the user the text you write between tool calls as progress updates; tool calls and thinking are collapsed. " +
	"During long work, write a brief update when there is something worth reporting, such as a finding, a finished step, or a change of plan, " +
	"typically every several tool calls rather than before each one. Write each update for the user, in the user's language, " +
	"as one to three self-contained sentences saying what you did or found and what comes next. Do not write notes to yourself there. " +
	"Your last message in a turn is the answer.\n"

func (server *Server) claudeStartTurn(ctx context.Context, w http.ResponseWriter, r *http.Request, s claudeSession) error {
	b, err := server.readBody(r)
	if err != nil {
		return err
	}
	turnID := claudeString(b, "requestId")
	text := claudeString(b, "text")
	nodeID := claudeString(b, "nodeId")
	if nodeID == "" {
		nodeID = s.NodeID
	}
	if !claudeUUID(turnID) || !claudeUUID(nodeID) || strings.TrimSpace(text) == "" {
		return claudeError(400, "requestId, nodeId and text are required")
	}
	if len(text) > 4*1024*1024 {
		return claudeError(400, "Message exceeds 4 MiB")
	}
	request, _ := json.Marshal(b)
	var previous []byte
	var owner string
	replayErr := server.pool.QueryRow(ctx, `SELECT request,session_id::text FROM mira_claude_turns WHERE turn_id=$1`, turnID).Scan(&previous, &owner)
	if replayErr == nil {
		if owner != s.ID || string(previous) != string(request) {
			return claudeError(409, "Turn request ID already used")
		}
		return writeJSON(w, 200, map[string]any{"turnId": turnID, "replayed": true})
	}
	if !errors.Is(replayErr, pgx.ErrNoRows) {
		return replayErr
	}
	node, err := server.nodes.Get(ctx, nodeID, false)
	if err != nil {
		return err
	}
	if node == nil || node.ApprovalStatus != "approved" || node.Capabilities["claudeRuntimeV1"] != true || !server.channel.IsConnected(nodeID) {
		return claudeError(409, "Execution Node is unavailable")
	}
	cwd := claudeString(b, "cwd")
	if cwd == "" {
		cwd = s.Cwd
	}
	if !claudeCwd(cwd, node.Platform) {
		return claudeError(400, "Choose an absolute workspace path compatible with the execution Node")
	}
	status, err := server.channel.Invoke(ctx, nodeID, "claude", map[string]any{"action": "status"}, 30*time.Second)
	if err != nil {
		return err
	}
	state, _ := status.(map[string]any)
	if state["status"] != "ready" {
		return claudeError(409, "Prepare the Claude runtime before sending")
	}
	instructions, err := server.channel.RuntimeInstructions(ctx, nodeID)
	if err != nil {
		return err
	}
	instructions = "You are running in Mira on a trusted execution Node. Use home_nodes MCP tools for other authorized devices.\n" + claudeProgressInstructions + instructions
	tx, err := server.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	s, err = scanClaude(tx.QueryRow(ctx, `SELECT `+claudeColumns+` FROM mira_claude_sessions WHERE session_id=$1 FOR UPDATE`, s.ID))
	if err != nil {
		return err
	}
	var prior []byte
	var priorSession string
	err = tx.QueryRow(ctx, `SELECT request,session_id::text FROM mira_claude_turns WHERE turn_id=$1`, turnID).Scan(&prior, &priorSession)
	if err == nil {
		if priorSession != s.ID || string(prior) != string(request) {
			return claudeError(409, "Turn request ID already used")
		}
		return writeJSON(w, 200, map[string]any{"turnId": turnID, "replayed": true})
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if s.ActiveTurn != nil {
		return claudeError(409, "A Claude turn is already active; stop it before starting another")
	}

	var count int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM mira_claude_entries WHERE session_id=$1 AND subpath=''`, s.ID).Scan(&count)
	if err != nil {
		return err
	}
	if count == 0 {
		var priorTurns bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_claude_turns WHERE session_id=$1)`, s.ID).Scan(&priorTurns); err != nil {
			return err
		}
		if priorTurns {
			return claudeError(409, "No acknowledged native Claude history is available. Start a new conversation instead of falling back to local history.")
		}
	}
	if s.Persistence == "incomplete" && b["continueAcknowledgedHistory"] != true {
		return claudeError(409, "Claude history is incomplete. Inspect the saved history before explicitly continuing with acknowledged history.")
	}
	revision := s.Revision + 1
	accountID := s.AccountID
	if raw, present := b["nodeAccountId"]; present {
		var valid bool
		accountID, valid = raw.(string)
		if !valid {
			return claudeError(400, "Invalid Claude account")
		}
	}
	account, err := claudeAccountForTurn(ctx, tx, nodeID, accountID)
	if err != nil {
		return err
	}
	if accountID != "" {
		if node.Capabilities["claudeAccountsV1"] != true {
			return claudeError(409, "Upgrade this Node to use managed Claude accounts")
		}
		if _, err = server.channel.Invoke(ctx, nodeID, "claude", map[string]any{"action": "status", "nodeAccountId": accountID}, 30*time.Second); err != nil {
			return err
		}
	}
	model := claudeString(b, "model")
	if _, provided := b["model"]; !provided && accountID == s.AccountID {
		model = s.Model
	}
	if model == "" {
		model = account.Model
	}
	effort := claudeString(b, "effort")
	if _, provided := b["effort"]; !provided {
		effort = s.Effort
	}
	if effort == "" && model == account.Model {
		effort = account.Effort
	}
	if !clauderuntime.ValidEffort(effort) {
		return claudeError(400, "Invalid effort")
	}

	_, err = tx.Exec(ctx, `INSERT INTO mira_claude_turns(turn_id,session_id,node_id,revision,request,runtime_id,node_account_id,account_name)
 VALUES($1,$2,$3,$4,$5::json,$6,NULLIF($7,'')::uuid,(SELECT name FROM mira_claude_accounts WHERE node_account_id=NULLIF($7,'')::uuid))`, turnID, s.ID, nodeID, revision, request, state["runtimeId"], accountID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE mira_claude_sessions SET active_turn=$2,node_id=$3,revision=$4,model=$5,effort=$6,cwd=$7,node_account_id=NULLIF($8,'')::uuid,persistence=CASE WHEN persistence='incomplete' THEN persistence ELSE 'pending' END,updated_at=now() WHERE session_id=$1`, s.ID, turnID, nodeID, revision, model, effort, cwd, accountID)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}

	params := map[string]any{"runtimeId": state["runtimeId"], "action": "start", "sessionId": s.ID, "turnId": turnID, "revision": revision, "cwd": cwd, "model": model, "effort": effort, "text": text, "attachments": b["attachments"], "resume": count > 0, "instructions": instructions, "nodeAccountId": accountID}
	result, err := server.channel.Invoke(ctx, nodeID, "claude", params, 30*time.Second)
	// A timeout is ambiguous. Keep the reservation until the owner reports exit;
	// neither an HTTP retry nor a reconnect may run the prompt again.
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"turnId": turnID, "runtime": result})
}

func (server *Server) claudeStorage(ctx context.Context, w http.ResponseWriter, r *http.Request, actor *foundation.Principal, id, op string) error {
	tx, err := server.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	s, err := scanClaude(tx.QueryRow(ctx, `SELECT `+claudeColumns+` FROM mira_claude_sessions WHERE session_id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	revision, _ := strconv.ParseInt(r.Header.Get("X-Mira-Claude-Revision"), 10, 64)
	turn := r.Header.Get("X-Mira-Claude-Turn")
	if s.NodeID != actor.NodeID || revision != s.Revision || !claudeUUID(turn) {
		return claudeError(409, "Stale or foreign Claude runtime")
	}
	var owns bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_claude_turns WHERE turn_id=$1 AND session_id=$2 AND node_id=$3 AND revision=$4)`, turn, id, actor.NodeID, revision).Scan(&owns); err != nil {
		return err
	}
	if !owns {
		return claudeError(403, "Runtime does not own this turn")
	}
	subpath := r.URL.Query().Get("subpath")
	if len(subpath) > 1024 || strings.ContainsAny(subpath, "\\\x00") || strings.HasPrefix(subpath, "/") || subpath != "" && (path.Clean(subpath) != subpath || subpath == ".." || strings.HasPrefix(subpath, "../")) {
		return claudeError(400, "Invalid native transcript subpath")
	}
	if op == "subkeys" && r.Method == "GET" {
		rows, err := tx.Query(ctx, `SELECT subpath FROM mira_claude_transcripts WHERE session_id=$1 AND subpath<>'' ORDER BY subpath`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		keys := []string{}
		for rows.Next() {
			var key string
			if err = rows.Scan(&key); err != nil {
				return err
			}
			keys = append(keys, key)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]any{"subkeys": keys})
	}
	if op == "entries" && r.Method == "GET" {
		var after int64
		if r.URL.Query().Get("cache") == "1" {
			var end int64
			if err = tx.QueryRow(ctx, `SELECT coalesce((SELECT next_seq-1 FROM mira_claude_transcripts WHERE session_id=$1 AND subpath=$2),0)`, id, subpath).Scan(&end); err != nil {
				return err
			}
			after, err = strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
			if err != nil || after < 0 {
				return claudeError(400, "Invalid native cache cursor")
			}
			// Raw records are immutable and contiguous. Validate the cached prefix
			// against PostgreSQL before returning only its missing suffix. A stale
			// cursor (including after a restore) falls back to the complete stream.
			if after > end {
				after = 0
			}
			if after > 0 {
				var raw []byte
				if err = tx.QueryRow(ctx, `SELECT payload FROM mira_claude_entries WHERE session_id=$1 AND subpath=$2 AND seq=$3`, id, subpath, after).Scan(&raw); err != nil {
					return err
				}
				digest := sha256.Sum256(raw)
				if hex.EncodeToString(digest[:]) != r.URL.Query().Get("prefix") {
					after = 0
				}
			}
			var prefix string
			if end > 0 {
				var raw []byte
				if err = tx.QueryRow(ctx, `SELECT payload FROM mira_claude_entries WHERE session_id=$1 AND subpath=$2 AND seq=$3`, id, subpath, end).Scan(&raw); err != nil {
					return err
				}
				digest := sha256.Sum256(raw)
				prefix = hex.EncodeToString(digest[:])
			}
			w.Header().Set("X-Mira-Claude-Cache-Version", "1")
			w.Header().Set("X-Mira-Claude-Cache-Start", strconv.FormatInt(after, 10))
			w.Header().Set("X-Mira-Claude-Cache-End", strconv.FormatInt(end, 10))
			w.Header().Set("X-Mira-Claude-Cache-Prefix", prefix)
		}
		rows, err := tx.Query(ctx, `SELECT payload FROM mira_claude_entries WHERE session_id=$1 AND subpath=$2 AND seq>$3 ORDER BY seq`, id, subpath, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		w.Header().Set("Content-Type", "application/x-ndjson")
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				return err
			}
			if _, err = w.Write(append(raw, '\n')); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if r.Method != "POST" {
		return claudeError(405, "Unsupported storage operation")
	}
	if op == "entries" {
		operation := r.URL.Query().Get("operationId")
		if !claudeUUID(operation) {
			return claudeError(400, "operationId is required")
		}
		child := id
		var parent any
		if subpath != "" {
			child, _ = randomUUID()
			// Native subpaths identify the root collection, not necessarily a direct parent.
			// Preserve unknown nested parentage in raw entries instead of inventing a tree.
		}
		_, err = tx.Exec(ctx, `INSERT INTO mira_claude_transcripts(session_id,subpath,thread_id,parent_thread_id,source_kind) VALUES($1,$2,$3,$4,CASE WHEN $2='' THEN 'claude_session' ELSE 'claude_subagent' END) ON CONFLICT(session_id,subpath) DO NOTHING`, id, subpath, child, parent)
		if err != nil {
			return err
		}
		hash := sha256.New()
		hash.Write([]byte(subpath + "\n"))
		scanner := bufio.NewScanner(io.TeeReader(r.Body, hash))
		scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
		var seq int64
		if err = tx.QueryRow(ctx, `SELECT next_seq FROM mira_claude_transcripts WHERE session_id=$1 AND subpath=$2`, id, subpath).Scan(&seq); err != nil {
			return err
		}
		for scanner.Scan() {
			raw := scanner.Bytes()
			var entry map[string]json.RawMessage
			if err = json.Unmarshal(raw, &entry); err != nil || entry == nil {
				return claudeError(400, "Invalid raw transcript entry")
			}
			var uuid string
			_ = json.Unmarshal(entry["uuid"], &uuid)
			var key any
			if uuid != "" {
				key = uuid
			}
			tag, err := tx.Exec(ctx, `INSERT INTO mira_claude_entries(session_id,subpath,seq,entry_uuid,payload) VALUES($1,$2,$3,$4,$5::json) ON CONFLICT(session_id,subpath,entry_uuid) DO NOTHING`, id, subpath, seq, key, raw)
			if err != nil {
				return err
			}
			seq += tag.RowsAffected()
		}
		if err = scanner.Err(); err != nil {
			return claudeError(400, "Unable to read transcript batch (maximum single record: 64 MiB)")
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		var previous, owner string
		err = tx.QueryRow(ctx, `SELECT digest,session_id::text FROM mira_claude_operations WHERE operation_id=$1`, operation).Scan(&previous, &owner)
		if err == nil {
			if previous != digest || owner != id {
				return claudeError(409, "Storage operation ID reused with different content")
			}
			return writeJSON(w, 200, map[string]any{"replayed": true})
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO mira_claude_operations VALUES($1,$2,$3)`, operation, id, digest)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE mira_claude_transcripts SET next_seq=$3 WHERE session_id=$1 AND subpath=$2`, id, subpath, seq)
		if err != nil {
			return err
		}
	} else if op == "events" {
		var b struct {
			EventID string          `json:"eventId"`
			Payload json.RawMessage `json:"payload"`
		}
		if err = foundation.ReadJSON(r, &b, server.config.Foundation.MaxBodyBytes); err != nil {
			return err
		}
		if !claudeUUID(b.EventID) || len(b.Payload) == 0 {
			return claudeError(400, "eventId and payload are required")
		}
		var event struct {
			Type        string `json:"type"`
			Subtype     string `json:"subtype"`
			Failed      bool   `json:"failed"`
			Degraded    bool   `json:"degraded"`
			Interrupted bool   `json:"interrupted"`
		}
		if err = json.Unmarshal(b.Payload, &event); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO mira_claude_events(event_id,session_id,turn_id,payload,event_type) VALUES($1,$2,$3,$4::json,$5) ON CONFLICT(event_id) DO NOTHING`, b.EventID, id, turn, b.Payload, strings.ReplaceAll(event.Type, "\x00", ""))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var previous []byte
			var eventSession, eventTurn string
			if err = tx.QueryRow(ctx, `SELECT session_id::text,turn_id::text,payload FROM mira_claude_events WHERE event_id=$1`, b.EventID).Scan(&eventSession, &eventTurn, &previous); err != nil {
				return err
			}
			if eventSession != id || eventTurn != turn || string(previous) != string(b.Payload) {
				return claudeError(409, "Event ID reused with different content")
			}
		}
		if tag.RowsAffected() > 0 {
			if event.Type == "system" && event.Subtype == "mirror_error" || event.Degraded {
				_, err = tx.Exec(ctx, `UPDATE mira_claude_sessions SET persistence='incomplete' WHERE session_id=$1`, id)
				if err != nil {
					return err
				}
			}
			if event.Type == "mira_completed" {
				status := "completed"
				if event.Interrupted {
					status = "interrupted"
				}
				if event.Failed {
					status = "failed"
				}
				// Push and native notification queues currently resolve Codex
				// ThreadStore identities only. Claude completion must not depend
				// on whether the administrator has subscribed to those queues.
				_, err = tx.Exec(ctx, `UPDATE mira_claude_turns SET status=$2,completed_at=now() WHERE turn_id=$1`, turn, status)
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `UPDATE mira_claude_sessions SET active_turn=NULL,persistence=CASE WHEN persistence='incomplete' OR NOT EXISTS(SELECT 1 FROM mira_claude_entries WHERE session_id=$1 AND subpath='') THEN 'incomplete' ELSE 'saved' END,updated_at=now() WHERE session_id=$1 AND active_turn=$2`, id, turn)
				if err != nil {
					return err
				}
			} else {
				_, err = tx.Exec(ctx, `UPDATE mira_claude_turns SET status='running' WHERE turn_id=$1 AND completed_at IS NULL`, turn)
				if err != nil {
					return err
				}
			}
		}
	} else {
		return claudeError(404, fmt.Sprintf("Unknown storage operation %s", op))
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"ok": true})
}

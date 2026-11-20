package views

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	conversationMessagesDefault = 200
	conversationMessagesMax     = 1000
	conversationChildrenMax     = 200
)

// MessagesOptions pages one conversation read by item sequence.
type MessagesOptions struct {
	After int64
	Limit int
	// Tools reads canonical history: prose plus tool calls with their inputs
	// and outputs, each field clipped to ToolLimit bytes (0 = 1 MiB).
	Tools     bool
	ToolLimit int
}

type conversationChild struct {
	ThreadID string  `json:"threadId"`
	Title    *string `json:"title"`
}

// ReadConversationMessages returns one Codex thread or Claude conversation
// (root session or native subagent transcript), oldest first. Without Tools it
// reads user and assistant prose from the derived conversation index, which it
// advances on demand (complete=false: later prose may still be missing). With
// Tools it projects canonical history like the Web transcript, hydrating cold
// bodies and failing rather than returning a partial page. Reasoning is never
// included. nextCursor is the last item sequence read.
func (service *Service) ReadConversationMessages(ctx context.Context, storeID, threadID string, options MessagesOptions) (Result, error) {
	after := max(options.After, 0)
	body, found, err := service.describeCodexConversation(ctx, storeID, threadID, !options.Tools)
	if err != nil {
		return Result{}, err
	}
	if !found {
		body, found, err = service.describeClaudeConversation(ctx, threadID, !options.Tools)
		if err != nil {
			return Result{}, err
		}
	}
	if !found {
		return errorResult(404, "conversation not found", "not_found"), nil
	}
	engine, generation := body["engine"].(string), body["generation"].(int64)
	subpath, _ := body["subpath"].(string)
	delete(body, "subpath")
	var items []conversationItem
	var next *int64
	switch {
	case options.Tools && engine == "codex":
		items, next, err = service.readCodexItems(ctx, storeID, threadID, generation, after, options.Limit, options.ToolLimit)
	case options.Tools:
		items, next, err = service.readClaudeItems(ctx, body["sessionId"].(string), subpath, after, options.Limit, options.ToolLimit)
	default:
		items, next, err = service.readIndexedProse(ctx, engine, storeID, threadID, generation, after, options.Limit)
	}
	if errors.Is(err, errHistoryIncomplete) {
		return errorResult(409, "thread history is incomplete", "history_incomplete"), nil
	}
	if err != nil {
		return Result{}, err
	}
	body["items"], body["nextCursor"], body["tools"] = items, nil, options.Tools
	if next != nil {
		body["nextCursor"] = next
	}
	return Result{Status: 200, Body: body}, nil
}

func (service *Service) readIndexedProse(ctx context.Context, engine, storeID, threadID string, generation, after int64, limit int) ([]conversationItem, *int64, error) {
	if limit <= 0 {
		limit = conversationMessagesDefault
	}
	limit = min(limit, conversationMessagesMax)
	scope := storeID
	if engine == "claude" {
		scope = ""
	}
	rows, err := service.pool.Query(ctx, `SELECT item_seq,role,body,recorded_at FROM mira_conversation_messages
	  WHERE engine=$1 AND store_id=$2 AND thread_id=$3 AND generation=$4 AND item_seq>$5 ORDER BY item_seq LIMIT $6`,
		engine, scope, threadID, generation, after, limit+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := []conversationItem{}
	for rows.Next() {
		var item conversationItem
		var at *time.Time
		if err := rows.Scan(&item.Seq, &item.Kind, &item.Text, &at); err != nil {
			return nil, nil, err
		}
		item.RecordedAt = optionalTime(at)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(items) > limit {
		items = items[:limit]
		return items, &items[limit-1].Seq, nil
	}
	return items, nil, nil
}

func (service *Service) describeCodexConversation(ctx context.Context, storeID, threadID string, index bool) (map[string]any, bool, error) {
	var exists bool
	if err := service.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM codex_thread_projections p WHERE p.store_id=$1 AND p.thread_id=$2
	  AND NOT EXISTS(SELECT 1 FROM mira_thread_actions d WHERE d.store_id=p.store_id AND d.thread_id=p.thread_id AND d.action='delete'))`,
		storeID, threadID).Scan(&exists); err != nil || !exists {
		return nil, false, err
	}
	complete := true
	if index {
		var err error
		if complete, err = service.IndexCodexThread(ctx, storeID, threadID, time.Now().Add(outlineCatchUpBudget)); err != nil {
			return nil, false, err
		}
		if !complete {
			service.WakeConversationIndexer()
		}
	}
	var generation, itemCount int64
	var parent, title, cwd *string
	var updated time.Time
	var archived bool
	err := service.pool.QueryRow(ctx, `SELECT p.active_generation,p.item_count,p.parent_thread_id,COALESCE(NULLIF(n.value#>>'{}',''),p.title),p.cwd,p.updated_at,
	    COALESCE((SELECT action='archive' FROM mira_thread_actions a WHERE a.store_id=p.store_id AND a.thread_id=p.thread_id AND a.action IN ('archive','restore') ORDER BY action_seq DESC LIMIT 1),false)
	  FROM codex_thread_projections p
	  LEFT JOIN codex_store_state_entries n ON n.store_id=p.store_id AND n.field='names' AND NOT n.is_root AND n.entry_key=p.thread_id
	  WHERE p.store_id=$1 AND p.thread_id=$2`, storeID, threadID).Scan(&generation, &itemCount, &parent, &title, &cwd, &updated, &archived)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	children, err := service.conversationChildren(ctx, `SELECT p.thread_id,COALESCE(NULLIF(n.value#>>'{}',''),p.title) FROM codex_thread_projections p
	  LEFT JOIN codex_store_state_entries n ON n.store_id=p.store_id AND n.field='names' AND NOT n.is_root AND n.entry_key=p.thread_id
	  WHERE p.store_id=$1 AND p.parent_thread_id=$2
	    AND NOT EXISTS(SELECT 1 FROM mira_thread_actions d WHERE d.store_id=p.store_id AND d.thread_id=p.thread_id AND d.action='delete')
	  ORDER BY p.updated_at, p.thread_id LIMIT $3`, storeID, threadID)
	if err != nil {
		return nil, false, err
	}
	return map[string]any{
		"engine": "codex", "threadId": threadID, "storeId": storeID, "generation": generation, "itemCount": itemCount,
		"parentThreadId": parent, "title": title, "cwd": cwd, "archived": archived, "updatedAt": optionalTime(&updated),
		"complete": complete, "children": children,
	}, true, nil
}

func (service *Service) describeClaudeConversation(ctx context.Context, threadID string, index bool) (map[string]any, bool, error) {
	var sessionID, subpath, cwd, title string
	var parent *string
	var archived bool
	var updated time.Time
	err := service.pool.QueryRow(ctx, `SELECT s.session_id::text,'',NULL::text,s.cwd,s.title,s.archived,s.updated_at FROM mira_claude_sessions s WHERE s.session_id::text=$1
	  UNION ALL
	  SELECT s.session_id::text,t.subpath,s.session_id::text,s.cwd,'子 Agent · '||regexp_replace(regexp_replace(t.subpath,'^subagents/agent-',''),'\.jsonl$',''),s.archived,s.updated_at
	  FROM mira_claude_transcripts t JOIN mira_claude_sessions s USING(session_id) WHERE t.subpath<>'' AND t.thread_id::text=$1
	  LIMIT 1`, threadID).Scan(&sessionID, &subpath, &parent, &cwd, &title, &archived, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	complete := true
	// Canonical (tool) reads do not depend on the index.
	if index {
		if err := service.IndexClaudeTranscript(ctx, sessionID, subpath); err != nil {
			if ctx.Err() != nil {
				return nil, false, err
			}
			// Serve what is already indexed; the background indexer retries.
			complete = false
			service.WakeConversationIndexer()
		}
	}
	children := []conversationChild{}
	if subpath == "" {
		children, err = service.conversationChildren(ctx, `SELECT t.thread_id::text,'子 Agent · '||regexp_replace(regexp_replace(t.subpath,'^subagents/agent-',''),'\.jsonl$','')
		  FROM mira_claude_transcripts t WHERE t.session_id::text=$1 AND t.subpath<>'' ORDER BY t.subpath LIMIT $2`, sessionID)
		if err != nil {
			return nil, false, err
		}
	}
	return map[string]any{
		"engine": "claude", "threadId": threadID, "sessionId": sessionID, "generation": int64(1), "subpath": subpath,
		"parentThreadId": parent, "title": title, "cwd": cwd, "archived": archived, "updatedAt": optionalTime(&updated),
		"complete": complete, "children": children,
	}, true, nil
}

// conversationChildren runs a child listing whose last placeholder is the bound.
func (service *Service) conversationChildren(ctx context.Context, query string, args ...any) ([]conversationChild, error) {
	rows, err := service.pool.Query(ctx, query, append(args, conversationChildrenMax)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	children := []conversationChild{}
	for rows.Next() {
		var child conversationChild
		if err := rows.Scan(&child.ThreadID, &child.Title); err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return children, rows.Err()
}

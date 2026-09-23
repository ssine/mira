package miraserver

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Live Codex turns persist their initial history before sampling. Only append
// commits to that generation can notify; imports, forks, replacements and
// projection rebuilds must not replay old completions.
func enqueueCodexCompletions(ctx context.Context, tx pgx.Tx, storeID, operationID string, before, after map[string]historyEntry) error {
	if storeID != "personal" { // The Web console currently displays this store.
		return nil
	}
	var subscribed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_push_subscriptions s JOIN mira_admin_sessions a USING(session_id)
	 WHERE a.revoked_at IS NULL AND a.expires_at>now())`).Scan(&subscribed); err != nil || !subscribed {
		return err
	}
	for id, previous := range before {
		next, exists := after[id]
		if !exists || next.Generation != previous.Generation || next.ItemCount <= previous.ItemCount {
			continue
		}
		// Read one lifecycle record at a time. Only its turn ID enters the queue;
		// any final-answer body stays out of notifications.
		rows, err := tx.Query(ctx, `SELECT e.payload FROM codex_thread_events e JOIN codex_thread_projections p USING(store_id,thread_id)
		 WHERE e.store_id=$1 AND e.thread_id=$2 AND e.generation=$3 AND e.item_seq>$4 AND e.operation_id=$5
		 AND p.parent_thread_id IS NULL AND lower(COALESCE(p.source_kind,'')) NOT LIKE '%subagent%'
		 AND NOT EXISTS(SELECT 1 FROM mira_agent_graph_edges g WHERE g.store_id=e.store_id AND g.child_thread_id=e.thread_id AND g.child_generation=e.generation)
		 AND e.payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(task_complete|turn_complete|turn_aborted)"'
		 ORDER BY e.item_seq`, storeID, id, next.Generation, previous.ItemCount, operationID)
		if err != nil {
			return err
		}
		var turns []string
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				break
			}
			if turn := completedPushTurn(raw); turn != "" {
				turns = append(turns, turn)
			}
		}
		rows.Close()
		if err != nil {
			return err
		}
		if err = rows.Err(); err != nil {
			return err
		}
		for _, turn := range turns {
			var title string
			if err = tx.QueryRow(ctx, `SELECT COALESCE(title,'') FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2`, storeID, id).Scan(&title); err != nil {
				return err
			}
			if err = enqueuePush(ctx, tx, "codex", storeID, id, next.Generation, turn, title); err != nil {
				return err
			}
		}
	}
	return nil
}

func completedPushTurn(raw []byte) string {
	var record struct {
		Type    string `json:"type"`
		Payload struct {
			Type   string          `json:"type"`
			TurnID string          `json:"turn_id"`
			Error  json.RawMessage `json:"error"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &record) != nil || record.Type != "event_msg" {
		return ""
	}
	p := record.Payload
	if (p.Type != "task_complete" && p.Type != "turn_complete") || (len(p.Error) > 0 && string(p.Error) != "null") || len(p.TurnID) > 256 {
		return ""
	}
	return p.TurnID
}

func enqueuePush(ctx context.Context, tx pgx.Tx, runtime, storeID, threadID string, generation int64, turnID, title string) error {
	if !operationIDPattern.MatchString(threadID) {
		return nil
	}
	title = strings.TrimSpace(title)
	if runes := []rune(title); len(runes) > 120 {
		title = string(runes[:120]) + "…"
	}
	if title == "" {
		title = "Mira 对话"
	}
	_, err := tx.Exec(ctx, `INSERT INTO mira_push_deliveries(subscription_id,runtime,store_id,thread_id,generation,turn_id,title)
	 SELECT s.subscription_id,$1,$2,$3,$4,$5,$6 FROM mira_push_subscriptions s JOIN mira_admin_sessions a USING(session_id)
	 WHERE a.revoked_at IS NULL AND a.expires_at>now() ON CONFLICT DO NOTHING`, runtime, storeID, threadID, generation, turnID, title)
	return err
}

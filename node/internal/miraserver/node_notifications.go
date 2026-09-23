package miraserver

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func (server *Server) routeNodeNotifications(ctx context.Context, w http.ResponseWriter, r *http.Request) (bool, error) {
	if r.URL.Path != "/v1/push/node-subscription" {
		return false, nil
	}
	principal, err := server.authorize(ctx, w, r, "admin", authOptions{CSRF: true})
	if err != nil || principal == nil {
		return true, err
	}
	key := r.URL.Query().Get("nodeKey")
	if r.Method != http.MethodGet {
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			return true, foundation.WriteErrorJSON(w, 405, "method not allowed", "method_not_allowed")
		}
		var body struct {
			NodeKey string `json:"nodeKey"`
		}
		if err = foundation.ReadJSON(r, &body, 1024); err != nil {
			return true, err
		}
		key = body.NodeKey
	}
	if len(key) == 0 || len(key) > 256 {
		return true, foundation.WriteErrorJSON(w, 400, "无效的设备", "invalid_node")
	}
	tx, err := server.pool.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx.Rollback(ctx)
	var nodeID string
	err = tx.QueryRow(ctx, `SELECT node_id FROM codex_nodes WHERE node_key=$1 AND platform='android' AND approval_status='approved' FOR UPDATE`, key).Scan(&nodeID)
	if err == pgx.ErrNoRows {
		return true, foundation.WriteErrorJSON(w, 404, "请先连接并批准这台 Android 设备", "node_not_found")
	}
	if err != nil {
		return true, err
	}
	enabled := false
	switch r.Method {
	case http.MethodGet:
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_node_notification_subscriptions WHERE node_id=$1 AND session_id=$2)`, nodeID, principal.SessionID).Scan(&enabled)
	case http.MethodDelete:
		_, err = tx.Exec(ctx, `DELETE FROM mira_node_notification_subscriptions WHERE node_id=$1 AND session_id=$2`, nodeID, principal.SessionID)
	case http.MethodPost:
		// A new login does not inherit an old session's queued notifications.
		_, err = tx.Exec(ctx, `DELETE FROM mira_node_notification_subscriptions WHERE node_id=$1 AND session_id<>$2`, nodeID, principal.SessionID)
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO mira_node_notification_subscriptions(node_id,session_id) VALUES($1,$2) ON CONFLICT(node_id) DO NOTHING`, nodeID, principal.SessionID)
		}
		enabled = true
	}
	if err != nil {
		return true, err
	}
	if err = tx.Commit(ctx); err != nil {
		return true, err
	}
	return true, writeJSON(w, 200, map[string]any{"enabled": enabled})
}

func enqueueNodeCompletion(ctx context.Context, tx pgx.Tx, storeID, threadID string, generation int64, turnID, title string) error {
	// Lock subscriptions in a stable order so simultaneous turns cannot exceed
	// the 128 pending deliveries per device. Completed IDs remain dedup tombstones.
	rows, err := tx.Query(ctx, `SELECT s.node_id FROM mira_node_notification_subscriptions s
 JOIN mira_admin_sessions a USING(session_id) JOIN codex_nodes n USING(node_id)
 WHERE a.revoked_at IS NULL AND a.expires_at>now() AND n.approval_status='approved'
 ORDER BY s.node_id FOR UPDATE OF s`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		_, err = tx.Exec(ctx, `INSERT INTO mira_node_notification_deliveries(node_id,store_id,thread_id,generation,turn_id,title)
 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, id, storeID, threadID, generation, turnID, title)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE mira_node_notification_deliveries SET finished=true WHERE delivery_id IN
 (SELECT delivery_id FROM mira_node_notification_deliveries WHERE node_id=$1 AND NOT finished ORDER BY created_at DESC,delivery_id DESC OFFSET 128)`, id)
		if err != nil {
			return err
		}
	}
	return nil
}

func (server *Server) startNodeNotificationWorker(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if err := server.processNodeNotification(ctx); err != nil && ctx.Err() == nil {
				server.config.Logger.Print("native notification processing failed")
			}
		}
	}()
	return func() { cancel(); <-done }
}

type nodeNotificationSender interface {
	DeliverNotification(context.Context, string, map[string]any) (any, error)
}

func (server *Server) processNodeNotification(ctx context.Context) error {
	return server.processNodeNotificationWith(ctx, server.channel)
}

func (server *Server) processNodeNotificationWith(ctx context.Context, sender nodeNotificationSender) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := server.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `DELETE FROM mira_node_notification_subscriptions s USING mira_admin_sessions a,codex_nodes n
 WHERE s.session_id=a.session_id AND s.node_id=n.node_id AND (a.revoked_at IS NOT NULL OR a.expires_at<=now() OR n.approval_status<>'approved')`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM mira_node_notification_deliveries WHERE delivery_id IN(SELECT delivery_id FROM mira_node_notification_deliveries WHERE expires_at<=now() LIMIT 128)`)
	if err != nil {
		return err
	}
	var id, nodeID, threadID, title, storeID string
	var generation int64
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT d.delivery_id,d.node_id,d.thread_id,d.title,d.store_id,d.generation,d.expires_at FROM mira_node_notification_deliveries d
 JOIN mira_node_notification_subscriptions s USING(node_id) JOIN mira_admin_sessions a USING(session_id) JOIN codex_nodes n USING(node_id)
 WHERE NOT d.finished AND d.next_attempt_at<=now() AND d.expires_at>now() AND a.revoked_at IS NULL AND a.expires_at>now() AND n.approval_status='approved'
 ORDER BY d.next_attempt_at,d.created_at LIMIT 1 FOR UPDATE OF d SKIP LOCKED`).Scan(&id, &nodeID, &threadID, &title, &storeID, &generation, &expires)
	if err == pgx.ErrNoRows {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	var valid bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM codex_thread_projections p WHERE store_id=$1 AND thread_id=$2 AND active_generation=$3 AND parent_thread_id IS NULL AND lower(COALESCE(source_kind,'')) NOT LIKE '%subagent%'
 AND NOT EXISTS(SELECT 1 FROM mira_agent_graph_edges g WHERE g.store_id=p.store_id AND g.child_thread_id=p.thread_id AND g.child_generation=p.active_generation)
 AND NOT EXISTS(SELECT 1 FROM mira_thread_actions a WHERE a.store_id=p.store_id AND a.thread_id=p.thread_id AND a.action='delete'))`, storeID, threadID, generation).Scan(&valid)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE mira_node_notification_deliveries SET next_attempt_at=now()+interval '30 seconds',finished=$2 WHERE delivery_id=$1`, id, !valid)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if !valid {
		return nil
	}
	// Release locks before waiting for a phone. Only a correlated acknowledgement
	// after Android posts/deduplicates the notification completes this delivery.
	result, err := sender.DeliverNotification(ctx, nodeID, map[string]any{"deliveryId": id, "threadId": threadID, "title": strings.TrimSpace(title), "expiresAt": expires.UnixMilli()})
	if err != nil {
		return nil
	}
	object, ok := result.(map[string]any)
	if !ok || object["accepted"] != true {
		return nil
	}
	_, err = server.pool.Exec(ctx, `UPDATE mira_node_notification_deliveries SET finished=true WHERE delivery_id=$1`, id)
	return err
}

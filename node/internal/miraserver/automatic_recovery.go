package miraserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

var recoveryPreferenceRoute = regexp.MustCompile(`^/v1/codex/threads/([^/]+)/automatic-input-recovery$`)

func (server *Server) routeAutomaticRecovery(ctx context.Context, response http.ResponseWriter, request *http.Request) (bool, error) {
	match := recoveryPreferenceRoute.FindStringSubmatch(request.URL.Path)
	if match == nil {
		return false, nil
	}
	principal, err := server.authorize(ctx, response, request, "admin", authOptions{CSRF: request.Method != http.MethodGet})
	if err != nil || principal == nil {
		return true, err
	}
	if request.Method != http.MethodGet && request.Method != http.MethodPut {
		return true, &HTTPError{Status: 405, Code: "method_not_allowed", Message: "method not allowed"}
	}
	turnIDs := request.URL.Query()["turnId"]
	if len(turnIDs) > 64 {
		return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "最多同时读取 64 个轮次的恢复状态"}
	}
	for _, id := range turnIDs {
		if id == "" || len(id) > 256 || strings.ContainsRune(id, 0) {
			return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid turn id"}
		}
	}
	store := request.URL.Query().Get("storeId")
	if store == "" {
		store = "personal"
	}
	if _, ok := safeStoreID(store); !ok {
		return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store id"}
	}
	tx, err := server.pool.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx.Rollback(ctx)
	if err = lockScope(ctx, tx, store, []string{match[1]}); err != nil {
		return true, err
	}
	var generation int64
	err = tx.QueryRow(ctx, `SELECT active_generation FROM codex_thread_projections p WHERE store_id=$1 AND thread_id=$2
 AND NOT EXISTS(SELECT 1 FROM mira_thread_actions d WHERE d.store_id=p.store_id AND d.thread_id=p.thread_id AND d.action='delete')`, store, match[1]).Scan(&generation)
	if err == pgx.ErrNoRows {
		return true, &HTTPError{Status: 404, Code: "not_found", Message: "对话不存在"}
	}
	if err != nil {
		return true, err
	}
	if request.Method == http.MethodPut {
		request.Body = http.MaxBytesReader(response, request.Body, 4096)
		body, err := server.readBody(request)
		if err != nil {
			return true, err
		}
		enabled, ok := body["enabled"].(bool)
		expected, valid := integer(body["generation"])
		if !ok || !valid {
			return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "enabled and generation are required"}
		}
		if expected != generation {
			return true, &HTTPError{Status: 409, Code: "recovery_changed", Message: "对话已变更，请重新打开后设置"}
		}
		_, err = tx.Exec(ctx, `INSERT INTO mira_codex_recovery_preferences(store_id,thread_id,generation,enabled) VALUES($1,$2,$3,$4)
   ON CONFLICT(store_id,thread_id,generation) DO UPDATE SET enabled=EXCLUDED.enabled,updated_at=now()`, store, match[1], generation, enabled)
		if err != nil {
			return true, err
		}
		if err = foundation.AppendAudit(ctx, tx, foundation.AuditEvent{Action: "codex_thread.automatic_input_recovery", Principal: principal, Request: request, Metadata: map[string]any{"storeId": store, "threadId": match[1], "generation": generation, "enabled": enabled}}, server.config.Foundation.TrustProxyHeaders); err != nil {
			return true, err
		}
	}
	var enabled bool
	if err = tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM mira_codex_recovery_preferences WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND NOT enabled)`, store, match[1], generation).Scan(&enabled); err != nil {
		return true, err
	}
	var status, reason, latestTurn string
	err = tx.QueryRow(ctx, `SELECT a.status,a.reason,COALESCE(e.turn_id,'') FROM mira_codex_recovery_attempts a
 JOIN mira_codex_execution_events e ON e.operation_id=a.failure_id
 WHERE a.store_id=$1 AND a.thread_id=$2 AND a.generation=$3 ORDER BY a.created_at DESC LIMIT 1`, store, match[1], generation).Scan(&status, &reason, &latestTurn)
	if err != nil && err != pgx.ErrNoRows {
		return true, err
	}
	if latestTurn != "" {
		turnIDs = append(turnIDs, latestTurn)
	}
	notices, err := automaticRecoveryNotices(ctx, tx, store, match[1], generation, turnIDs)
	if err != nil {
		return true, err
	}
	if latest, ok := notices[latestTurn]; ok {
		status, reason = latest.Status, latest.Reason
	}
	if err = tx.Commit(ctx); err != nil {
		return true, err
	}
	return true, writeJSON(response, 200, map[string]any{"enabled": enabled, "generation": generation, "status": status, "reason": reason, "turns": notices})
}

type automaticRecoveryNotice struct {
	RetryCount int    `json:"retryCount"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	Resolved   bool   `json:"resolved"`
}

// Counts share the effective-history boundary used by the stopping budget.
// A dispatched retry is independent of the worker observing it: a Server restart
// must not turn a canceled observer into a stopped conversation.
func automaticRecoveryNotices(ctx context.Context, tx pgx.Tx, store, thread string, generation int64, turnIDs []string) (map[string]automaticRecoveryNotice, error) {
	notices := map[string]automaticRecoveryNotice{}
	if len(turnIDs) == 0 {
		return notices, nil
	}
	rows, err := tx.Query(ctx, `SELECT e.turn_id,
 CASE WHEN dispatched.event_seq IS NOT NULL THEN
   CASE WHEN next_request.seq IS NOT NULL OR EXISTS(SELECT 1 FROM mira_codex_execution_events done
     WHERE done.store_id=$1 AND done.thread_id=$2 AND done.generation=$3 AND done.kind='turn/completed'
     AND done.event_seq>dispatched.event_seq AND (next_request.seq IS NULL OR done.event_seq<next_request.seq)) THEN 'completed'
   WHEN EXISTS(SELECT 1 FROM mira_codex_execution_routes r WHERE r.store_id=$1 AND r.thread_id=$2 AND r.generation=$3
     AND r.node_account_id=e.node_account_id AND r.state IN ('starting','running')) THEN 'dispatching'
   ELSE 'unconfirmed' END
 WHEN a.reason='context canceled' THEN 'unconfirmed' ELSE COALESCE(a.status,'') END,
 CASE WHEN dispatched.event_seq IS NOT NULL OR a.reason='context canceled' THEN '' ELSE COALESCE(a.reason,'') END,
 EXISTS(SELECT 1 FROM mira_codex_input_compatibility d WHERE d.store_id=$1 AND d.thread_id=$2 AND d.generation=$3 AND d.item_refs->>'failureId'=e.operation_id::text),
 (SELECT count(*) FROM mira_codex_execution_events failure
  JOIN mira_codex_execution_events retry ON retry.store_id=$1 AND retry.thread_id=$2 AND retry.generation=$3
    AND retry.kind='turn_requested' AND retry.detail->>'recoveryFailureId'=failure.operation_id::text
  WHERE failure.store_id=$1 AND failure.thread_id=$2 AND failure.generation=$3
    AND failure.kind='invalid_encrypted_content' AND failure.event_seq<=e.event_seq
    AND failure.node_account_id=e.node_account_id
    AND failure.detail->>'credentialRevision'=e.detail->>'credentialRevision'
    AND (failure.detail->>'throughItemSeq')::bigint>=progress.seq
    AND retry.event_seq<=COALESCE(dispatched.event_seq,e.event_seq))
 FROM (SELECT DISTINCT unnest($4::text[]) AS turn_id) requested
 JOIN LATERAL (SELECT * FROM mira_codex_execution_events failure WHERE failure.store_id=$1 AND failure.thread_id=$2 AND failure.generation=$3
  AND failure.turn_id=requested.turn_id AND failure.kind='invalid_encrypted_content' ORDER BY failure.event_seq DESC LIMIT 1) e ON true
 LEFT JOIN mira_codex_recovery_attempts a ON a.failure_id=e.operation_id
 CROSS JOIN LATERAL (SELECT COALESCE((SELECT item_seq FROM codex_thread_events
  WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq<=(e.detail->>'throughItemSeq')::bigint
  AND (payload->>'type'='compacted' OR (payload->>'type'='response_item'
    AND NOT (payload->'payload'->>'type'='message' AND COALESCE(payload->'payload'->>'role','') IN ('system','developer'))))
  ORDER BY item_seq DESC LIMIT 1),0) AS seq) progress
 LEFT JOIN LATERAL (SELECT event_seq FROM mira_codex_execution_events retry
  WHERE retry.store_id=$1 AND retry.thread_id=$2 AND retry.generation=$3 AND retry.kind='turn_requested'
    AND retry.detail->>'recoveryFailureId'=e.operation_id::text ORDER BY event_seq DESC LIMIT 1) dispatched ON true
 LEFT JOIN LATERAL (SELECT event_seq AS seq FROM mira_codex_execution_events ordinary
  WHERE ordinary.store_id=$1 AND ordinary.thread_id=$2 AND ordinary.generation=$3
    AND ordinary.kind='turn_requested' AND ordinary.event_seq>dispatched.event_seq ORDER BY event_seq LIMIT 1) next_request ON true`, store, thread, generation, turnIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var turn string
		var notice automaticRecoveryNotice
		if err := rows.Scan(&turn, &notice.Status, &notice.Reason, &notice.Resolved, &notice.RetryCount); err != nil {
			return nil, err
		}
		notices[turn] = notice
	}
	return notices, rows.Err()
}

type automaticRecoveryJob struct{ Failure, Store, Thread, Node, Binding, Runtime string }

const automaticRecoveryFailureLimit = 20

// Bounded workers also own the broker connection while the continuation runs.
// A persisted receipt is consumed once, even if the process dies before ack.
func (server *Server) startAutomaticRecovery(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					job, err := server.claimAutomaticRecovery(ctx)
					if err != nil {
						if ctx.Err() == nil {
							server.config.Logger.Printf("automatic input recovery claim failed: %v", err)
						}
						continue
					}
					if job == nil {
						continue
					}
					err = server.runAutomaticRecovery(ctx, *job)
					if ctx.Err() != nil {
						// The retry may still run on its execution Node. Its durable
						// dispatch/lifecycle records, not this observer, own the outcome.
						return
					}
					status, reason := "completed", ""
					if err != nil {
						status = "stopped"
						reason = err.Error()
					}
					// Do not store provider responses or command output in status metadata.
					if len(reason) > 512 {
						reason = "自动恢复失败，请打开对话检查并继续"
					}
					saveCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
					_, saveErr := server.pool.Exec(saveCtx, `UPDATE mira_codex_recovery_attempts SET status=$2,reason=$3,updated_at=now() WHERE failure_id=$1::uuid`, job.Failure, status, reason)
					done()
					if saveErr != nil {
						server.config.Logger.Printf("automatic input recovery outcome could not be saved: %v", saveErr)
					}
				}
			}
		}()
	}
	return func() { cancel(); workers.Wait() }
}

func (server *Server) claimAutomaticRecovery(ctx context.Context) (*automaticRecoveryJob, error) {
	// Each distinct failed turn is consumed once. The history-based failure
	// budget is checked before applying another recovery, not by retry ancestry.
	var job automaticRecoveryJob
	err := server.pool.QueryRow(ctx, `WITH candidate AS (
 SELECT e.* FROM codex_thread_projections p
 LEFT JOIN mira_codex_recovery_preferences pref ON p.store_id=pref.store_id AND p.thread_id=pref.thread_id AND p.active_generation=pref.generation
 JOIN mira_codex_execution_routes r ON r.store_id=p.store_id AND r.thread_id=p.thread_id AND r.generation=p.active_generation
 JOIN mira_node_codex_accounts b USING(node_account_id)
 JOIN mira_codex_execution_events e ON e.store_id=r.store_id AND e.thread_id=r.thread_id AND e.generation=r.generation
   AND e.node_account_id=r.node_account_id AND e.runtime_id=r.runtime_id AND e.turn_id=r.turn_id AND e.kind='invalid_encrypted_content'

 WHERE COALESCE(pref.enabled,true) AND r.state='idle' AND r.turn_id=e.turn_id AND r.revision=e.revision AND b.enabled
 AND b.credential_revision=(e.detail->>'credentialRevision')::bigint
 AND NOT EXISTS(SELECT 1 FROM mira_codex_recovery_attempts a WHERE a.failure_id=e.operation_id)
 AND NOT EXISTS(SELECT 1 FROM mira_codex_input_compatibility d WHERE d.item_refs->>'failureId'=e.operation_id::text AND d.store_id=e.store_id AND d.thread_id=e.thread_id)
 AND COALESCE((SELECT d.action FROM mira_thread_actions d WHERE d.store_id=p.store_id AND d.thread_id=p.thread_id ORDER BY action_seq DESC LIMIT 1),'restore') NOT IN ('delete','archive')
 ORDER BY e.event_seq LIMIT 1
 ), claimed AS (
 INSERT INTO mira_codex_recovery_attempts(failure_id,store_id,thread_id,generation,status)
 SELECT operation_id,store_id,thread_id,generation,'applying' FROM candidate ON CONFLICT DO NOTHING RETURNING failure_id)
 SELECT c.operation_id::text,c.store_id,c.thread_id,b.node_id::text,c.node_account_id::text,c.runtime_id
 FROM candidate c JOIN claimed a ON a.failure_id=c.operation_id JOIN mira_node_codex_accounts b USING(node_account_id)`).Scan(&job.Failure, &job.Store, &job.Thread, &job.Node, &job.Binding, &job.Runtime)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func (server *Server) runAutomaticRecovery(ctx context.Context, job automaticRecoveryJob) error {
	setupCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	node, err := server.nodes.Get(setupCtx, job.Node, false)
	if err != nil {
		return errors.New("运行节点不可用，请手动继续")
	}
	account, err := nodes.SelectAccount(node, job.Binding)
	if err != nil || account == nil || account.Reported["runtimeId"] != job.Runtime {
		return errors.New("执行账号已变更，请手动继续")
	}
	plan, err := server.inputRecoveryPlan(setupCtx, job.Store, job.Thread, account)
	if err != nil || plan.FailureID != job.Failure {
		return errors.New("上下文已变化，请手动继续")
	}
	if !plan.Recoverable {
		return errors.New(plan.Reason)
	}
	failures, err := server.automaticRecoveryFailureCount(setupCtx, job, plan)
	if err != nil {
		return err
	}
	if failures >= automaticRecoveryFailureLimit {
		return fmt.Errorf("同一份上下文已连续失败 %d 次，已停止自动恢复；请检查账号或服务商后继续", automaticRecoveryFailureLimit)
	}
	resume, err := server.automaticRecoveryResume(setupCtx, job, plan.Generation)
	if err != nil {
		return err
	}
	decision, err := randomUUID()
	if err != nil {
		return err
	}
	err = server.applyInputRecovery(setupCtx, nil, &foundation.Principal{Kind: "node", NodeID: job.Node, ClientType: "automatic-input-recovery", Transport: "internal"}, job.Store, job.Thread, job.Node, account, plan, decision,
		func(ctx context.Context, tx pgx.Tx) error {
			return channel.CheckAutomaticRecovery(ctx, tx, job.Store, job.Thread, job.Failure)
		})
	if err != nil {
		return errors.New("自动兼容处理未完成，请使用手动处理检查上下文")
	}
	_, err = server.pool.Exec(setupCtx, `UPDATE mira_codex_recovery_attempts SET status='dispatching',updated_at=now() WHERE failure_id=$1::uuid`, job.Failure)
	if err != nil {
		return errors.New("兼容处理已保存，重试状态未确认，请手动继续")
	}
	return server.channel.ContinueRecoveredThread(ctx, job.Store, job.Thread, job.Node, job.Binding, job.Runtime, job.Failure, resume)
}

// Only model-facing history progress resets the budget. Resume settings,
// lifecycle markers and token counters must not turn every retry into a new
// history. Count durable failures rather than process-local attempts so the
// limit survives Server restarts, including attempts made by older Servers.
func (server *Server) automaticRecoveryFailureCount(ctx context.Context, job automaticRecoveryJob, plan inputRecoveryPlan) (int, error) {
	var count int
	err := server.pool.QueryRow(ctx, `WITH progress AS (
 SELECT COALESCE((SELECT item_seq FROM codex_thread_events
 WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq<=$4
 AND (payload->>'type'='compacted' OR (payload->>'type'='response_item'
   AND NOT (payload->'payload'->>'type'='message' AND COALESCE(payload->'payload'->>'role','') IN ('system','developer'))))
 ORDER BY item_seq DESC LIMIT 1),0) AS seq
), failures AS (
 SELECT 1 FROM mira_codex_execution_events e
 JOIN mira_codex_execution_events current ON current.operation_id=$5::uuid
 WHERE e.store_id=$1 AND e.thread_id=$2 AND e.generation=$3
 AND e.kind='invalid_encrypted_content' AND e.event_seq<=current.event_seq
 AND e.node_account_id=current.node_account_id
 AND e.detail->>'credentialRevision'=current.detail->>'credentialRevision'
 AND (e.detail->>'throughItemSeq')::bigint >= (SELECT seq FROM progress)
 LIMIT $6
) SELECT count(*) FROM failures`, job.Store, job.Thread, plan.Generation, plan.ThroughItemSeq, job.Failure, automaticRecoveryFailureLimit).Scan(&count)
	return count, err
}

// Preserve the failed turn's execution options; never broaden an explicit
// sandbox or approval policy when creating the continuation.
func (server *Server) automaticRecoveryResume(ctx context.Context, job automaticRecoveryJob, generation int64) (map[string]any, error) {
	var raw []byte
	err := server.pool.QueryRow(ctx, `SELECT payload FROM codex_thread_events_versioned WHERE store_id=$1 AND thread_id=$2 AND generation=$3
 AND payload->>'type'='turn_context' ORDER BY item_seq DESC LIMIT 1`, job.Store, job.Thread, generation).Scan(&raw)
	if err != nil {
		return nil, errors.New("缺少原轮次的运行设置，请手动继续")
	}
	var record map[string]any
	if err = json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	p := object(record["payload"])
	result := map[string]any{}
	for from, to := range map[string]string{"model": "model", "cwd": "cwd", "approval_policy": "approvalPolicy", "effort": "reasoningEffort"} {
		if value := p[from]; value != nil {
			result[to] = value
		}
	}
	policy := object(p["sandbox_policy"])
	switch policy["type"] {
	case "danger-full-access":
		result["sandbox"] = "danger-full-access"
	case "read-only":
		result["sandbox"] = "read-only"
	case "workspace-write":
		result["sandbox"] = "workspace-write"
		result["config"] = map[string]any{"sandbox_workspace_write": policy}
	default:
		return nil, errors.New("当前沙箱设置需要手动恢复")
	}
	if result["approvalPolicy"] == nil {
		return nil, errors.New("缺少原轮次的审批设置，请手动继续")
	}
	// App Server exposes reasoning effort through config on resume.
	if effort := result["reasoningEffort"]; effort != nil {
		delete(result, "reasoningEffort")
		config, _ := result["config"].(map[string]any)
		if config == nil {
			config = map[string]any{}
		}
		config["model_reasoning_effort"] = effort
		result["config"] = config
	}
	return result, nil
}

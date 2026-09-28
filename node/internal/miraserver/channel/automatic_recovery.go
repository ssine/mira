package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

// Negotiate before saving an in-place decision. Older runtimes keep the
// unload/resume protocol and never receive an unacknowledged history change.
func (channel *Channel) SupportsInputRecovery(ctx context.Context, node, binding, runtime, thread, turn, failure string) (bool, error) {
	session, err := channel.accounts.open(ctx, node, binding, runtime)
	if err != nil {
		return false, err
	}
	defer channel.accounts.closeSession(session)
	value, err := channel.accounts.call(ctx, session, "mira/thread/recover", map[string]any{
		"threadId": thread, "expectedTurnId": turn, "failureId": failure, "probe": true,
	})
	if errors.Is(err, errInputRecoveryUnsupported) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result, _ := value.(map[string]any)
	return fmt.Sprint(result["version"]) == "1", nil
}

var errInputRecoveryUnsupported = errors.New("input recovery RPC is unavailable")

// Call under the execution and canonical thread gates, immediately before
// changing input or reserving a retry. New user turns/account changes win.
func CheckAutomaticRecovery(ctx context.Context, tx pgx.Tx, store, thread, failure string) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_codex_execution_events e
 JOIN mira_codex_execution_routes r USING(store_id,thread_id,generation,node_account_id,runtime_id,revision)
 JOIN codex_thread_projections p ON p.store_id=r.store_id AND p.thread_id=r.thread_id AND p.active_generation=r.generation
 LEFT JOIN mira_codex_recovery_preferences pref ON pref.store_id=p.store_id AND pref.thread_id=p.thread_id AND pref.generation=p.active_generation
 JOIN mira_codex_recovery_attempts a ON a.failure_id=e.operation_id
 JOIN mira_node_codex_accounts b USING(node_account_id)
 WHERE e.operation_id=$3::uuid AND e.store_id=$1 AND e.thread_id=$2 AND e.kind='invalid_encrypted_content'
 AND r.state='idle' AND r.turn_id=e.turn_id AND COALESCE(pref.enabled,true)
 AND b.enabled AND b.credential_revision=(e.detail->>'credentialRevision')::bigint
 AND a.status IN ('applying','dispatching')
 AND NOT EXISTS(SELECT 1 FROM mira_agent_graph_edges g WHERE g.store_id=p.store_id AND g.child_thread_id=p.thread_id AND g.child_generation=p.active_generation AND g.status='closed')
 AND COALESCE((SELECT d.action FROM mira_thread_actions d WHERE d.store_id=p.store_id AND d.thread_id=p.thread_id ORDER BY action_seq DESC LIMIT 1),'restore') NOT IN ('delete','archive'))`, store, thread, failure).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("对话已继续、切换账号或关闭自动恢复，请手动继续")
	}
	return nil
}

func recoveryAncestorIDs(family []executionMember, thread string) []string {
	parents := map[string]string{}
	for _, member := range family {
		parents[member.ID] = member.Parent
	}
	var result []string
	seen := map[string]bool{thread: true}
	for parent := parents[thread]; parent != "" && !seen[parent]; parent = parents[parent] {
		seen[parent] = true
		result = append(result, parent)
	}
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

func isRecoveryAncestor(family []executionMember, thread, ancestor string) bool {
	for _, id := range recoveryAncestorIDs(family, thread) {
		if id == ancestor {
			return true
		}
	}
	return false
}

func (channel *Channel) recoveryAncestors(ctx context.Context, store, thread string) ([]string, error) {
	db, ok := channel.db.(transactionDatabase)
	if !ok {
		return nil, errors.New("execution transactions are unavailable")
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	family, _, err := readExecutionFamily(ctx, tx, store, thread)
	if err != nil {
		return nil, err
	}
	return recoveryAncestorIDs(family, thread), nil
}

// ContinueRecoveredThread uses the ordinary broker, including injected tools,
// developer instructions and execution gates. It has no browser dependency.
// The caller owns a durable receipt BEFORE calling; never repeat this call on
// timeout/disconnect. Keep the proxy alive for tools through turn completion.
func (channel *Channel) ContinueRecoveredThread(ctx context.Context, store, thread, nodeID, bindingID, runtimeID, failureID, failedTurn string, resume map[string]any, inPlace bool, dispatched func()) error {
	target, err := channel.nodes.Get(ctx, nodeID, false)
	if err != nil {
		return err
	}
	account, err := nodes.SelectAccount(target, bindingID)
	if err != nil {
		return err
	}
	target = nodes.AccountNode(target, *account)
	if stringValue(target.ReportedAppServer["runtimeId"]) != runtimeID {
		return errors.New("账号运行实例已变更，请手动继续")
	}
	responses := make(chan map[string]any, 8)
	completed := make(chan map[string]any, 1)
	closed := make(chan struct{})
	transport := &socket{internalClose: func() { close(closed) }, internalWrite: func(raw []byte) error {
		message, err := decodeObject(raw)
		if err != nil {
			return err
		}
		if _, ok := message["id"]; ok {
			select {
			case responses <- message:
			default:
				return errors.New("automatic recovery RPC buffer full")
			}
		}
		if message["method"] == "turn/completed" {
			params, _ := message["params"].(map[string]any)
			if params["threadId"] == thread {
				select {
				case completed <- params:
				default:
				}
			}
		}
		return nil
	}}
	client := channel.attachProxy(nodeID, transport, &foundation.Principal{Kind: "admin", SubjectID: "automatic-input-recovery"}, store, target, failureID)
	if client == nil {
		return errors.New("运行节点离线，请手动继续")
	}
	defer channel.cleanupProxy(client)
	// Already bound durably; don't re-claim in response notification bookkeeping.
	client.mu.Lock()
	client.recoveryThreadID = thread
	client.boundThreadIDs[thread] = true
	client.mu.Unlock()
	next := 0
	call := func(method string, params map[string]any) (map[string]any, error) {
		next++
		id := fmt.Sprint(next)
		raw, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
		requestCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		if err := channel.forwardProxyClientMessage(requestCtx, client, raw); err != nil {
			return nil, err
		}
		select {
		case message := <-responses:
			if message["id"] != id || message["method"] != nil {
				return nil, errors.New("自动恢复需要人工处理，请打开对话继续")
			}
			if message["error"] != nil {
				return nil, errors.New("自动恢复请求被拒绝，请打开对话继续")
			}
			result, _ := message["result"].(map[string]any)
			return result, nil
		case <-closed:
			return nil, errors.New("自动恢复连接已断开，请检查对话后继续")
		case <-requestCtx.Done():
			return nil, errors.New("自动恢复未收到确认，请检查对话后继续")
		}
	}
	if _, err = call("initialize", map[string]any{"clientInfo": map[string]any{"name": "mira_input_recovery", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		return err
	}
	if err = channel.forwardProxyClientMessage(ctx, client, []byte(`{"method":"initialized"}`)); err != nil {
		return err
	}
	if inPlace {
		// No overrides: warm sessions and parent-owned children retain their
		// exact configuration. Restore cold ancestors without starting turns.
		ancestors, err := channel.recoveryAncestors(ctx, store, thread)
		if err != nil {
			return err
		}
		for _, ancestor := range ancestors {
			state, err := call("thread/read", map[string]any{"threadId": ancestor, "includeTurns": false})
			if err != nil {
				return err
			}
			if recoveryObject(recoveryObject(state["thread"])["status"])["type"] != "notLoaded" {
				continue
			}
			var raw []byte
			err = channel.db.QueryRow(ctx, `SELECT e.payload FROM codex_thread_projections p
 JOIN codex_thread_events e ON e.store_id=p.store_id AND e.thread_id=p.thread_id AND e.generation=p.active_generation
 WHERE p.store_id=$1 AND p.thread_id=$2 AND e.payload->>'type'='turn_context' ORDER BY e.item_seq DESC LIMIT 1`, store, ancestor).Scan(&raw)
			if err != nil {
				return errors.New("父节点缺少恢复设置，请先打开主对话")
			}
			settings, err := RecoveryResumeSettings(raw)
			if err != nil {
				return err
			}
			settings["threadId"] = ancestor
			if _, err := call("thread/resume", settings); err != nil {
				return err
			}
		}
	}
	resume["threadId"] = thread
	if _, err = call("thread/resume", resume); err != nil {
		return err
	}
	method, params := "turn/start", map[string]any{"threadId": thread, "input": []any{}}
	if inPlace {
		method, params = "mira/thread/recover", map[string]any{"threadId": thread, "expectedTurnId": failedTurn, "failureId": failureID}
	}
	result, err := call(method, params)
	if err != nil {
		return err
	}
	if dispatched != nil {
		dispatched()
	}
	turn, _ := result["turn"].(map[string]any)
	for {
		select {
		case params := <-completed:
			done, _ := params["turn"].(map[string]any)
			if done["id"] != turn["id"] {
				continue
			}
			if done["status"] != "completed" {
				return errors.New("本次自动重试未完成；兼容错误会按连续失败上限继续恢复，其他错误请查看对话详情")
			}
			return nil
		case <-responses:
			return errors.New("自动恢复需要人工处理，请打开对话继续")
		case <-closed:
			return errors.New("自动恢复连接已断开，请检查对话后继续")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

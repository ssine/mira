package miraserver

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/channel"
)

type claudeExecutionTarget struct {
	session, turn, node, runtime string
	revision                     int64
	protocol                     bool
}

type claudeExecutionObservation struct {
	turn, node, state, reason string
	revision                  int64
	checked                   time.Time
}

// Observations are disposable. Losing or evicting one means unknown, never idle.
func (server *Server) rememberClaudeExecution(target claudeExecutionTarget, state, reason string) {
	server.claudeExecutionMu.Lock()
	defer server.claudeExecutionMu.Unlock()
	if server.claudeExecution == nil {
		server.claudeExecution = make(map[string]claudeExecutionObservation)
	}
	if len(server.claudeExecution) >= 4096 {
		for key := range server.claudeExecution {
			delete(server.claudeExecution, key)
			break
		}
	}
	server.claudeExecution[target.session] = claudeExecutionObservation{turn: target.turn, node: target.node, revision: target.revision, state: state, reason: reason, checked: time.Now()}
}

func (server *Server) claudeActivity(s claudeSession) (string, string) {
	if server.channel == nil || !server.channel.IsConnected(s.NodeID) {
		return "unknown", "offline"
	}
	server.claudeExecutionMu.Lock()
	observation := server.claudeExecution[s.ID]
	server.claudeExecutionMu.Unlock()
	if s.ActiveTurn != nil && observation.turn == *s.ActiveTurn && observation.node == s.NodeID && observation.revision == s.Revision && time.Since(observation.checked) < 20*time.Second {
		return observation.state, observation.reason
	}
	return "unknown", "connection"
}

const claudeExecutionSelect = `SELECT s.session_id::text,t.turn_id::text,t.node_id::text,t.runtime_id,t.revision,
 COALESCE(n.capabilities->>'claudeExecutionStatusV1','false')='true'
 FROM mira_claude_sessions s JOIN mira_claude_turns t ON t.turn_id=s.active_turn
 JOIN codex_nodes n ON n.node_id=t.node_id`

func scanClaudeExecution(row pgx.Row) (claudeExecutionTarget, error) {
	var target claudeExecutionTarget
	err := row.Scan(&target.session, &target.turn, &target.node, &target.runtime, &target.revision, &target.protocol)
	return target, err
}

func claudeExecutionUnknown(target claudeExecutionTarget, state map[string]any) *HTTPError {
	message := "执行节点未提供上一轮的退出凭据，请检查原执行进程。"
	switch state["reason"] {
	case "offline":
		message = "执行节点离线，请待节点重新连接后检查状态。"
	case "connection":
		message = "暂时无法联系执行节点，请稍后检查状态。"
	default:
		if !target.protocol {
			message = "执行节点不支持退出状态核验，请升级节点后检查原执行进程。"
		}
	}
	return &HTTPError{Status: 409, Code: "execution_unknown", Message: "无法确认上一轮是否已停止，暂时保留运行占用。" + message}
}

// Rejecting input does not prove that the owning execution ended. In particular,
// a restarted Node may lack both the old worker and its shutdown receipt. Read
// the current reservation and probe it before allowing the Web's next-turn path.
func (server *Server) claudeSteerUnavailable(ctx context.Context, invoker channel.CapabilityInvoker, session, turn string) error {
	notSteerable := &HTTPError{Status: 409, Code: "turn_not_steerable", Message: "本轮无法再追加消息，确认结束后可作为新一轮发送"}
	target, err := scanClaudeExecution(server.pool.QueryRow(ctx, claudeExecutionSelect+` WHERE s.session_id=$1 AND s.active_turn=$2`, session, turn))
	if err == pgx.ErrNoRows {
		return notSteerable
	}
	if err != nil {
		return err
	}
	state, err := server.reconcileClaudeExecution(ctx, invoker, target)
	if err != nil || state["state"] == "unknown" {
		failure := claudeExecutionUnknown(target, state)
		failure.Message = "消息未发送：" + failure.Message
		return failure
	}
	return notSteerable
}

// Do not hold a database lock over a Node round trip. Finalization below rechecks
// the exact ownership tuple under the same session lock used by native writes.
func (server *Server) reconcileClaudeExecution(ctx context.Context, invoker channel.CapabilityInvoker, target claudeExecutionTarget) (map[string]any, error) {
	unknown := func(reason string) map[string]any {
		server.rememberClaudeExecution(target, "unknown", reason)
		return map[string]any{"state": "unknown", "reason": reason, "turnId": target.turn}
	}
	if !invoker.IsConnected(target.node) {
		return unknown("offline"), nil
	}
	if target.runtime == "" {
		return unknown("runtime"), nil
	}
	action := "status"
	if target.protocol {
		action = "reconcile"
	}
	value, err := invoker.Invoke(ctx, target.node, "claude", map[string]any{
		"action": action, "turnId": target.turn, "runtimeId": target.runtime,
	}, 3*time.Second)
	if err != nil {
		return unknown("connection"), err
	}
	state, _ := value.(map[string]any)
	if !target.protocol {
		// Old Nodes can confirm life, but cannot fence an absent turn against a
		// start that was delayed in the control channel.
		if state["runtimeId"] == target.runtime && state["active"] == true {
			server.rememberClaudeExecution(target, "running", "")
			return map[string]any{"state": "running", "turnId": target.turn}, nil
		}
		return unknown("runtime"), nil
	}
	if state["expectedRuntimeId"] != target.runtime || state["turnId"] != target.turn {
		return unknown("runtime"), nil
	}
	currentRuntime, _ := state["runtimeId"].(string)
	if currentRuntime == "" || len(currentRuntime) > 128 {
		return unknown("runtime"), nil
	}
	if state["state"] == "running" && currentRuntime == target.runtime {
		server.rememberClaudeExecution(target, "running", "")
		return state, nil
	}
	proof, _ := state["proof"].(string)
	if state["state"] != "stopped" || !(proof == "turn_fenced" && currentRuntime == target.runtime || proof == "runtime_closed" && currentRuntime != target.runtime) {
		return unknown("runtime"), nil
	}
	if err := server.finalizeClaudeExecution(ctx, target, proof); err != nil {
		return unknown("connection"), err
	}
	server.rememberClaudeExecution(target, "unknown", "history")
	return state, nil
}

func (server *Server) finalizeClaudeExecution(ctx context.Context, target claudeExecutionTarget, proof string) error {
	tx, err := server.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var matches bool
	err = tx.QueryRow(ctx, `SELECT active_turn=$2 AND node_id=$3 AND revision=$4 FROM mira_claude_sessions
 WHERE session_id=$1 AND active_turn IS NOT NULL FOR UPDATE`, target.session, target.turn, target.node, target.revision).Scan(&matches)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if !matches {
		return nil
	}
	tag, err := tx.Exec(ctx, `UPDATE mira_claude_turns SET status='failed',error='Runtime ended without an acknowledged completion',completed_at=now()
 WHERE turn_id=$1 AND session_id=$2 AND node_id=$3 AND revision=$4 AND runtime_id=$5 AND completed_at IS NULL`, target.turn, target.session, target.node, target.revision, target.runtime)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	// Separate evidence from native events; never fabricate a successful result
	// or an acknowledged transcript. Existing history remains untouched.
	id, err := randomUUID()
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"type": "mira_execution_stopped", "degraded": true, "proof": proof, "runtimeId": target.runtime,
		"message": "执行进程已退出，未收到完整的结束记录。已保存的历史仍可查看。"})
	_, err = tx.Exec(ctx, `INSERT INTO mira_claude_events(event_id,session_id,turn_id,payload,event_type) VALUES($1,$2,$3,$4::json,'mira_execution_stopped')`, id, target.session, target.turn, payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE mira_claude_sessions SET active_turn=NULL,persistence='incomplete',updated_at=now() WHERE session_id=$1`, target.session)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Page active reservations independently of the browser, including archived
// sessions. Each pass is bounded and the cursor prevents a stuck Node starving
// later sessions. Neither polling nor reconnection ever restarts model work.
func (server *Server) reconcileClaudeExecutionPage(ctx context.Context, invoker channel.CapabilityInvoker, after string) (string, error) {
	// Give a newly committed start reservation its normal control-call budget
	// before probing absence; a queued start after that budget is fenced safely.
	rows, err := server.pool.Query(ctx, claudeExecutionSelect+` WHERE s.session_id::text>$1
 AND (t.status<>'starting' OR t.created_at<now()-interval '35 seconds') ORDER BY s.session_id LIMIT 64`, after)
	if err != nil {
		return after, err
	}
	var targets []claudeExecutionTarget
	for rows.Next() {
		target, err := scanClaudeExecution(rows)
		if err != nil {
			rows.Close()
			return after, err
		}
		targets = append(targets, target)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return after, err
	}
	jobs := make(chan claudeExecutionTarget)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for target := range jobs {
				probe, cancel := context.WithTimeout(ctx, 4*time.Second)
				_, _ = server.reconcileClaudeExecution(probe, invoker, target)
				cancel()
			}
		}()
	}
	for _, target := range targets {
		select {
		case jobs <- target:
		case <-ctx.Done():
		}
	}
	close(jobs)
	workers.Wait()
	if len(targets) == 64 {
		return targets[len(targets)-1].session, nil
	}
	return "", nil
}

func (server *Server) startClaudeExecutionReconciliation(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		cursor := ""
		for {
			pass, stop := context.WithTimeout(ctx, 40*time.Second)
			next, err := server.reconcileClaudeExecutionPage(pass, server.channel, cursor)
			stop()
			if err == nil {
				cursor = next
			} else if ctx.Err() == nil {
				server.config.Logger.Printf("Claude execution reconciliation failed: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

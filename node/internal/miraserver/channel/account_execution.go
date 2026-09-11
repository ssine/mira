package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

type transactionDatabase interface {
	Begin(context.Context) (pgx.Tx, error)
}

type executionRoute struct {
	Binding    string
	Runtime    string
	Revision   int64
	Generation int64
	State      string
}

// A route is a rebuildable projection of execution events, independent of
// canonical history. The transaction lock serializes handoff with turn/start.
func (channel *Channel) claimExecution(ctx context.Context, proxy *proxy, threadID string, handoff, turn bool) (int64, error) {
	if proxy.nodeAccountID == "" {
		return 0, nil
	}
	db, ok := channel.db.(transactionDatabase)
	if !ok {
		return 0, errors.New("execution transactions are unavailable")
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "mira-execution:"+proxy.storeID+":"+threadID); err != nil {
		return 0, err
	}
	var generation int64
	err = tx.QueryRow(ctx, `SELECT active_generation FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2`, proxy.storeID, threadID).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("对话尚未完成持久化，请稍后重试")
	}
	if err != nil {
		return 0, err
	}
	current, err := channel.nodes.Get(ctx, proxy.targetNodeID, false)
	if err != nil {
		return 0, err
	}
	account, err := nodes.SelectAccount(current, proxy.nodeAccountID)
	if err != nil || account == nil {
		return 0, errors.New("所选账号已不可用")
	}
	if stringValue(account.Reported["runtimeId"]) != proxy.runtimeID || account.Reported["status"] != "running" {
		return 0, errors.New("账号运行实例已变更，请重新连接")
	}
	previous := executionRoute{}
	err = tx.QueryRow(ctx, `SELECT node_account_id::text,runtime_id,revision,generation,state FROM mira_codex_execution_routes WHERE store_id=$1 AND thread_id=$2 FOR UPDATE`, proxy.storeID, threadID).Scan(&previous.Binding, &previous.Runtime, &previous.Revision, &previous.Generation, &previous.State)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	changed := exists && (previous.Binding != proxy.nodeAccountID || previous.Runtime != proxy.runtimeID || previous.Generation != generation)
	if changed && !handoff {
		return 0, errors.New("此对话已切换到其他账号，请重新打开后再发送")
	}
	if changed {
		var sourceNode string
		if err = tx.QueryRow(ctx, `SELECT node_id::text FROM mira_node_codex_accounts WHERE node_account_id=$1::uuid`, previous.Binding).Scan(&sourceNode); err != nil {
			return 0, err
		}
		if err = channel.retirePreviousAccount(ctx, sourceNode, previous.Binding, previous.Runtime); err != nil {
			return 0, err
		}
	}
	if !exists {
		// Imported/legacy threads still have a Node-only binding. Resolve its
		// default profile without relabeling that Node or rewriting history.
		var sourceNode string
		err = tx.QueryRow(ctx, `SELECT node_id::text FROM mira_codex_thread_runtimes WHERE store_id=$1 AND thread_id=$2`, proxy.storeID, threadID).Scan(&sourceNode)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
		if sourceNode != "" && (sourceNode != proxy.targetNodeID || !account.IsDefault) {
			if !handoff {
				return 0, errors.New("请先使用所选账号继续此对话")
			}
			source, err := channel.nodes.Get(ctx, sourceNode, false)
			if err != nil {
				return 0, err
			}
			legacy, err := nodes.SelectAccount(source, "")
			if err != nil {
				return 0, err
			}
			if legacy == nil {
				return 0, errors.New("旧节点需要升级或停止其 Codex 实例后才能切换账号")
			}
			if err = channel.retirePreviousAccount(ctx, sourceNode, legacy.NodeAccountID, stringValue(legacy.Reported["runtimeId"])); err != nil {
				return 0, err
			}
		}
	}
	if turn && exists && !changed && (previous.State == "starting" || previous.State == "running") {
		return 0, errors.New("此对话已有执行中的请求，请等待完成")
	}
	// Old writes may finish while retirement drains the runtime. Only after its
	// exit is acknowledged do we serialize the new route with canonical commits.
	for index, key := range [][]string{{"mira-store", proxy.storeID}, {"mira-thread", proxy.storeID, threadID}} {
		encoded, _ := json.Marshal(key)
		lock := "SELECT pg_advisory_xact_lock(hashtextextended($1,0))"
		if index == 0 {
			lock = "SELECT pg_advisory_xact_lock_shared(hashtextextended($1,0))"
		}
		if _, err = tx.Exec(ctx, lock, string(encoded)); err != nil {
			return 0, err
		}
	}

	revision := previous.Revision
	state := previous.State
	kind := "bound"
	if !exists || changed {
		revision++
		state = "idle"
	}
	if turn {
		state = "starting"
		kind = "turn_requested"
	}
	if !exists || changed || turn {
		operationID, err := randomUUID()
		if err != nil {
			return 0, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO mira_codex_execution_events(operation_id,store_id,thread_id,generation,node_account_id,runtime_id,revision,kind)
		 VALUES($1::uuid,$2,$3,$4,$5::uuid,$6,$7,$8)`, operationID, proxy.storeID, threadID, generation, proxy.nodeAccountID, proxy.runtimeID, revision, kind)
		if err != nil {
			return 0, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO mira_codex_execution_routes(store_id,thread_id,generation,node_account_id,runtime_id,revision,state)
		 VALUES($1,$2,$3,$4::uuid,$5,$6,$7) ON CONFLICT(store_id,thread_id) DO UPDATE SET generation=EXCLUDED.generation,node_account_id=EXCLUDED.node_account_id,runtime_id=EXCLUDED.runtime_id,revision=EXCLUDED.revision,state=EXCLUDED.state,turn_id=NULL,updated_at=NOW()`, proxy.storeID, threadID, generation, proxy.nodeAccountID, proxy.runtimeID, revision, state)
		if err != nil {
			return 0, err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO mira_codex_thread_runtimes(store_id,thread_id,node_id,node_account_id,bound_at)
	 VALUES($1,$2,$3::uuid,$4::uuid,NOW()) ON CONFLICT(store_id,thread_id) DO UPDATE SET node_id=EXCLUDED.node_id,node_account_id=EXCLUDED.node_account_id,bound_at=EXCLUDED.bound_at`, proxy.storeID, threadID, proxy.targetNodeID, proxy.nodeAccountID)
	if err != nil {
		return 0, err
	}
	return revision, tx.Commit(ctx)
}

func (channel *Channel) retirePreviousAccount(ctx context.Context, nodeID, bindingID, runtimeID string) error {
	node, err := channel.nodes.Get(ctx, nodeID, false)
	if err != nil {
		return err
	}
	account, err := nodes.SelectAccount(node, bindingID)
	if err != nil || account == nil {
		return errors.New("无法确认旧账号状态，暂不能切换")
	}
	if node.Status != "online" || !channel.IsConnected(nodeID) {
		return errors.New("旧账号所在节点离线，暂不能确认任务已结束")
	}
	if account.Reported["status"] == "stopped" {
		return nil
	}
	actual := stringValue(account.Reported["runtimeId"])
	if runtimeID != "" && actual != "" && actual != runtimeID {
		return nil
	}
	if actual == "" {
		return errors.New("请先停止旧节点的 Codex 实例并升级节点，再切换账号")
	}
	if err := channel.accounts.Retire(ctx, nodeID, bindingID, actual); err != nil {
		return fmt.Errorf("无法切换账号：%w", err)
	}
	return nil
}

func (channel *Channel) recordExecutionStatus(ctx context.Context, proxy *proxy, threadID, kind, state, turnID string) error {
	if proxy.nodeAccountID == "" || threadID == "" {
		return nil
	}
	operationID, err := randomUUID()
	if err != nil {
		return err
	}
	_, err = channel.db.Exec(ctx, `WITH changed AS (
	 UPDATE mira_codex_execution_routes SET state=$5,turn_id=NULLIF($6,''),updated_at=NOW()
	 WHERE store_id=$1 AND thread_id=$2 AND node_account_id=$3::uuid AND runtime_id=$4
	   AND (($8='request_failed' AND state='starting' AND turn_id IS NULL)
	     OR ($8='turn/started' AND (state='starting' OR (state='running' AND turn_id=$6) OR (state='idle' AND turn_id IS DISTINCT FROM $6)))
	     OR ($8='turn/completed' AND state='running' AND turn_id=$6))
	   AND (state IS DISTINCT FROM $5 OR turn_id IS DISTINCT FROM NULLIF($6,'')) RETURNING *
	) INSERT INTO mira_codex_execution_events(operation_id,store_id,thread_id,generation,node_account_id,runtime_id,revision,kind,turn_id)
	 SELECT $7::uuid,store_id,thread_id,generation,node_account_id,runtime_id,revision,$8,turn_id FROM changed`, proxy.storeID, threadID, proxy.nodeAccountID, proxy.runtimeID, state, turnID, operationID, kind)
	return err
}

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

	family, root, err := readExecutionFamily(ctx, tx, proxy.storeID, threadID)
	if err != nil {
		return 0, err
	}
	// All members use the root's execution gate. Do not hold canonical storage
	// locks while waiting for old processes to drain their outstanding writes.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "mira-execution:"+proxy.storeID+":"+root); err != nil {
		return 0, err
	}
	family, lockedRoot, err := readExecutionFamily(ctx, tx, proxy.storeID, threadID)
	if err != nil {
		return 0, err
	}
	if lockedRoot != root {
		return 0, errors.New("会话父子关系已变更，请重新打开")
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

	retired := map[string]bool{}
	for _, member := range family {
		source, err := channel.executionSource(ctx, member, proxy, account.IsDefault)
		if err != nil {
			return 0, err
		}
		if source == nil {
			continue
		}
		if !handoff {
			return 0, errors.New("此对话或父子会话已切换到其他账号，请重新打开主会话后再发送")
		}
		if threadID != root {
			return 0, errors.New("子 Agent 与主会话共用账号，请先从主会话切换账号")
		}
		if len(family) > 1 && len(retired) == 0 {
			if err = channel.requireTreeExecutionProtocol(ctx, tx, proxy); err != nil {
				return 0, err
			}
		}
		if !retired[source.key()] {
			if err = channel.retirePreviousAccount(ctx, source.Node, source.Binding, source.Runtime); err != nil {
				return 0, err
			}
			retired[source.key()] = true
		}
	}

	// A handoff takes the store gate exclusively only after retirement. This
	// freezes graph membership/generations and makes every route switch atomic.
	gate, _ := json.Marshal([]string{"mira-store", proxy.storeID})
	lock := "SELECT pg_advisory_xact_lock_shared(hashtextextended($1,0))"
	if handoff {
		lock = "SELECT pg_advisory_xact_lock(hashtextextended($1,0))"
	}
	if _, err = tx.Exec(ctx, lock, string(gate)); err != nil {
		return 0, err
	}
	family, lockedRoot, err = readExecutionFamily(ctx, tx, proxy.storeID, threadID)
	if err != nil {
		return 0, err
	}
	if lockedRoot != root {
		return 0, errors.New("会话父子关系已变更，请重新打开")
	}
	for _, member := range family {
		source, err := channel.executionSource(ctx, member, proxy, account.IsDefault)
		if err != nil {
			return 0, err
		}
		if source != nil && (!handoff || !retired[source.key()]) {
			return 0, errors.New("会话树的执行账号已变更，请重新打开主会话")
		}
	}
	var requestedRevision int64
	for _, member := range family {
		if !handoff && member.ID != threadID {
			continue
		}
		key, _ := json.Marshal([]string{"mira-thread", proxy.storeID, member.ID})
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", string(key)); err != nil {
			return 0, err
		}
		revision, err := writeExecutionRoute(ctx, tx, proxy, member, turn && member.ID == threadID, root)
		if err != nil {
			return 0, err
		}
		if member.ID == threadID {
			requestedRevision = revision
		}
	}
	return requestedRevision, tx.Commit(ctx)
}

// A fresh account may not have touched the remote store yet. A bounded list
// probes its actual adapter without starting a turn or trusting a version name.
func (channel *Channel) requireTreeExecutionProtocol(ctx context.Context, tx pgx.Tx, proxy *proxy) error {
	check := func() (bool, error) {
		var supported bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_codex_account_protocols WHERE node_account_id=$1::uuid AND runtime_id=$2::uuid AND protocol>=2)`, proxy.nodeAccountID, proxy.runtimeID).Scan(&supported)
		return supported, err
	}
	if supported, err := check(); err != nil || supported {
		return err
	}
	if channel.accounts != nil {
		session, err := channel.accounts.open(ctx, proxy.targetNodeID, proxy.nodeAccountID, proxy.runtimeID, false)
		if err != nil {
			return err
		}
		defer channel.accounts.closeSession(session)
		if _, err = channel.accounts.call(ctx, session, "thread/list", map[string]any{"limit": 1}); err != nil {
			return err
		}
		if supported, err := check(); err != nil || supported {
			return err
		}
	}
	return errors.New("目标 Codex 运行时需要升级后才能完整切换父子会话的账号")
}

type executionMember struct {
	ID         string
	Generation int64
	Previous   executionRoute
	SourceNode string
	LegacyNode string
}

type executionSource struct{ Node, Binding, Runtime string }

func (source executionSource) key() string {
	return source.Node + ":" + source.Binding + ":" + source.Runtime
}

func (channel *Channel) executionSource(ctx context.Context, member executionMember, proxy *proxy, defaultAccount bool) (*executionSource, error) {
	if member.Previous.Binding != "" {
		if member.Previous.Binding == proxy.nodeAccountID && member.Previous.Runtime == proxy.runtimeID {
			return nil, nil
		}
		return &executionSource{member.SourceNode, member.Previous.Binding, member.Previous.Runtime}, nil
	}
	if member.LegacyNode == "" || (member.LegacyNode == proxy.targetNodeID && defaultAccount) {
		return nil, nil
	}
	node, err := channel.nodes.Get(ctx, member.LegacyNode, false)
	if err != nil {
		return nil, err
	}
	account, err := nodes.SelectAccount(node, "")
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, errors.New("旧节点需要升级或停止其 Codex 实例后才能切换账号")
	}
	return &executionSource{member.LegacyNode, account.NodeAccountID, stringValue(account.Reported["runtimeId"])}, nil
}

// Prefer generation-scoped graph edges. Legacy parent metadata fills only rows
// with no graph entry; a stale edge must not attach a recreated parent/child.
const executionFamilySQL = `WITH RECURSIVE edges AS (
 SELECT e.parent_thread_id AS parent,e.child_thread_id AS child FROM mira_agent_graph_edges e
 JOIN codex_thread_projections p ON p.store_id=e.store_id AND p.thread_id=e.parent_thread_id AND p.active_generation=e.parent_generation
 JOIN codex_thread_projections c ON c.store_id=e.store_id AND c.thread_id=e.child_thread_id AND c.active_generation=e.child_generation WHERE e.store_id=$1
 UNION SELECT c.parent_thread_id,c.thread_id FROM codex_thread_projections c
 JOIN codex_thread_projections p ON p.store_id=c.store_id AND p.thread_id=c.parent_thread_id
 WHERE c.store_id=$1 AND NOT EXISTS(SELECT 1 FROM mira_agent_graph_edges e WHERE e.store_id=c.store_id AND e.child_thread_id=c.thread_id)
), ancestors(id) AS (SELECT $2::text UNION SELECT e.parent FROM edges e JOIN ancestors a ON e.child=a.id),
roots(id) AS (SELECT a.id FROM ancestors a WHERE NOT EXISTS(SELECT 1 FROM edges e WHERE e.child=a.id)),
family(id) AS (SELECT id FROM roots UNION SELECT e.child FROM edges e JOIN family f ON e.parent=f.id)
SELECT p.thread_id,p.active_generation,COALESCE(r.node_account_id::text,''),COALESCE(r.runtime_id,''),COALESCE(r.revision,0),COALESCE(r.generation,0),COALESCE(r.state,''),COALESCE(b.node_id::text,''),COALESCE(l.node_id::text,''),(SELECT count(*) FROM roots),(SELECT min(id) FROM roots)
FROM family f JOIN codex_thread_projections p ON p.store_id=$1 AND p.thread_id=f.id
LEFT JOIN mira_codex_execution_routes r ON r.store_id=p.store_id AND r.thread_id=p.thread_id
LEFT JOIN mira_node_codex_accounts b USING(node_account_id)
LEFT JOIN mira_codex_thread_runtimes l ON l.store_id=p.store_id AND l.thread_id=p.thread_id
ORDER BY p.thread_id`

func readExecutionFamily(ctx context.Context, tx pgx.Tx, storeID, threadID string) ([]executionMember, string, error) {
	rows, err := tx.Query(ctx, executionFamilySQL, storeID, threadID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var family []executionMember
	var root string
	found := false
	for rows.Next() {
		var member executionMember
		var roots int
		if err = rows.Scan(&member.ID, &member.Generation, &member.Previous.Binding, &member.Previous.Runtime, &member.Previous.Revision, &member.Previous.Generation, &member.Previous.State, &member.SourceNode, &member.LegacyNode, &roots, &root); err != nil {
			return nil, "", err
		}
		if roots != 1 {
			return nil, "", errors.New("会话父子关系不一致，无法切换账号")
		}
		found = found || member.ID == threadID
		family = append(family, member)
	}
	if err = rows.Err(); err != nil {
		return nil, "", err
	}
	if !found {
		return nil, "", errors.New("对话尚未完成持久化或父子关系存在循环，请稍后重试")
	}
	return family, root, nil
}

func writeExecutionRoute(ctx context.Context, tx pgx.Tx, proxy *proxy, member executionMember, turn bool, root string) (int64, error) {
	previous := member.Previous
	exists := previous.Binding != ""
	changed := !exists || previous.Binding != proxy.nodeAccountID || previous.Runtime != proxy.runtimeID || previous.Generation != member.Generation
	revision, state, kind := previous.Revision, previous.State, "bound"
	if changed {
		revision++
		state = "idle"
	}
	// Same-runtime turn/start can steer; retain its running state and turn ID.
	starting := turn && state != "starting" && state != "running"
	if starting {
		state = "starting"
		kind = "turn_requested"
	}
	if changed || starting {
		operationID, err := randomUUID()
		if err != nil {
			return 0, err
		}
		detail, _ := json.Marshal(map[string]string{"rootThreadId": root})
		if _, err = tx.Exec(ctx, `INSERT INTO mira_codex_execution_events(operation_id,store_id,thread_id,generation,node_account_id,runtime_id,revision,kind,detail) VALUES($1::uuid,$2,$3,$4,$5::uuid,$6,$7,$8,$9::jsonb)`, operationID, proxy.storeID, member.ID, member.Generation, proxy.nodeAccountID, proxy.runtimeID, revision, kind, detail); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO mira_codex_execution_routes(store_id,thread_id,generation,node_account_id,runtime_id,revision,state) VALUES($1,$2,$3,$4::uuid,$5,$6,$7) ON CONFLICT(store_id,thread_id) DO UPDATE SET generation=EXCLUDED.generation,node_account_id=EXCLUDED.node_account_id,runtime_id=EXCLUDED.runtime_id,revision=EXCLUDED.revision,state=EXCLUDED.state,turn_id=NULL,updated_at=NOW()`, proxy.storeID, member.ID, member.Generation, proxy.nodeAccountID, proxy.runtimeID, revision, state); err != nil {
			return 0, err
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO mira_codex_thread_runtimes(store_id,thread_id,node_id,node_account_id,bound_at) VALUES($1,$2,$3::uuid,$4::uuid,NOW()) ON CONFLICT(store_id,thread_id) DO UPDATE SET node_id=EXCLUDED.node_id,node_account_id=EXCLUDED.node_account_id,bound_at=EXCLUDED.bound_at`, proxy.storeID, member.ID, proxy.targetNodeID, proxy.nodeAccountID)
	return revision, err
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

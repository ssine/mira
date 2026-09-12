package channel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

// The returned release keeps the Node's thread gate until the database route
// transaction has committed or rolled back. Other conversations keep running.
func (channel *Channel) unloadAccountThreads(ctx context.Context, source executionSource, root string, ids []string) (func(), error) {
	node, err := channel.nodes.Get(ctx, source.Node, false)
	if err != nil {
		return nil, err
	}
	account, err := nodes.SelectAccount(node, source.Binding)
	if err != nil || account == nil {
		return nil, errors.New("无法确认旧账号状态，暂不能交接此对话")
	}
	if node.Status != "online" || !channel.IsConnected(source.Node) {
		return nil, errors.New("旧账号所在节点离线，暂不能确认对话已停止")
	}
	actual := stringValue(account.Reported["runtimeId"])
	if account.Reported["status"] == "stopped" || (source.Runtime != "" && actual != "" && actual != source.Runtime) {
		return func() {}, nil
	}
	if node.Capabilities["codexThreadHandoffV1"] != true {
		return nil, errors.New("请升级旧账号所在 Mira Node，以支持单独交接对话")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	session, err := channel.accounts.openScoped(ctx, source.Node, source.Binding, actual, false, ids)
	if err != nil {
		return nil, err
	}
	release := func() { channel.accounts.closeSession(session) }
	value, err := channel.accounts.call(ctx, session, "mira/thread/unload", map[string]any{"threadId": root, "threadIds": ids})
	if err != nil {
		release()
		return nil, err
	}
	result, _ := value.(map[string]any)
	confirmed, _ := result["threadIds"].([]any)
	if len(confirmed) != len(ids) {
		release()
		return nil, errors.New("未确认全部对话已卸载，暂不能交接")
	}
	seen := make(map[string]bool, len(confirmed))
	for _, id := range confirmed {
		if id, ok := id.(string); ok {
			seen[id] = true
		}
	}
	for _, id := range ids {
		if !seen[id] {
			release()
			return nil, fmt.Errorf("未确认对话 %s 已卸载", id)
		}
	}
	return release, nil
}

// PrepareExecutionReload applies the same root gate and precise unload when
// changing the model-facing history. It deliberately precedes storage locks.
func (channel *Channel) PrepareExecutionReload(ctx context.Context, tx pgx.Tx, storeID, threadID, nodeID, bindingID, runtimeID string) (func(), error) {
	family, root, err := readExecutionFamily(ctx, tx, storeID, threadID)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "mira-execution:"+storeID+":"+root); err != nil {
		return nil, err
	}
	family, lockedRoot, err := readExecutionFamily(ctx, tx, storeID, threadID)
	if err != nil {
		return nil, err
	}
	if root != lockedRoot {
		return nil, errors.New("会话父子关系已变更，请重新打开")
	}
	ids := make([]string, 0, len(family))
	for _, member := range family {
		if member.Previous.Binding != bindingID || member.Previous.Runtime != runtimeID {
			return nil, errors.New("会话树的账号已变更，请重新打开主会话")
		}
		ids = append(ids, member.ID)
	}
	return channel.unloadAccountThreads(ctx, executionSource{Node: nodeID, Binding: bindingID, Runtime: runtimeID}, root, ids)
}

package channel

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/executionstate"
)

func invalidEncryptedContent(value any, depth int) bool {
	return executionstate.InvalidEncryptedContent(value, depth)
}

func (channel *Channel) recordInputFailure(ctx context.Context, proxy *proxy, message map[string]any) error {
	if proxy.nodeAccountID == "" {
		return nil
	}
	method := stringValue(message["method"])
	params, _ := message["params"].(map[string]any)
	threadID := stringValue(params["threadId"])
	turnID := stringValue(params["turnId"])
	var failure any
	if method == "error" {
		failure = params["error"]
	} else if method == "turn/completed" {
		turn, _ := params["turn"].(map[string]any)
		failure = turn["error"]
		turnID = stringValue(turn["id"])
	}
	if threadID == "" || !invalidEncryptedContent(failure, 0) {
		return nil
	}
	// Normalize gateway routing failures to the existing recovery event so the
	// account/credential scope, frozen input prefix and compatibility policy stay shared.
	id, err := randomUUID()
	if err != nil {
		return err
	}
	tag, err := channel.db.Exec(ctx, `INSERT INTO mira_codex_execution_events(operation_id,store_id,thread_id,generation,node_account_id,runtime_id,revision,kind,turn_id,detail)
	 SELECT $1::uuid,r.store_id,r.thread_id,r.generation,r.node_account_id,r.runtime_id,r.revision,'invalid_encrypted_content',NULLIF($6,''),
	 jsonb_build_object('code','invalid_encrypted_content','throughItemSeq',p.item_count,'credentialRevision',b.credential_revision)
	 FROM mira_codex_execution_routes r JOIN codex_thread_projections p USING(store_id,thread_id)
	 JOIN mira_node_codex_accounts b USING(node_account_id)
	 WHERE r.store_id=$2 AND r.thread_id=$3 AND r.node_account_id=$4::uuid AND r.runtime_id=$5 AND r.generation=p.active_generation
	 ON CONFLICT DO NOTHING`, id, proxy.storeID, threadID, proxy.nodeAccountID, proxy.runtimeID, turnID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// The canonical history commit may have recorded this first, including
		// while the browser was detached. Still publish the existing notice.
		err = channel.db.QueryRow(ctx, `SELECT e.operation_id::text FROM mira_codex_execution_events e
        JOIN codex_thread_projections p ON p.store_id=e.store_id AND p.thread_id=e.thread_id AND p.active_generation=e.generation
        WHERE e.store_id=$1 AND e.thread_id=$2 AND e.node_account_id=$3::uuid AND e.runtime_id=$4 AND e.turn_id=$5
        AND e.kind='invalid_encrypted_content' ORDER BY event_seq DESC LIMIT 1`, proxy.storeID, threadID, proxy.nodeAccountID, proxy.runtimeID, turnID).Scan(&id)
		if err == pgx.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return channel.writeProxyJSON(proxy, map[string]any{"method": "mira/account/contextIncompatible", "params": map[string]any{"threadId": threadID, "nodeAccountId": proxy.nodeAccountID, "failureId": id, "code": "invalid_encrypted_content"}})
}

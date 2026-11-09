package nodes

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// DeleteAccount retires a stopped binding without deleting credentials, billing
// records, or canonical conversations. Keep the default binding as a tombstone
// so enrollment cannot recreate it or fall back to the Node's local login.
func (service *Service) DeleteAccount(ctx context.Context, request *http.Request, principal *Principal, nodeID, bindingID string) (Result, error) {
	tx, err := service.db.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	var accountID string
	var deleted, stopped bool
	err = tx.QueryRow(ctx, `SELECT b.account_id::text,b.deleted_at IS NOT NULL,
	  COALESCE((CASE WHEN b.is_default THEN n.desired_app_server ELSE b.desired END->>'running')::boolean,false)=false
	  AND COALESCE(CASE WHEN b.is_default THEN n.reported_app_server ELSE b.reported END->>'status','')='stopped'
	  FROM mira_node_codex_accounts b JOIN codex_nodes n USING(node_id)
	  WHERE b.node_id=$1::uuid AND b.node_account_id=$2::uuid FOR UPDATE OF n,b`, nodeID, bindingID).Scan(&accountID, &deleted, &stopped)
	if err == pgx.ErrNoRows {
		return result(404, map[string]any{"error": "Account not found"}), nil
	}
	if err != nil {
		return Result{}, err
	}
	if deleted {
		return result(200, map[string]any{"ok": true}), nil
	}
	if !stopped {
		return result(409, map[string]any{"error": "请先停止此账号并等待节点确认，再删除账号；历史会话会保留", "code": "account_busy"}), nil
	}
	_, err = tx.Exec(ctx, `UPDATE mira_node_codex_accounts SET deleted_at=now(),enabled=false,desired=desired || '{"running":false}'::jsonb,revision=revision+1 WHERE node_account_id=$1::uuid`, bindingID)
	if err != nil {
		return Result{}, err
	}
	if err = service.audit(ctx, tx, AuditEvent{Action: "codex_account.deleted", Principal: principal, TargetNodeID: nodeID, Request: request, Metadata: map[string]any{"nodeAccountId": bindingID, "accountId": accountID}}); err != nil {
		return Result{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return result(200, map[string]any{"ok": true}), nil
}

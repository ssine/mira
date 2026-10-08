package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// RemoveAccount retires the local binding without deleting execution provenance
// or canonical history. The disabled row remains the target of historical FKs.
func (service *Service) RemoveAccount(ctx context.Context, request *http.Request, principal *Principal, nodeID, bindingID string) (Result, error) {
	tx, err := service.db.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	var accountID string
	var isDefault, enabled bool
	var desiredJSON, reportedJSON []byte
	err = tx.QueryRow(ctx, `SELECT account_id::text,is_default,enabled,desired,reported
 FROM mira_node_codex_accounts WHERE node_id=$1::uuid AND node_account_id=$2::uuid FOR UPDATE`, nodeID, bindingID).
		Scan(&accountID, &isDefault, &enabled, &desiredJSON, &reportedJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return result(404, map[string]any{"error": "Account not found", "code": "not_found"}), nil
	}
	if err != nil {
		return Result{}, err
	}
	if isDefault {
		return result(409, map[string]any{"error": "节点默认账号必须保留；可以停止或退出登录", "code": "default_account"}), nil
	}
	if !enabled {
		return result(200, map[string]any{"removed": true}), nil
	}
	var desired, reported map[string]any
	if err := json.Unmarshal(desiredJSON, &desired); err != nil {
		return Result{}, err
	}
	if err := json.Unmarshal(reportedJSON, &reported); err != nil {
		return Result{}, err
	}
	if desired["running"] == true || reported["status"] != "stopped" {
		return result(409, map[string]any{"error": "请先停止此账号并等待节点确认，再删除账号", "code": "account_busy"}), nil
	}
	if _, err := tx.Exec(ctx, `UPDATE mira_node_codex_accounts SET enabled=false,revision=revision+1
 WHERE node_account_id=$1::uuid`, bindingID); err != nil {
		return Result{}, err
	}
	if err := service.audit(ctx, tx, AuditEvent{Action: "codex_account.removed", Principal: principal, TargetNodeID: nodeID, Request: request,
		Metadata: map[string]any{"nodeAccountId": bindingID, "accountId": accountID}}); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return result(200, map[string]any{"removed": true}), nil
}

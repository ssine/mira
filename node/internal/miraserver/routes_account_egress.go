package miraserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/accountegress"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func accountEgressTable(engine string) (string, bool) {
	switch engine {
	case "claude":
		return "mira_claude_accounts", true
	case "codex":
		return "mira_node_codex_accounts", true
	default:
		return "", false
	}
}

func (server *Server) routeAccountEgress(ctx context.Context, response http.ResponseWriter, request *http.Request) (bool, error) {
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "accounts" || parts[4] != "egress" {
		return false, nil
	}
	if request.Method != http.MethodGet && request.Method != http.MethodPost && request.Method != http.MethodDelete {
		return true, foundation.WriteErrorJSON(response, http.StatusMethodNotAllowed, "unsupported account egress operation", "method_not_allowed")
	}
	actor, err := server.authorize(ctx, response, request, "admin", authOptions{CSRF: request.Method != http.MethodGet})
	if err != nil || actor == nil {
		return true, err
	}
	engine, accountID := parts[2], parts[3]
	table, ok := accountEgressTable(engine)
	if !ok || !claudeUUID(accountID) {
		return true, foundation.WriteErrorJSON(response, http.StatusNotFound, "account not found", "not_found")
	}
	var nodeID string
	query := `SELECT node_id::text FROM ` + table + ` WHERE node_account_id=$1`
	if err := server.pool.QueryRow(ctx, query, accountID).Scan(&nodeID); err == pgx.ErrNoRows {
		return true, foundation.WriteErrorJSON(response, http.StatusNotFound, "account not found", "not_found")
	} else if err != nil {
		return true, err
	}
	node, err := server.nodes.Get(ctx, nodeID, false)
	if err != nil {
		return true, err
	}
	if node == nil || node.ApprovalStatus != "approved" || node.Capabilities["accountEgressV1"] != true {
		return true, foundation.WriteErrorJSON(response, http.StatusConflict, "this Node does not support managed account egress", "egress_unavailable")
	}
	switch request.Method {
	case http.MethodGet:
		var egress []byte
		var configured bool
		if err := server.pool.QueryRow(ctx, `SELECT egress,egress_configured FROM `+table+` WHERE node_account_id=$1`, accountID).Scan(&egress, &configured); err != nil {
			return true, err
		}
		var public any
		_ = json.Unmarshal(egress, &public)
		result, err := server.channel.Invoke(ctx, nodeID, "egress", map[string]any{"action": "status", "nodeAccountId": accountID}, 15*time.Second)
		if err != nil {
			return true, writeJSON(response, http.StatusOK, map[string]any{"configured": configured, "egress": public, "reported": nil, "error": err.Error()})
		}
		return true, writeJSON(response, http.StatusOK, map[string]any{"configured": configured, "egress": public, "reported": result})
	case http.MethodDelete:
		if _, err := server.channel.Invoke(ctx, nodeID, "egress", map[string]any{"action": "delete", "nodeAccountId": accountID}, 20*time.Second); err != nil {
			return true, err
		}
		if _, err := server.pool.Exec(ctx, `UPDATE `+table+` SET egress='{}'::jsonb,egress_configured=false WHERE node_account_id=$1`, accountID); err != nil {
			return true, err
		}
		_ = foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "account_egress.deleted", Principal: actor, TargetNodeID: nodeID, Request: request, Metadata: map[string]any{"nodeAccountId": accountID, "engine": engine}}, server.config.Foundation.TrustProxyHeaders)
		return true, writeJSON(response, http.StatusOK, map[string]any{"configured": false})
	}

	body := map[string]any{}
	if err := foundation.ReadJSON(request, &body, 64*1024); err != nil {
		return true, err
	}
	spec, public, err := accountegress.ParsePublic(body)
	if err != nil {
		return true, foundation.WriteErrorJSON(response, http.StatusBadRequest, err.Error(), "invalid_egress")
	}
	if _, err := server.channel.Invoke(ctx, nodeID, "egress", map[string]any{"action": "configure", "nodeAccountId": accountID, "egress": spec}, 30*time.Second); err != nil {
		return true, err
	}
	encoded, _ := json.Marshal(public)
	if _, err := server.pool.Exec(ctx, `UPDATE `+table+` SET egress=$2::jsonb,egress_configured=true WHERE node_account_id=$1`, accountID, encoded); err != nil {
		return true, err
	}
	_ = foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "account_egress.configured", Principal: actor, TargetNodeID: nodeID, Request: request, Metadata: map[string]any{"nodeAccountId": accountID, "engine": engine, "profile": public.Profile}}, server.config.Foundation.TrustProxyHeaders)
	return true, writeJSON(response, http.StatusOK, map[string]any{"configured": true, "egress": public})
}

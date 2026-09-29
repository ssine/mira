package miraserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/clauderuntime"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

// Account management shares the administrator/CSRF/Node-credential boundary with
// Codex. Engine-specific credentials and runtime state remain on the Node.
func (server *Server) routeClaudeAccounts(ctx context.Context, w http.ResponseWriter, r *http.Request, actor *foundation.Principal, parts []string) error {
	if len(parts) > 3 {
		return claudeError(404, "Unknown Claude account operation")
	}
	if len(parts) == 1 && r.Method == "POST" {
		b, err := server.readBody(r)
		if err != nil {
			return err
		}
		nodeID, name := claudeString(b, "nodeId"), strings.TrimSpace(claudeString(b, "name"))
		if !claudeUUID(nodeID) || name == "" || len(name) > 128 || strings.ContainsAny(name, "\r\n\x00") {
			return claudeError(400, "Choose a Node and an account name")
		}
		node, err := server.nodes.Get(ctx, nodeID, false)
		if err != nil {
			return err
		}
		if node == nil || node.ApprovalStatus != "approved" || node.Capabilities["claudeAccountsV1"] != true {
			return claudeError(409, "Upgrade this Node to manage Claude accounts")
		}
		id, _ := randomUUID()
		_, err = server.pool.Exec(ctx, `INSERT INTO mira_claude_accounts(node_account_id,node_id,name) VALUES($1,$2,$3)`, id, nodeID, name)
		if err != nil {
			return err
		}
		if err = foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "claude_account.created", Principal: actor, TargetNodeID: nodeID, Request: r, Metadata: map[string]any{"nodeAccountId": id}}, server.config.Foundation.TrustProxyHeaders); err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]any{"engine": "claude", "nodeAccountId": id, "accountId": id, "nodeId": nodeID, "name": name, "enabled": true, "configured": false})
	}
	if len(parts) < 2 || !claudeUUID(parts[1]) {
		return claudeError(404, "Unknown Claude account")
	}
	if r.Method != "POST" && r.Method != "PATCH" {
		return claudeError(405, "Unsupported account operation")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	b, err := server.readBody(r)
	if err != nil {
		return err
	}
	id := parts[1]
	tx, err := server.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var nodeID string
	if err = tx.QueryRow(ctx, `SELECT node_id::text FROM mira_claude_accounts WHERE node_account_id=$1 FOR UPDATE`, id).Scan(&nodeID); err == pgx.ErrNoRows {
		return claudeError(404, "Claude account not found")
	} else if err != nil {
		return err
	}
	var active bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_claude_sessions WHERE node_account_id=$1 AND active_turn IS NOT NULL)`, id).Scan(&active); err != nil {
		return err
	}
	_, changesEnabled := b["enabled"]
	if active && (len(parts) == 3 || changesEnabled) {
		return claudeError(409, "Claude account is running a turn; stop or finish it before changing account configuration")
	}
	operation := "renamed"
	result := map[string]any{"ok": true}
	if len(parts) == 2 && r.Method == "PATCH" {
		if raw, ok := b["name"]; ok {
			name, valid := raw.(string)
			name = strings.TrimSpace(name)
			if !valid || name == "" || len(name) > 128 || strings.ContainsAny(name, "\r\n\x00") {
				return claudeError(400, "Invalid account name")
			}
			_, err = tx.Exec(ctx, `UPDATE mira_claude_accounts SET name=$2 WHERE node_account_id=$1`, id, name)
			if err != nil {
				return err
			}
		}
		if raw, ok := b["enabled"]; ok {
			enabled, valid := raw.(bool)
			if !valid {
				return claudeError(400, "Invalid enabled setting")
			}
			_, err = tx.Exec(ctx, `UPDATE mira_claude_accounts SET enabled=$2 WHERE node_account_id=$1`, id, enabled)
			if err != nil {
				return err
			}
			operation = "enabled_changed"
		}
	} else if len(parts) == 3 && parts[2] == "configure" && r.Method == "POST" {
		var provider clauderuntime.AccountProvider
		raw, _ := json.Marshal(b["provider"])
		if json.Unmarshal(raw, &provider) != nil {
			return claudeError(400, "Invalid Claude provider")
		}
		if err = provider.Validate(); err != nil {
			return claudeError(400, err.Error())
		}
		params := map[string]any{"action": "account/configure", "nodeAccountId": id, "provider": provider}
		if key, present := b["apiKey"]; present {
			params["apiKey"] = key
		}
		configured, invokeErr := server.channel.Invoke(ctx, nodeID, "claude", params, 30*time.Second)
		if invokeErr != nil {
			return invokeErr
		}
		state, _ := configured.(map[string]any)
		ready := state["configured"] == true
		public, _ := json.Marshal(provider)
		_, err = tx.Exec(ctx, `UPDATE mira_claude_accounts SET provider=$2::jsonb,configured=$3,credential_revision=credential_revision+1 WHERE node_account_id=$1`, id, public, ready)
		if err != nil {
			return err
		}
		result["configured"] = ready
		operation = "configured"
	} else {
		return claudeError(405, "Unsupported account operation")
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if err = foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "claude_account." + operation, Principal: actor, TargetNodeID: nodeID, Request: r, Metadata: map[string]any{"nodeAccountId": id}}, server.config.Foundation.TrustProxyHeaders); err != nil {
		return err
	}
	return writeJSON(w, 200, result)
}

// Called while holding the account row through reservation. Configuration edits
// hold the same row, so no turn can race a credential change or disable operation.
func claudeAccountForTurn(ctx context.Context, tx pgx.Tx, nodeID, accountID string) (string, error) {
	if accountID == "" {
		return "", nil
	}
	if !claudeUUID(accountID) {
		return "", claudeError(400, "Invalid Claude account ID")
	}
	var owner string
	var enabled, configured bool
	var provider clauderuntime.AccountProvider
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT node_id::text,enabled,configured,provider FROM mira_claude_accounts WHERE node_account_id=$1 FOR UPDATE`, accountID).Scan(&owner, &enabled, &configured, &raw)
	if err == pgx.ErrNoRows {
		return "", claudeError(409, "Claude account is unavailable on this Node")
	}
	if err != nil {
		return "", err
	}
	if owner != nodeID || !enabled || !configured {
		return "", claudeError(409, "Choose a configured, enabled Claude account on the execution Node")
	}
	if err = json.Unmarshal(raw, &provider); err != nil {
		return "", err
	}
	return provider.Model, nil
}

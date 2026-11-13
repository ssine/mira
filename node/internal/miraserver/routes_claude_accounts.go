package miraserver

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/clauderuntime"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

var claudeLoginSessionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// Account management shares the administrator/CSRF/Node-credential boundary with
// Codex. Engine-specific credentials and runtime state remain on the Node.
func (server *Server) routeClaudeAccounts(ctx context.Context, w http.ResponseWriter, r *http.Request, actor *foundation.Principal, parts []string) error {
	if len(parts) > 4 {
		return claudeError(404, "Unknown Claude account operation")
	}
	if len(parts) == 1 && r.Method == "POST" {
		b, err := server.readBody(r)
		if err != nil {
			return err
		}
		nodeID, name := claudeString(b, "nodeId"), strings.TrimSpace(claudeString(b, "name"))
		authType := strings.TrimSpace(claudeString(b, "authType"))
		if authType == "" {
			authType = "api_key"
		}
		email := strings.TrimSpace(claudeString(b, "loginEmail"))
		if !claudeUUID(nodeID) || name == "" || len(name) > 128 || strings.ContainsAny(name, "\r\n\x00") {
			return claudeError(400, "Choose a Node and an account name")
		}
		if authType != "api_key" && authType != "subscription" {
			return claudeError(400, "Choose API key or Claude.ai subscription authentication")
		}
		if len(email) > 320 || strings.ContainsAny(email, "\r\n\x00") {
			return claudeError(400, "Invalid Claude login email")
		}
		node, err := server.nodes.Get(ctx, nodeID, false)
		if err != nil {
			return err
		}
		if node == nil || node.ApprovalStatus != "approved" || node.Capabilities["claudeAccountsV1"] != true {
			return claudeError(409, "Upgrade this Node to manage Claude accounts")
		}
		id, _ := randomUUID()
		_, err = server.pool.Exec(ctx, `INSERT INTO mira_claude_accounts(node_account_id,node_id,name,auth_type,login_email) VALUES($1,$2,$3,$4,$5)`, id, nodeID, name, authType, email)
		if err != nil {
			return err
		}
		if err = foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "claude_account.created", Principal: actor, TargetNodeID: nodeID, Request: r, Metadata: map[string]any{"nodeAccountId": id}}, server.config.Foundation.TrustProxyHeaders); err != nil {
			return err
		}
		return writeJSON(w, 200, map[string]any{"engine": "claude", "nodeAccountId": id, "accountId": id, "nodeId": nodeID, "name": name, "authType": authType, "email": email, "enabled": true, "configured": false})
	}
	if len(parts) < 2 || !claudeUUID(parts[1]) {
		return claudeError(404, "Unknown Claude account")
	}
	if r.Method != "GET" && r.Method != "POST" && r.Method != "PATCH" {
		return claudeError(405, "Unsupported account operation")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	id := parts[1]
	if len(parts) == 3 && parts[2] == "login" && r.Method == "GET" {
		return server.claudeAccountLoginStatus(ctx, w, r, id)
	}
	if len(parts) == 3 && parts[2] == "usage" && r.Method == "GET" {
		return server.claudeAccountUsage(ctx, w, r, id)
	}
	if len(parts) == 3 && parts[2] == "usage-history" && r.Method == "GET" {
		return server.claudeAccountUsageHistory(ctx, w, r, id)
	}
	if (len(parts) == 3 && parts[2] == "login" && r.Method == "POST") ||
		(len(parts) == 4 && parts[2] == "login" && (parts[3] == "complete" || parts[3] == "cancel") && r.Method == "POST") ||
		(len(parts) == 3 && parts[2] == "logout" && r.Method == "POST") {
		b, err := server.readBody(r)
		if err != nil {
			return err
		}
		switch {
		case len(parts) == 3 && parts[2] == "login":
			return server.startClaudeAccountLogin(ctx, w, r, actor, id, b)
		case len(parts) == 4 && parts[3] == "complete":
			return server.completeClaudeAccountLogin(ctx, w, r, actor, id, b)
		case len(parts) == 4 && parts[3] == "cancel":
			return server.cancelClaudeAccountLogin(ctx, w, r, actor, id, b)
		default:
			return server.logoutClaudeAccount(ctx, w, r, actor, id)
		}
	}
	b, err := server.readBody(r)
	if err != nil {
		return err
	}
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
		var authType, email string
		if err = tx.QueryRow(ctx, `SELECT auth_type,login_email FROM mira_claude_accounts WHERE node_account_id=$1`, id).Scan(&authType, &email); err != nil {
			return err
		}
		if raw, present := b["authType"]; present {
			value, valid := raw.(string)
			if !valid || value != "api_key" && value != "subscription" {
				return claudeError(400, "Invalid Claude authentication type")
			}
			authType = value
		}
		if raw, present := b["loginEmail"]; present {
			value, valid := raw.(string)
			value = strings.TrimSpace(value)
			if !valid || len(value) > 320 || strings.ContainsAny(value, "\r\n\x00") {
				return claudeError(400, "Invalid Claude login email")
			}
			email = value
		}
		params["authType"] = authType
		configured, invokeErr := server.channel.Invoke(ctx, nodeID, "claude", params, 30*time.Second)
		if invokeErr != nil {
			return invokeErr
		}
		state, _ := configured.(map[string]any)
		ready := state["configured"] == true
		public, _ := json.Marshal(provider)
		_, err = tx.Exec(ctx, `UPDATE mira_claude_accounts SET provider=$2::jsonb,auth_type=$3,login_email=$4,configured=$5,credential_revision=credential_revision+1 WHERE node_account_id=$1`, id, public, authType, email, ready)
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

type claudeLoginAccount struct {
	nodeID     string
	authType   string
	email      string
	activeTurn bool
}

func (server *Server) claudeLoginAccount(ctx context.Context, id string) (claudeLoginAccount, error) {
	var account claudeLoginAccount
	err := server.pool.QueryRow(ctx, `SELECT node_id::text,auth_type,login_email,
	  EXISTS(SELECT 1 FROM mira_claude_sessions WHERE node_account_id=$1 AND active_turn IS NOT NULL)
	  FROM mira_claude_accounts WHERE node_account_id=$1`, id).
		Scan(&account.nodeID, &account.authType, &account.email, &account.activeTurn)
	if err == pgx.ErrNoRows {
		return account, claudeError(404, "Claude account not found")
	}
	if err != nil {
		return account, err
	}
	if account.authType != "subscription" {
		return account, claudeError(409, "Claude account does not use subscription authentication")
	}
	node, err := server.nodes.Get(ctx, account.nodeID, false)
	if err != nil {
		return account, err
	}
	if node == nil || node.ApprovalStatus != "approved" || node.Capabilities["claudeAccountsV1"] != true {
		return account, claudeError(409, "Upgrade and connect this Node before logging in")
	}
	return account, nil
}

func (server *Server) claudeAccountLoginStatus(ctx context.Context, w http.ResponseWriter, r *http.Request, id string) error {
	account, err := server.claudeLoginAccount(ctx, id)
	if err != nil {
		return err
	}
	sessionID := r.URL.Query().Get("sessionId")
	if !claudeLoginSessionPattern.MatchString(sessionID) {
		return claudeError(400, "Invalid Claude login session")
	}
	result, err := server.channel.Invoke(ctx, account.nodeID, "claude", map[string]any{
		"action": "account/login/status", "nodeAccountId": id, "sessionId": sessionID,
	}, 15*time.Second)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, result)
}

func (server *Server) startClaudeAccountLogin(ctx context.Context, w http.ResponseWriter, r *http.Request, actor *foundation.Principal, id string, body map[string]any) error {
	account, err := server.claudeLoginAccount(ctx, id)
	if err != nil {
		return err
	}
	if account.activeTurn {
		return claudeError(409, "Claude account is running a turn; wait or interrupt it before logging in")
	}
	email := account.email
	if raw, present := body["email"]; present {
		value, valid := raw.(string)
		value = strings.TrimSpace(value)
		if !valid || len(value) > 320 || strings.ContainsAny(value, "\r\n\x00") {
			return claudeError(400, "Invalid Claude login email")
		}
		email = value
	}
	result, err := server.channel.Invoke(ctx, account.nodeID, "claude", map[string]any{
		"action": "account/login/start", "nodeAccountId": id, "email": email,
	}, 30*time.Second)
	if err != nil {
		return err
	}
	if email != account.email {
		if _, err = server.pool.Exec(ctx, `UPDATE mira_claude_accounts SET login_email=$2 WHERE node_account_id=$1`, id, email); err != nil {
			return err
		}
	}
	_ = foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "claude_account.login_started", Principal: actor, TargetNodeID: account.nodeID, Request: r, Metadata: map[string]any{"nodeAccountId": id}}, server.config.Foundation.TrustProxyHeaders)
	return writeJSON(w, 200, result)
}

func (server *Server) completeClaudeAccountLogin(ctx context.Context, w http.ResponseWriter, r *http.Request, actor *foundation.Principal, id string, body map[string]any) error {
	account, err := server.claudeLoginAccount(ctx, id)
	if err != nil {
		return err
	}
	sessionID, code := claudeString(body, "sessionId"), claudeString(body, "code")
	if !claudeLoginSessionPattern.MatchString(sessionID) || strings.TrimSpace(code) == "" || len(code) > 4096 {
		return claudeError(400, "Enter the Claude authorization code from the login page")
	}
	result, err := server.channel.Invoke(ctx, account.nodeID, "claude", map[string]any{
		"action": "account/login/complete", "nodeAccountId": id, "sessionId": sessionID, "code": strings.TrimSpace(code),
	}, 40*time.Second)
	if err != nil {
		return err
	}
	state, _ := result.(map[string]any)
	if state["status"] == "completed" {
		if _, err = server.pool.Exec(ctx, `UPDATE mira_claude_accounts SET configured=true,credential_revision=credential_revision+1 WHERE node_account_id=$1`, id); err != nil {
			return err
		}
		_ = foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "claude_account.login_completed", Principal: actor, TargetNodeID: account.nodeID, Request: r, Metadata: map[string]any{"nodeAccountId": id}}, server.config.Foundation.TrustProxyHeaders)
	}
	return writeJSON(w, 200, result)
}

func (server *Server) cancelClaudeAccountLogin(ctx context.Context, w http.ResponseWriter, r *http.Request, actor *foundation.Principal, id string, body map[string]any) error {
	account, err := server.claudeLoginAccount(ctx, id)
	if err != nil {
		return err
	}
	sessionID := claudeString(body, "sessionId")
	if !claudeLoginSessionPattern.MatchString(sessionID) {
		return claudeError(400, "Invalid Claude login session")
	}
	result, err := server.channel.Invoke(ctx, account.nodeID, "claude", map[string]any{
		"action": "account/login/cancel", "nodeAccountId": id, "sessionId": sessionID,
	}, 15*time.Second)
	if err != nil {
		return err
	}
	_ = foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "claude_account.login_cancelled", Principal: actor, TargetNodeID: account.nodeID, Request: r, Metadata: map[string]any{"nodeAccountId": id}}, server.config.Foundation.TrustProxyHeaders)
	return writeJSON(w, 200, result)
}

func (server *Server) logoutClaudeAccount(ctx context.Context, w http.ResponseWriter, r *http.Request, actor *foundation.Principal, id string) error {
	account, err := server.claudeLoginAccount(ctx, id)
	if err != nil {
		return err
	}
	if account.activeTurn {
		return claudeError(409, "Claude account is running a turn; wait or interrupt it before logging out")
	}
	result, err := server.channel.Invoke(ctx, account.nodeID, "claude", map[string]any{
		"action": "account/logout", "nodeAccountId": id,
	}, 30*time.Second)
	if err != nil {
		return err
	}
	if _, err = server.pool.Exec(ctx, `UPDATE mira_claude_accounts SET configured=false,credential_revision=credential_revision+1 WHERE node_account_id=$1`, id); err != nil {
		return err
	}
	_ = foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "claude_account.logged_out", Principal: actor, TargetNodeID: account.nodeID, Request: r, Metadata: map[string]any{"nodeAccountId": id}}, server.config.Foundation.TrustProxyHeaders)
	return writeJSON(w, 200, result)
}

// Called while holding the account row through reservation. Configuration edits
// hold the same row, so no turn can race a credential change or disable operation.
func claudeAccountForTurn(ctx context.Context, tx pgx.Tx, nodeID, accountID string) (clauderuntime.AccountProvider, error) {
	var provider clauderuntime.AccountProvider
	if accountID == "" {
		return provider, nil
	}
	if !claudeUUID(accountID) {
		return provider, claudeError(400, "Invalid Claude account ID")
	}
	var owner string
	var enabled, configured, egressConfigured bool
	var raw, egress []byte
	err := tx.QueryRow(ctx, `SELECT node_id::text,enabled,configured,egress_configured,provider,egress FROM mira_claude_accounts WHERE node_account_id=$1 FOR UPDATE`, accountID).Scan(&owner, &enabled, &configured, &egressConfigured, &raw, &egress)
	if err == pgx.ErrNoRows {
		return provider, claudeError(409, "Claude account is unavailable on this Node")
	}
	if err != nil {
		return provider, err
	}
	if owner != nodeID || !enabled || !configured {
		return provider, claudeError(409, "Choose a configured, enabled Claude account on the execution Node")
	}
	if len(egress) > 2 && !egressConfigured {
		return provider, claudeError(409, "Claude account egress is not configured on the execution Node")
	}
	err = json.Unmarshal(raw, &provider)
	return provider, err
}

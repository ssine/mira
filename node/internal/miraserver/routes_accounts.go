package miraserver

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/ssine/mira/node/internal/miraserver/accountsampler"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

var nodeAccountsPattern = regexp.MustCompile(`(?i)^/v1/nodes/([0-9a-f-]{36})/codex-accounts(?:/([0-9a-f-]{36})(?:/(quota|quota-history|login|logout|login-status|login-cancel|configure))?)?$`)

func (server *Server) routeAccounts(ctx context.Context, response http.ResponseWriter, request *http.Request) (bool, error) {
	path := request.URL.Path
	match := nodeAccountsPattern.FindStringSubmatch(path)
	if path != "/v1/codex/accounts" && match == nil {
		return false, nil
	}
	role := "admin"
	if match != nil && request.Method == http.MethodGet && match[3] == "" {
		role = "trusted"
	}
	principal, err := server.authorize(ctx, response, request, role, authOptions{CSRF: request.Method != http.MethodGet})
	if err != nil || principal == nil {
		return true, err
	}
	if path == "/v1/codex/accounts" {
		if request.Method != http.MethodGet {
			return true, foundation.WriteErrorJSON(response, 405, "method not allowed", "method_not_allowed")
		}
		listed, err := server.nodes.List(ctx, false)
		if err != nil {
			return true, err
		}
		result := []map[string]any{}
		for _, node := range listed {
			for _, account := range node.CodexAccounts {
				result = append(result, map[string]any{
					"account": account, "nodeName": node.Hostname, "nodeDisplayName": node.DisplayName, "nodeStatus": node.Status, "nodeMode": node.NodeMode,
				})
			}
		}
		return true, writeJSON(response, 200, map[string]any{"data": result})
	}
	node, err := server.nodes.Get(ctx, match[1], false)
	if err != nil {
		return true, err
	}
	if node == nil {
		return true, foundation.WriteErrorJSON(response, 404, "Node not found", "not_found")
	}
	if match[2] == "" {
		if request.Method == http.MethodGet {
			return true, writeJSON(response, 200, map[string]any{"data": node.CodexAccounts})
		}
		if request.Method == http.MethodPost {
			body, err := server.readBody(request)
			if err != nil {
				return true, err
			}
			result, err := server.nodes.CreateAccount(ctx, request, principal, node.NodeID, body)
			return true, writeNodeResult(response, result, err)
		}
	}
	account, err := nodes.SelectAccount(node, match[2])
	if err != nil {
		return true, err
	}
	if account == nil {
		return true, foundation.WriteErrorJSON(response, 404, "Account not found", "not_found")
	}
	audit := func(action string) error {
		return foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "codex_account." + action, Principal: principal, TargetNodeID: node.NodeID, Request: request,
			Metadata: map[string]any{"nodeAccountId": account.NodeAccountID, "accountId": account.AccountID}}, server.config.Foundation.TrustProxyHeaders)
	}
	if request.Method == http.MethodGet && match[3] == "" {
		return true, writeJSON(response, 200, account)
	}
	if request.Method == http.MethodGet && match[3] == "quota-history" {
		result, err := server.views.CodexAccountHistory(ctx, account, request.URL.Query().Get("range"))
		if err != nil {
			return true, err
		}
		return true, writeJSON(response, 200, result)
	}
	runtimeID, _ := account.Reported["runtimeId"].(string)
	reader := server.channel.Accounts()
	if request.Method == http.MethodGet && match[3] == "login-status" {
		return true, writeJSON(response, 200, reader.LoginStatus(node.NodeID, account.NodeAccountID, request.URL.Query().Get("sessionId")))
	}
	if request.Method == http.MethodGet && match[3] == "quota" {
		if account.Reported["status"] != "running" {
			return true, writeJSON(response, 200, map[string]any{"status": "stopped", "snapshot": account.Snapshot})
		}
		if provider, _ := account.Reported["provider"].(map[string]any); provider != nil && provider["id"] != nil && provider["id"] != "openai" {
			return true, writeJSON(response, 200, map[string]any{"status": "ok", "quotaSupported": false, "provider": provider})
		}
		snapshot, err := reader.ReadAccount(ctx, node.NodeID, account.NodeAccountID, runtimeID)
		if err != nil {
			return true, &HTTPError{Status: 409, Code: "account_read_failed", Message: err.Error()}
		}
		return true, writeJSON(response, 200, accountsampler.CleanSnapshot(snapshot))
	}
	if request.Method == http.MethodPost || request.Method == http.MethodPatch {
		if match[3] != "" && node.Capabilities["codexAccountsV1"] != true {
			return true, &HTTPError{Status: 409, Code: "account_unsupported", Message: "请先升级此节点，再通过 Mira 管理账号凭据"}
		}
		if request.Method == http.MethodPatch && match[3] != "" {
			return true, &HTTPError{Status: 405, Code: "method_not_allowed", Message: "method not allowed"}
		}
		request.Body = http.MaxBytesReader(response, request.Body, 256*1024)
		body, err := server.readBody(request)
		if err != nil {
			return true, err
		}
		if match[3] == "configure" {
			if account.Desired["running"] == true || account.Reported["status"] != "stopped" {
				return true, &HTTPError{Status: 409, Code: "account_busy", Message: "请先停止此账号并等待节点确认，再修改配置或凭据"}
			}
			settings := map[string]any{"running": false}
			for _, key := range []string{"environmentFiles", "inheritEnv"} {
				if value, exists := body[key]; exists {
					settings[key] = value
				}
			}
			result, err := server.nodes.SetAccountDesired(ctx, node.NodeID, account.NodeAccountID, settings)
			if err != nil || result.Status >= 400 {
				return true, writeNodeResult(response, result, err)
			}
			local := map[string]any{}
			for _, key := range []string{"provider", "apiKey", "environment"} {
				if value, exists := body[key]; exists {
					local[key] = value
				}
			}
			var configured any = map[string]any{"configured": true}
			if len(local) > 0 {
				configured, err = reader.Configure(ctx, node.NodeID, account.NodeAccountID, local)
			}
			if err != nil {
				return true, &HTTPError{Status: 409, Code: "account_configuration_failed", Message: err.Error()}
			}
			_, err = server.pool.Exec(ctx, `UPDATE mira_node_codex_accounts SET credential_revision=credential_revision+1,account_snapshot=NULL,observed_at=NULL WHERE node_account_id=$1::uuid`, account.NodeAccountID)
			if err != nil {
				return true, err
			}
			if err := audit("configured"); err != nil {
				return true, err
			}
			return true, writeJSON(response, 200, configured)
		}
		if match[3] == "login-cancel" {
			sessionID, _ := body["sessionId"].(string)
			err = reader.CancelLogin(ctx, node.NodeID, account.NodeAccountID, sessionID)
		} else if match[3] == "login" {
			provider, _ := account.Reported["provider"].(map[string]any)
			if provider != nil && provider["id"] != nil && provider["id"] != "openai" {
				return true, &HTTPError{Status: 409, Code: "provider_credentials", Message: "此账号使用自定义服务商，请在配置中管理其密钥或环境变量"}
			}
			params := map[string]any{"type": "chatgptDeviceCode"}
			if key, ok := body["apiKey"].(string); ok {
				if key == "" || len(key) > 32768 || strings.ContainsAny(key, "\r\n\x00") {
					return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid API key"}
				}
				params = map[string]any{"type": "apiKey", "apiKey": key}
			}
			result, loginErr := reader.Login(ctx, node.NodeID, account.NodeAccountID, runtimeID, params)
			if loginErr != nil {
				return true, &HTTPError{Status: 409, Code: "account_login_failed", Message: loginErr.Error()}
			}
			// Invalidate quota identity before authentication changes. Secret input
			// is never included in SQL, desired state, or audit metadata.
			_, err = server.pool.Exec(ctx, `UPDATE mira_node_codex_accounts SET revision=revision+1,credential_revision=credential_revision+1,account_snapshot=NULL,observed_at=NULL WHERE node_account_id=$1::uuid`, account.NodeAccountID)
			if err != nil {
				return true, err
			}
			if err := audit("login_started"); err != nil {
				return true, err
			}
			return true, writeJSON(response, 200, result)
		} else if match[3] == "logout" {
			provider, _ := account.Reported["provider"].(map[string]any)
			if provider != nil && provider["id"] != nil && provider["id"] != "openai" {
				return true, &HTTPError{Status: 409, Code: "provider_credentials", Message: "自定义服务商的凭据由配置或环境变量提供；请停止账号后清除对应密钥"}
			}
			err = reader.Logout(ctx, node.NodeID, account.NodeAccountID, runtimeID)
		} else if match[3] == "" && request.Method == http.MethodPatch {
			name, ok := body["name"].(string)
			if !ok || strings.TrimSpace(name) == "" || len(name) > 128 || strings.ContainsAny(name, "\r\n\x00") {
				return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid account name"}
			}
			_, err = server.pool.Exec(ctx, `UPDATE mira_codex_accounts SET name=$2 WHERE account_id=$1::uuid`, account.AccountID, strings.TrimSpace(name))
		} else {
			return true, foundation.WriteErrorJSON(response, 405, "method not allowed", "method_not_allowed")
		}
		if err != nil {
			return true, &HTTPError{Status: 409, Code: "account_operation_failed", Message: err.Error()}
		}
		if match[3] == "logout" {
			_, err = server.pool.Exec(ctx, `UPDATE mira_node_codex_accounts SET revision=revision+1,credential_revision=credential_revision+1,account_snapshot=NULL,observed_at=NULL WHERE node_account_id=$1::uuid`, account.NodeAccountID)
			if err != nil {
				return true, err
			}
		}
		action := match[3]
		if action == "" {
			action = "renamed"
		}
		if err := audit(action); err != nil {
			return true, err
		}
		return true, writeJSON(response, 200, map[string]any{"ok": true})
	}
	return true, foundation.WriteErrorJSON(response, 405, "method not allowed", "method_not_allowed")
}

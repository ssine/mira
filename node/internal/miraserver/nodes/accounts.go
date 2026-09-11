package nodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

// CodexAccount is a Node-local login binding, not a Mira security principal.
// Credentials never appear in desired state or in this public representation.
type CodexAccount struct {
	NodeAccountID      string         `json:"nodeAccountId"`
	NodeID             string         `json:"nodeId"`
	AccountID          string         `json:"accountId"`
	Name               string         `json:"name"`
	Provider           string         `json:"provider"`
	AuthType           string         `json:"authType"`
	IsDefault          bool           `json:"isDefault"`
	Enabled            bool           `json:"enabled"`
	Revision           int64          `json:"revision"`
	CredentialRevision int64          `json:"credentialRevision"`
	Desired            map[string]any `json:"desiredAppServer"`
	Reported           map[string]any `json:"reportedAppServer"`
	Snapshot           map[string]any `json:"snapshot,omitempty"`
	ObservedAt         *string        `json:"observedAt"`
}

// Each Node has one backwards-compatible binding for its existing CODEX_HOME.
// Stable IDs avoid creating new accounts on registration retries or upgrades.
func (service *Service) ensureDefaultAccount(ctx context.Context, nodeID string) error {
	_, err := service.db.Exec(ctx, `WITH account AS (
      INSERT INTO mira_codex_accounts(account_id,name)
      SELECT md5('mira:legacy-account:' || node_id::text)::uuid,'默认账号' FROM codex_nodes
      WHERE node_id=$1::uuid AND capabilities->>'appServer'='true'
      ON CONFLICT DO NOTHING
    ) INSERT INTO mira_node_codex_accounts(node_account_id,node_id,account_id,is_default)
      SELECT md5('mira:legacy-binding:' || node_id::text)::uuid,node_id,
        md5('mira:legacy-account:' || node_id::text)::uuid,true FROM codex_nodes
      WHERE node_id=$1::uuid AND capabilities->>'appServer'='true' ON CONFLICT DO NOTHING`, nodeID)
	return err
}

// SelectAccount preserves the legacy default when omitted, but never silently
// falls back when a caller explicitly selects an unknown or disabled account.
func SelectAccount(node *Node, selector string) (*CodexAccount, error) {
	if node == nil {
		return nil, &foundation.HTTPError{Status: 404, Code: "not_found", Message: "Node not found"}
	}
	var selected *CodexAccount
	for index := range node.CodexAccounts {
		account := &node.CodexAccounts[index]
		if (selector == "" && account.IsDefault) || selector == account.NodeAccountID || selector == account.AccountID || selector == account.Name {
			if selected != nil {
				return nil, &foundation.HTTPError{Status: 409, Code: "ambiguous_account", Message: "账号名称不唯一，请使用账号 ID"}
			}
			selected = account
		}
	}
	if selected == nil && selector == "" && len(node.CodexAccounts) == 0 {
		return nil, nil // A legacy Node/test registry without account metadata.
	}
	if selected == nil || !selected.Enabled {
		return nil, &foundation.HTTPError{Status: 409, Code: "account_unavailable", Message: "所选账号未在此节点配置或已停用"}
	}
	if !selected.IsDefault {
		if supported, _ := node.Capabilities["codexAccountsV1"].(bool); !supported {
			return nil, &foundation.HTTPError{Status: 409, Code: "account_unsupported", Message: "此节点需要升级后才能使用多个账号"}
		}
	}
	return selected, nil
}

// AccountNode supplies account-specific runtime state to existing readers while
// preserving the actual infrastructure Node ID and its capability boundary.
func AccountNode(node *Node, account CodexAccount) *Node {
	copy := *node
	copy.SelectedNodeAccountID = account.NodeAccountID
	copy.SelectedAccountID = account.AccountID
	copy.DesiredAppServer = account.Desired
	copy.ReportedAppServer = account.Reported
	return &copy
}

const accountRowsJSON = `COALESCE((SELECT jsonb_agg(jsonb_build_object(
  'nodeAccountId',b.node_account_id,'nodeId',b.node_id,'accountId',b.account_id,
  'name',a.name,'provider',a.provider,'authType',a.auth_type,'isDefault',b.is_default,
  'enabled',b.enabled,'revision',b.revision,'credentialRevision',b.credential_revision,
  'desiredAppServer',CASE WHEN b.is_default THEN nodes.desired_app_server ELSE
    b.desired || jsonb_build_object('defaultCwd',nodes.desired_app_server->'defaultCwd',
      'developerInstructionsFile',nodes.desired_app_server->'developerInstructionsFile') END,
  'reportedAppServer',CASE WHEN b.is_default THEN nodes.reported_app_server ELSE b.reported END,
  'snapshot',b.account_snapshot,'observedAt',b.observed_at
 ) ORDER BY b.is_default DESC,a.name,b.node_account_id)
 FROM mira_node_codex_accounts b JOIN mira_codex_accounts a USING(account_id)
 WHERE b.node_id=nodes.node_id),'[]'::jsonb)`

func (service *Service) DesiredAccounts(ctx context.Context, nodeID string) ([]map[string]any, error) {
	node, err := service.Get(ctx, nodeID, false)
	if err != nil || node == nil {
		return nil, err
	}
	if enabled, _ := node.Capabilities["codexAccountsV1"].(bool); !enabled {
		return nil, nil
	}
	result := make([]map[string]any, 0, len(node.CodexAccounts))
	for _, account := range node.CodexAccounts {
		result = append(result, map[string]any{
			"nodeAccountId": account.NodeAccountID, "accountId": account.AccountID,
			"name": account.Name, "isDefault": account.IsDefault, "enabled": account.Enabled,
			"revision": account.Revision, "desiredAppServer": account.Desired,
		})
	}
	return result, nil
}

func (service *Service) ReportAccounts(ctx context.Context, nodeID string, raw any) error {
	if raw == nil {
		return nil
	}
	reports, ok := raw.([]any)
	if !ok || len(reports) > 128 {
		return &foundation.HTTPError{Status: 400, Code: "invalid_request", Message: "invalid account reports"}
	}
	for _, entry := range reports {
		record, ok := entry.(map[string]any)
		if !ok {
			return &foundation.HTTPError{Status: 400, Code: "invalid_request", Message: "invalid account report"}
		}
		id, _ := record["nodeAccountId"].(string)
		if !accountUUID(id) {
			return &foundation.HTTPError{Status: 400, Code: "invalid_request", Message: "invalid account id"}
		}
		reported := object(record["reportedAppServer"], nil)
		clean := map[string]any{}
		if provider, ok := reported["provider"].(map[string]any); ok {
			view := map[string]any{}
			for _, key := range []string{"id", "name", "wireApi", "baseUrl", "credentialSource"} {
				if value, ok := provider[key].(string); ok && len(value) <= 2048 && !containsControl(value, false) {
					view[key] = value
				}
			}
			for _, key := range []string{"environmentKeys", "missingEnvironment", "requiredEnvironment"} {
				if values, ok := provider[key].([]any); ok && len(values) <= 128 {
					items := []string{}
					for _, raw := range values {
						if value, ok := raw.(string); ok && len(value) <= 128 && !containsControl(value, false) {
							items = append(items, value)
						}
					}
					view[key] = items
				}
			}
			clean["provider"] = view
		}
		for _, key := range []string{"status", "runtimeId", "codexHome", "codexPath", "codexVersion", "listenUrl", "startedAt", "miraCliPath", "runtimePreparing", "lastError", "busy"} {
			if value, exists := reported[key]; exists {
				if text, ok := value.(string); ok && (len(text) > 4096 || strings.ContainsRune(text, 0)) {
					continue
				}
				if _, ok := value.(string); ok {
					clean[key] = value
				}
				if _, ok := value.(bool); ok {
					clean[key] = value
				}
			}
		}
		encoded, err := marshalJSON(clean)
		if err != nil {
			return err
		}
		if _, err := service.db.Exec(ctx, `UPDATE mira_node_codex_accounts SET reported=$3::jsonb
          WHERE node_id=$1::uuid AND node_account_id=$2::uuid AND NOT is_default`, nodeID, id, encoded); err != nil {
			return err
		}
	}
	return nil
}

func accountUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// CreateAccount creates one local profile. Explicit accountId links a previously
// named global account; email equality never merges identities or quota scopes.
func (service *Service) CreateAccount(ctx context.Context, request *http.Request, principal *Principal, nodeID string, body map[string]any) (Result, error) {
	node, err := service.Get(ctx, nodeID, false)
	if err != nil {
		return Result{}, err
	}
	if node == nil {
		return result(404, map[string]any{"error": "Node not found"}), nil
	}
	if supported, _ := node.Capabilities["codexAccountsV1"].(bool); !supported {
		return result(409, map[string]any{"error": "请先升级此节点以支持多账号", "code": "account_unsupported"}), nil
	}
	name, err := requiredString("name", body["name"], 128)
	if err != nil || strings.TrimSpace(name) == "" {
		return result(400, map[string]any{"error": "账号名称不能为空且最多 128 字符"}), nil
	}
	provider, _ := body["provider"].(string)
	if provider == "" {
		provider = "openai"
	}
	if len(provider) > 128 || containsControl(provider, false) {
		return result(400, map[string]any{"error": "invalid provider"}), nil
	}
	authType, _ := body["authType"].(string)
	if authType == "" {
		authType = "chatgpt"
	}
	if authType != "chatgpt" && authType != "apiKey" && authType != "providerConfig" {
		return result(400, map[string]any{"error": "invalid authType"}), nil
	}
	home, err := optionalAbsolutePath("codexHome", body)
	if err != nil || (home.Value != nil && !nativeAbsolutePath(*home.Value, node.Platform)) {
		return result(400, map[string]any{"error": "Codex home 必须是此节点的绝对路径"}), nil
	}
	accountID, _ := body["accountId"].(string)
	if accountID != "" && !accountUUID(accountID) {
		return result(400, map[string]any{"error": "invalid accountId"}), nil
	}
	tx, err := service.db.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	var lockID string
	if err = tx.QueryRow(ctx, `SELECT node_id::text FROM codex_nodes WHERE node_id=$1::uuid FOR UPDATE`, nodeID).Scan(&lockID); err != nil {
		return Result{}, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM mira_node_codex_accounts WHERE node_id=$1::uuid`, nodeID).Scan(&count); err != nil {
		return Result{}, err
	}
	if count >= 128 {
		return result(409, map[string]any{"error": "此节点的账号配置已达到 128 个的资源上限", "code": "account_capacity"}), nil
	}
	if accountID == "" {
		err = tx.QueryRow(ctx, `INSERT INTO mira_codex_accounts(account_id,name,provider,auth_type) VALUES(gen_random_uuid(),$1,$2,$3) RETURNING account_id::text`, strings.TrimSpace(name), provider, authType).Scan(&accountID)
	} else {
		err = tx.QueryRow(ctx, `SELECT account_id::text FROM mira_codex_accounts WHERE account_id=$1::uuid`, accountID).Scan(&accountID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return result(404, map[string]any{"error": "账号不存在"}), nil
	}
	if err != nil {
		return Result{}, err
	}
	desired := map[string]any{"running": false, "listenUrl": "ws://127.0.0.1:0"}
	if home.Value != nil {
		desired["codexHome"] = *home.Value
	}
	encoded, err := marshalJSON(desired)
	if err != nil {
		return Result{}, err
	}
	var bindingID string
	err = tx.QueryRow(ctx, `INSERT INTO mira_node_codex_accounts(node_account_id,node_id,account_id,desired)
      VALUES(gen_random_uuid(),$1::uuid,$2::uuid,$3::jsonb) RETURNING node_account_id::text`, nodeID, accountID, encoded).Scan(&bindingID)
	if isUniqueViolation(err) {
		return result(409, map[string]any{"error": "此节点已配置该账号"}), nil
	}
	if err != nil {
		return Result{}, err
	}
	if err = service.audit(ctx, tx, AuditEvent{Action: "codex_account.created", Principal: principal, TargetNodeID: nodeID, Request: request,
		Metadata: map[string]any{"nodeAccountId": bindingID, "accountId": accountID}}); err != nil {
		return Result{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return result(201, map[string]any{"nodeAccountId": bindingID, "accountId": accountID, "nodeId": nodeID}), nil
}

func (service *Service) SetAccountDesired(ctx context.Context, nodeID, accountID string, body map[string]any) (Result, error) {
	node, err := service.Get(ctx, nodeID, false)
	if err != nil {
		return Result{}, err
	}
	account, err := SelectAccount(node, accountID)
	if err != nil {
		return Result{}, err
	}
	if account == nil {
		return Result{}, fmt.Errorf("account binding is required")
	}
	if account.IsDefault {
		return service.SetDesiredAppServer(ctx, nodeID, body)
	}
	desired, err := service.normalizedDesiredState(body)
	if err != nil {
		return result(400, map[string]any{"error": err.Error()}), nil
	}
	if err := validateAccountDesiredPaths(desired.JSON, node.Platform); err != nil {
		return result(400, map[string]any{"error": err.Error(), "code": "invalid_request"}), nil
	}
	encoded, err := marshalJSON(desired.JSON)
	if err != nil {
		return Result{}, err
	}
	_, err = service.db.Exec(ctx, `UPDATE mira_node_codex_accounts SET desired=desired || $3::jsonb,revision=revision+1
      WHERE node_id=$1::uuid AND node_account_id=$2::uuid`, nodeID, account.NodeAccountID, encoded)
	return result(200, map[string]any{"nodeAccountId": account.NodeAccountID, "desiredAppServer": desired.JSON}), err
}

var accountEnvironmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

func validAccountEnvironmentName(value string) bool {
	return accountEnvironmentName.MatchString(value) && !strings.EqualFold(value, "CODEX_HOME") && !strings.HasPrefix(strings.ToUpper(value), "MIRA_")
}

func validateAccountDesiredPaths(body map[string]any, platform string) error {
	for _, key := range []string{"codexHome", "codexPath", "defaultCwd", "developerInstructionsFile"} {
		if text, ok := body[key].(string); ok && text != "" && !nativeAbsolutePath(text, platform) {
			return fmt.Errorf("%s must be an absolute %s path", key, platform)
		}
	}
	if values, ok := array(body["environmentFiles"]); ok {
		for _, raw := range values {
			if text, ok := raw.(string); !ok || !nativeAbsolutePath(text, platform) {
				return fmt.Errorf("environmentFiles must contain absolute %s paths", platform)
			}
		}
	}
	return nil
}

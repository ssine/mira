package miraserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

var inputRecoveryRoute = regexp.MustCompile(`^/v1/codex/threads/([^/]+)/input-recovery$`)

func (server *Server) observeAccountProtocol(ctx context.Context, request *http.Request, principal *foundation.Principal) error {
	binding, runtimeID := request.Header.Get("X-Mira-Codex-Account"), request.Header.Get("X-Mira-Codex-Runtime")
	if binding == "" && runtimeID == "" {
		return nil
	}
	protocol := request.Header.Get("X-Mira-Accounts-Version")
	if protocol != "1" && protocol != "2" {
		return &HTTPError{Status: 400, Code: "unsupported_account_protocol", Message: "unsupported Codex account protocol"}
	}
	if principal.Kind != "node" || !operationIDPattern.MatchString(binding) || !operationIDPattern.MatchString(runtimeID) {
		return &HTTPError{Status: 400, Code: "invalid_account_context", Message: "invalid Codex account context"}
	}
	tag, err := server.pool.Exec(ctx, `INSERT INTO mira_codex_account_protocols(node_account_id,runtime_id,protocol)
	 SELECT node_account_id,$3::uuid,$4::integer FROM mira_node_codex_accounts WHERE node_id=$1::uuid AND node_account_id=$2::uuid AND enabled
	 ON CONFLICT(node_account_id,runtime_id) DO UPDATE SET protocol=EXCLUDED.protocol,observed_at=NOW() WHERE mira_codex_account_protocols.protocol<>EXCLUDED.protocol OR mira_codex_account_protocols.observed_at < NOW()-INTERVAL '5 minutes'`, principal.NodeID, binding, runtimeID, protocol)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var valid bool
		if err := server.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_node_codex_accounts WHERE node_id=$1::uuid AND node_account_id=$2::uuid AND enabled)`, principal.NodeID, binding).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return &HTTPError{Status: 403, Code: "account_unavailable", Message: "account does not belong to this Node"}
		}
	}
	return nil
}

func (server *Server) addInputRecovery(ctx context.Context, request *http.Request, storeID, threadID string, result *operationResponse) error {
	if result.Status != 200 || request.Header.Get("X-Mira-Codex-Account") == "" {
		return nil
	}
	var raw []byte
	var policy string
	err := server.pool.QueryRow(ctx, `SELECT d.item_refs,d.policy FROM mira_codex_input_compatibility d
	 JOIN mira_node_codex_accounts b USING(node_account_id)
	 WHERE d.store_id=$1 AND d.thread_id=$2 AND d.generation=$3 AND d.node_account_id=$4::uuid AND d.credential_revision=b.credential_revision
	 ORDER BY d.created_at DESC LIMIT 1`, storeID, threadID, result.Body["generation"], request.Header.Get("X-Mira-Codex-Account")).Scan(&raw, &policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var refs map[string]any
	if err := json.Unmarshal(raw, &refs); err != nil {
		return err
	}
	result.Body["inputRecovery"] = map[string]any{"throughItemSeq": refs["throughItemSeq"], "policy": policy}
	return nil
}

type inputRecoveryPlan struct {
	FailureID      string `json:"failureId"`
	Generation     int64  `json:"generation"`
	ItemCount      int64  `json:"itemCount"`
	ThroughItemSeq int64  `json:"throughItemSeq"`
	EncryptedItems int    `json:"encryptedItems"`
	Policy         string `json:"policy"`
	Recoverable    bool   `json:"recoverable"`
	Reason         string `json:"reason"`
}

func (server *Server) inputRecoveryPlan(ctx context.Context, storeID, threadID string, account *nodes.CodexAccount) (inputRecoveryPlan, error) {
	plan := inputRecoveryPlan{Policy: "omitReasoning", Recoverable: true}
	err := server.pool.QueryRow(ctx, `SELECT e.operation_id::text,p.active_generation,p.item_count,(e.detail->>'throughItemSeq')::bigint
	 FROM mira_codex_execution_events e JOIN codex_thread_projections p USING(store_id,thread_id)
	 WHERE e.store_id=$1 AND e.thread_id=$2 AND e.node_account_id=$3::uuid AND e.generation=p.active_generation
	 AND e.kind='invalid_encrypted_content' AND (e.detail->>'credentialRevision')::bigint=$4
	 AND NOT EXISTS(SELECT 1 FROM mira_codex_input_compatibility d WHERE d.store_id=e.store_id AND d.thread_id=e.thread_id
	 AND d.node_account_id=e.node_account_id AND d.generation=e.generation AND d.item_refs->>'failureId'=e.operation_id::text)
	 ORDER BY e.event_seq DESC LIMIT 1`, storeID, threadID, account.NodeAccountID, account.CredentialRevision).Scan(&plan.FailureID, &plan.Generation, &plan.ItemCount, &plan.ThroughItemSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return plan, &HTTPError{Status: 409, Code: "no_context_failure", Message: "尚未记录此账号的加密上下文错误，继续保留完整上下文"}
	}
	if err != nil {
		return plan, err
	}
	rows, err := server.pool.Query(ctx, `SELECT payload FROM codex_thread_events_versioned WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq<=$4 ORDER BY item_seq`, storeID, threadID, plan.Generation, plan.ThroughItemSeq)
	if err != nil {
		return plan, err
	}
	defer rows.Close()
	userSeen, metaSeen, sourceComplete := false, false, true
	count := int64(0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return plan, err
		}
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			return plan, err
		}
		count++
		payload := object(record["payload"])
		if record["type"] == "session_meta" {
			metaSeen = true
		}
		if record["type"] == "response_item" {
			if payload["type"] == "message" && payload["role"] == "user" {
				userSeen = true
			}
			encrypted, compact, unsupported := classifyEncryptedResponse(payload)
			if encrypted {
				plan.EncryptedItems++
			}
			if compact {
				plan.Policy = "rebuildContext"
				sourceComplete = sourceComplete && userSeen && metaSeen
			}
			if unsupported {
				plan.Recoverable = false
				plan.Reason = "历史包含无法安全丢弃的加密工具结果"
			}
		}
		if record["type"] == "compacted" {
			sourceComplete = sourceComplete && userSeen && metaSeen
			if history, ok := payload["replacement_history"].([]any); ok {
				for _, item := range history {
					encrypted, _, unsupported := classifyEncryptedResponse(object(item))
					if encrypted {
						plan.EncryptedItems++
						plan.Policy = "rebuildContext"
					}
					if unsupported {
						plan.Recoverable = false
						plan.Reason = "压缩上下文包含无法重建的加密工具结果"
					}
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return plan, err
	}
	if count != plan.ThroughItemSeq || (plan.Policy == "rebuildContext" && !sourceComplete) {
		plan.Recoverable = false
		plan.Reason = "缺少完整的压缩前历史，无法安全重建上下文"
	}
	if plan.EncryptedItems == 0 {
		plan.Recoverable = false
		plan.Reason = "未找到可安全处理的加密推理或压缩项"
	}
	return plan, nil
}

func classifyEncryptedResponse(item map[string]any) (encrypted, compaction, unsupported bool) {
	value, _ := item["encrypted_content"].(string)
	switch item["type"] {
	case "reasoning":
		return value != "", false, false
	case "compaction", "context_compaction":
		return value != "", value != "", false
	case "message", "agent_message", "function_call_output", "custom_tool_call_output":
		return false, false, value != "" || hasEncryptedPart(item["content"], 0) || hasEncryptedPart(item["output"], 0)
	}
	return false, false, value != ""
}

func hasEncryptedPart(value any, depth int) bool {
	if depth > 8 {
		return false
	}
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			if hasEncryptedPart(item, depth+1) {
				return true
			}
		}
	case map[string]any:
		if value["type"] == "encrypted_content" {
			return true
		}
		for _, key := range []string{"content", "items", "body"} {
			if hasEncryptedPart(value[key], depth+1) {
				return true
			}
		}
	}
	return false
}

func (server *Server) routeInputRecovery(ctx context.Context, response http.ResponseWriter, request *http.Request) (bool, error) {
	match := inputRecoveryRoute.FindStringSubmatch(request.URL.Path)
	if match == nil {
		return false, nil
	}
	principal, err := server.authorize(ctx, response, request, "admin", authOptions{CSRF: request.Method != http.MethodGet})
	if err != nil || principal == nil {
		return true, err
	}
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		return true, &HTTPError{Status: 405, Code: "method_not_allowed", Message: "method not allowed"}
	}
	storeID := request.URL.Query().Get("storeId")
	if storeID == "" {
		storeID = "personal"
	}
	if _, ok := safeStoreID(storeID); !ok {
		return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store id"}
	}
	bindingID := request.URL.Query().Get("nodeAccountId")
	if !operationIDPattern.MatchString(bindingID) {
		return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "nodeAccountId is required"}
	}
	var nodeID string
	if err := server.pool.QueryRow(ctx, `SELECT node_id::text FROM mira_node_codex_accounts WHERE node_account_id=$1::uuid`, bindingID).Scan(&nodeID); err != nil {
		return true, &HTTPError{Status: 404, Code: "not_found", Message: "account not found"}
	}
	node, err := server.nodes.Get(ctx, nodeID, false)
	if err != nil {
		return true, err
	}
	account, err := nodes.SelectAccount(node, bindingID)
	if err != nil {
		return true, err
	}
	body := map[string]any{}
	if request.Method == http.MethodPost {
		request.Body = http.MaxBytesReader(response, request.Body, 8192)
		body, err = server.readBody(request)
		if err != nil {
			return true, err
		}
		decisionID, _ := body["decisionId"].(string)
		if body["confirm"] != true || !operationIDPattern.MatchString(decisionID) {
			return true, &HTTPError{Status: 400, Code: "confirmation_required", Message: "必须明确确认此兼容处理"}
		}
		var savedStore, savedThread, savedBinding, savedPolicy, savedFailure, savedRuntime string
		var savedGeneration, savedCount int64
		var threadReload bool
		err = server.pool.QueryRow(ctx, `SELECT store_id,thread_id,node_account_id::text,policy,generation,item_refs->>'failureId',COALESCE((item_refs->>'itemCount')::bigint,0),COALESCE(item_refs->>'runtimeId',''),COALESCE((item_refs->>'threadReload')::boolean,false)
		 FROM mira_codex_input_compatibility WHERE decision_id=$1::uuid`, decisionID).Scan(&savedStore, &savedThread, &savedBinding, &savedPolicy, &savedGeneration, &savedFailure, &savedCount, &savedRuntime, &threadReload)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return true, err
		}
		if err == nil {
			generation, _ := integer(body["generation"])
			count, _ := integer(body["itemCount"])
			if savedStore != storeID || savedThread != match[1] || savedBinding != bindingID || savedGeneration != generation || savedFailure != body["failureId"] || savedCount != count {
				return true, &HTTPError{Status: 409, Code: "decision_reused", Message: "此确认编号已用于不同的兼容处理"}
			}
			result := map[string]any{"status": "confirmed", "policy": savedPolicy, "canonicalHistoryUnchanged": true, "duplicate": true}
			if threadReload {
				result["reloadedThreadId"] = savedThread
			} else {
				result["retiredRuntimeId"] = savedRuntime
			}
			return true, writeJSON(response, 200, result)
		}
	}
	plan, err := server.inputRecoveryPlan(ctx, storeID, match[1], account)
	if err != nil {
		return true, err
	}
	if request.Method == http.MethodGet {
		return true, writeJSON(response, 200, plan)
	}
	generation, _ := integer(body["generation"])
	count, _ := integer(body["itemCount"])
	decisionID, _ := body["decisionId"].(string)
	if body["confirm"] != true || !operationIDPattern.MatchString(decisionID) || body["failureId"] != plan.FailureID || generation != plan.Generation || count != plan.ItemCount {
		return true, &HTTPError{Status: 409, Code: "recovery_changed", Message: "上下文已变更，请重新查看并确认兼容处理"}
	}
	if !plan.Recoverable {
		return true, &HTTPError{Status: 409, Code: "context_unrecoverable", Message: plan.Reason}
	}
	runtimeID, _ := account.Reported["runtimeId"].(string)
	var supported bool
	if operationIDPattern.MatchString(runtimeID) {
		err = server.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_codex_account_protocols WHERE node_account_id=$1::uuid AND runtime_id=$2::uuid AND protocol>=1)`, bindingID, runtimeID).Scan(&supported)
		if err != nil {
			return true, err
		}
	}
	if !supported {
		return true, &HTTPError{Status: 409, Code: "runtime_upgrade_required", Message: "此 Codex 运行包尚不支持无损的输入兼容处理，请先更新运行包"}
	}
	tx, err := server.pool.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx.Rollback(ctx)
	release, err := server.channel.PrepareExecutionReload(ctx, tx, storeID, match[1], nodeID, bindingID, runtimeID)
	if err != nil {
		return true, &HTTPError{Status: 409, Code: "thread_busy", Message: err.Error()}
	}
	defer release()
	var state, routeBinding, routeRuntime string
	err = tx.QueryRow(ctx, `SELECT state,node_account_id::text,runtime_id FROM mira_codex_execution_routes WHERE store_id=$1 AND thread_id=$2 FOR UPDATE`, storeID, match[1]).Scan(&state, &routeBinding, &routeRuntime)
	if err != nil || state != "idle" || routeBinding != bindingID || routeRuntime != runtimeID {
		return true, &HTTPError{Status: 409, Code: "account_busy", Message: "请等待本轮结束，并保持当前账号再确认"}
	}
	if err := lockScope(ctx, tx, storeID, []string{match[1]}); err != nil {
		return true, err
	}
	refs, _ := json.Marshal(map[string]any{"throughItemSeq": plan.ThroughItemSeq, "failureId": plan.FailureID, "itemCount": plan.ItemCount, "runtimeId": runtimeID, "threadReload": true})
	tag, err := tx.Exec(ctx, `INSERT INTO mira_codex_input_compatibility(decision_id,store_id,thread_id,generation,node_account_id,model,policy,item_refs,credential_revision)
	 SELECT $1::uuid,$2,$3,$4,$5::uuid,'',$6,$7::jsonb,$8 FROM codex_thread_projections p WHERE p.store_id=$2 AND p.thread_id=$3 AND p.active_generation=$4 AND p.item_count=$9
	 AND EXISTS(SELECT 1 FROM mira_node_codex_accounts b WHERE b.node_account_id=$5::uuid AND b.credential_revision=$8 AND b.enabled)
	 ON CONFLICT(decision_id) DO NOTHING`, decisionID, storeID, match[1], plan.Generation, bindingID, plan.Policy, refs, account.CredentialRevision, plan.ItemCount)
	if err != nil {
		return true, err
	}
	if tag.RowsAffected() == 0 {
		return true, &HTTPError{Status: 409, Code: "recovery_changed", Message: "上下文已变化，请刷新后重试"}
	}
	if err := foundation.AppendAudit(ctx, tx, foundation.AuditEvent{Action: "codex_account.input_recovery_confirmed", Principal: principal, TargetNodeID: nodeID, Request: request,
		Metadata: map[string]any{"nodeAccountId": bindingID, "threadId": match[1], "decisionId": decisionID, "policy": plan.Policy}}, server.config.Foundation.TrustProxyHeaders); err != nil {
		return true, err
	}
	if err := tx.Commit(ctx); err != nil {
		return true, err
	}
	return true, writeJSON(response, 200, map[string]any{"status": "confirmed", "policy": plan.Policy, "canonicalHistoryUnchanged": true, "reloadedThreadId": match[1]})
}

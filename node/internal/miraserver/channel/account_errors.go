package channel

import (
	"context"
	"encoding/json"
	"regexp"
)

var encryptedContentCode = regexp.MustCompile(`"code"\s*:\s*"invalid_encrypted_content"`)

// Only inspect the model error envelope. A tool result mentioning this string
// must not trigger a context change, and ordinary authentication errors do not
// establish encrypted-context incompatibility.
func invalidEncryptedContent(value any, depth int) bool {
	if depth > 8 {
		return false
	}
	switch value := value.(type) {
	case map[string]any:
		if value["code"] == "invalid_encrypted_content" {
			return true
		}
		for _, key := range []string{"error", "message", "additionalDetails"} {
			if invalidEncryptedContent(value[key], depth+1) {
				return true
			}
		}
	case string:
		if len(value) > 32768 {
			return false
		}
		var parsed any
		if json.Unmarshal([]byte(value), &parsed) == nil {
			return invalidEncryptedContent(parsed, depth+1)
		}
		return encryptedContentCode.MatchString(value)
	}
	return false
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
	if tag.RowsAffected() > 0 {
		return proxy.socket.writeJSON(map[string]any{"method": "mira/account/contextIncompatible", "params": map[string]any{"threadId": threadID, "nodeAccountId": proxy.nodeAccountID, "failureId": id, "code": "invalid_encrypted_content"}})
	}
	return nil
}

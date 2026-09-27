package executionstate

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"regexp"
	"strings"
)

var encryptedContentCode = regexp.MustCompile(`"code"\s*:\s*"(?:invalid_encrypted_content|unknown_reasoning_pool)"`)

// Codex may keep only the gateway's message when formatting an HTTP error,
// dropping its structured unknown_reasoning_pool code. Match the known error
// exactly, including Codex's optional HTTP wrapper, rather than every 409.
var unknownReasoningPoolMessage = regexp.MustCompile(`^(?:unexpected status 409(?: Conflict)?: )?(?:encrypted history has no known compatibility pool|history belongs to different compatibility pools)(?:, url: \S+)?$`)

// Only inspect the model error envelope. A tool result mentioning this string
// must not trigger a context change, and ordinary authentication errors do not
// establish encrypted-context incompatibility.
func InvalidEncryptedContent(value any, depth int) bool {
	if depth > 8 {
		return false
	}
	switch value := value.(type) {
	case map[string]any:
		if value["code"] == "invalid_encrypted_content" || value["code"] == "unknown_reasoning_pool" {
			return true
		}
		for _, key := range []string{"error", "message", "additionalDetails"} {
			if InvalidEncryptedContent(value[key], depth+1) {
				return true
			}
		}
	case string:
		if len(value) > 32768 {
			return false
		}
		var parsed any
		if json.Unmarshal([]byte(value), &parsed) == nil {
			return InvalidEncryptedContent(parsed, depth+1)
		}
		return encryptedContentCode.MatchString(value) || unknownReasoningPoolMessage.MatchString(strings.TrimSpace(value))
	}
	return false
}

// This observation commits with the raw error, including with no Web client.
func recordInputFailure(ctx context.Context, tx pgx.Tx, store, thread string, generation int64, r route, m marker) error {
	_, err := tx.Exec(ctx, `INSERT INTO mira_codex_execution_events(operation_id,store_id,thread_id,generation,node_account_id,runtime_id,revision,kind,turn_id,detail)
 SELECT gen_random_uuid(),$1,$2,$3,b.node_account_id,$5,$6,'invalid_encrypted_content',$7,
 jsonb_build_object('code','invalid_encrypted_content','throughItemSeq',$8::bigint,'credentialRevision',b.credential_revision)
 FROM mira_node_codex_accounts b WHERE b.node_account_id=$4::uuid AND b.enabled
 ON CONFLICT DO NOTHING`, store, thread, generation, r.binding, r.runtime, r.revision, m.turn, m.seq)
	return err
}

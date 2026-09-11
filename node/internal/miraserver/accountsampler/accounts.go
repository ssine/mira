package accountsampler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

type scopedAccountReader interface {
	ReadAccount(context.Context, string, string, string) (channel.AccountSnapshot, error)
}

// CleanSnapshot retains only documented display/quota fields. Upstream auth
// responses and error strings must never be stored verbatim.
func CleanSnapshot(snapshot channel.AccountSnapshot) channel.AccountSnapshot {
	account := map[string]any{}
	for _, key := range []string{"type", "email", "planType"} {
		if value := safeText(accountField(snapshot.Account, key)); value != nil {
			account[key] = value
		}
	}
	var accountValue any
	if len(account) > 0 {
		accountValue = account
	}
	cleanLimit := func(value any) any {
		record, _ := value.(map[string]any)
		if record == nil {
			return nil
		}
		result := map[string]any{}
		for _, key := range []string{"limitId", "limitName", "planType"} {
			if text := safeText(record[key]); text != nil {
				result[key] = text
			}
		}
		for _, key := range []string{"primary", "secondary"} {
			window, _ := record[key].(map[string]any)
			clean := map[string]any{}
			for _, field := range []string{"usedPercent", "windowDurationMins", "resetsAt"} {
				if value, ok := finiteNumber(window[field]); ok && value >= 0 && value <= 9_007_199_254_740_991 {
					clean[field] = value
				}
			}
			if len(clean) > 0 {
				result[key] = clean
			}
		}
		return result
	}
	limits := map[string]any{}
	record, _ := snapshot.Limits.(map[string]any)
	if value := cleanLimit(record["rateLimits"]); value != nil {
		limits["rateLimits"] = value
	}
	if values, ok := record["rateLimitsByLimitId"].(map[string]any); ok && len(values) <= 64 {
		clean := map[string]any{}
		for key, value := range values {
			if safeText(key) != nil {
				clean[key] = cleanLimit(value)
			}
		}
		limits["rateLimitsByLimitId"] = clean
	}
	if credits, ok := record["rateLimitResetCredits"].(map[string]any); ok {
		if count, ok := safeNonnegativeInteger(credits["availableCount"]); ok {
			limits["rateLimitResetCredits"] = map[string]any{"availableCount": count}
		}
	}
	var limitsValue any
	if len(limits) > 0 {
		limitsValue = limits
	}
	return channel.AccountSnapshot{Account: accountValue, Limits: limitsValue}
}

func (sampler *Sampler) SampleAccount(ctx context.Context, node *nodes.Node) (bool, error) {
	reader, ok := sampler.accounts.(scopedAccountReader)
	if !ok || sampler.pool == nil || !Eligible(node, sampler.connectivity) {
		return false, nil
	}
	selected, err := nodes.SelectAccount(node, node.SelectedNodeAccountID)
	if err != nil || selected == nil {
		return false, err
	}
	slot := sampler.now().UnixMilli() / SampleInterval.Milliseconds()
	tx, err := sampler.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "mira-account-binding:"+selected.NodeAccountID).Scan(&locked); err != nil || !locked {
		return false, err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_codex_account_samples WHERE node_account_id=$1::uuid AND sample_slot=$2)`, selected.NodeAccountID, slot).Scan(&exists); err != nil || exists {
		return false, err
	}
	runtimeID, _ := node.ReportedAppServer["runtimeId"].(string)
	snapshot, readErr := reader.ReadAccount(ctx, node.NodeID, selected.NodeAccountID, runtimeID)
	status := "ok"
	if readErr != nil {
		status = "error"
		snapshot = channel.AccountSnapshot{}
	}
	if ctx.Err() != nil {
		return false, nil
	}
	current, err := sampler.nodes.Get(ctx, node.NodeID, false)
	if err != nil {
		return false, err
	}
	currentAccount, err := nodes.SelectAccount(current, selected.NodeAccountID)
	if err != nil || currentAccount == nil {
		return false, nil
	}
	current = nodes.AccountNode(current, *currentAccount)
	if !Eligible(current, sampler.connectivity) || stringRuntimeID(current.ReportedAppServer) != runtimeID || currentAccount.Revision != selected.Revision || currentAccount.CredentialRevision != selected.CredentialRevision {
		return false, nil
	}
	if provider, _ := current.ReportedAppServer["provider"].(map[string]any); provider != nil && provider["id"] != "openai" && provider["id"] != nil {
		snapshot = channel.AccountSnapshot{Account: map[string]any{"type": "providerConfig"}}
		status = "ok"
	}
	snapshot = CleanSnapshot(snapshot)
	accountJSON, _ := json.Marshal(snapshot.Account)
	limitsJSON, _ := json.Marshal(snapshot.Limits)
	identityData, _ := json.Marshal([]any{selected.AccountID, selected.CredentialRevision, accountField(snapshot.Account, "type"), accountField(snapshot.Account, "email")})
	digest := sha256.Sum256(identityData)
	identity := hex.EncodeToString(digest[:])
	timestamp := sampler.now()
	_, err = tx.Exec(ctx, `INSERT INTO mira_codex_account_samples(node_account_id,account_id,sample_slot,sampled_at,status,identity_key,account,limits)
	 VALUES($1::uuid,$2::uuid,$3,$4,$5,$6,$7::jsonb,$8::jsonb) ON CONFLICT DO NOTHING`, selected.NodeAccountID, selected.AccountID, slot, timestamp, status, identity, accountJSON, limitsJSON)
	if err != nil {
		return false, err
	}
	view, _ := json.Marshal(map[string]any{"account": snapshot.Account, "limits": snapshot.Limits, "status": status, "identityKey": identity, "quotaSupported": accountField(snapshot.Account, "type") == "chatgpt"})
	result, err := tx.Exec(ctx, `UPDATE mira_node_codex_accounts SET account_snapshot=$2::jsonb,observed_at=$3 WHERE node_account_id=$1::uuid AND revision=$4 AND credential_revision=$5`, selected.NodeAccountID, view, timestamp, selected.Revision, selected.CredentialRevision)
	if err != nil {
		return false, err
	}
	if result.RowsAffected() == 0 {
		return false, nil
	}
	// Preserve the legacy default endpoint during the rolling-upgrade window.
	if selected.IsDefault {
		quota := ProjectWeeklyQuota(snapshot.Limits)
		var reset any
		if quota.ResetsAtMillis != nil {
			reset = time.UnixMilli(*quota.ResetsAtMillis)
		}
		_, err = tx.Exec(ctx, `INSERT INTO mira_account_quota_samples(node_id,runtime_key,sampled_at,sample_slot,status,account_type,email,plan_type,remaining,resets_at,reset_count)
		 VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`, node.NodeID, RuntimeKey(node), timestamp, slot, status, safeText(accountField(snapshot.Account, "type")), safeText(accountField(snapshot.Account, "email")), safeText(accountField(snapshot.Account, "planType")), quota.Remaining, reset, quota.ResetCount)
		if err != nil {
			return false, err
		}
	}
	err = tx.Commit(ctx)
	if errors.Is(err, pgx.ErrTxClosed) {
		return false, err
	}
	return err == nil, err
}

func stringRuntimeID(reported map[string]any) string {
	value, _ := reported["runtimeId"].(string)
	return value
}

package views

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/accountsampler"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

func (service *Service) CodexAccountHistory(ctx context.Context, account *nodes.CodexAccount, rangeName string) (map[string]any, error) {
	if rangeName == "" {
		rangeName = "7d"
	}
	duration, ok := HistoryRanges[rangeName]
	if !ok {
		return nil, &foundation.HTTPError{Status: 400, Code: "invalid_request", Message: "range must be 24h, 7d or 30d"}
	}
	now := service.now()
	from := now.Add(-duration)
	rows, err := service.pool.Query(ctx, `SELECT sampled_at,status,identity_key,account,limits FROM mira_codex_account_samples WHERE node_account_id=$1::uuid AND sampled_at BETWEEN $2 AND $3 ORDER BY sampled_at LIMIT 8642`, account.NodeAccountID, from, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []map[string]any{}
	for rows.Next() {
		var at time.Time
		var status string
		var identity *string
		var accountJSON, limitsJSON []byte
		if err := rows.Scan(&at, &status, &identity, &accountJSON, &limitsJSON); err != nil {
			return nil, err
		}
		point := map[string]any{"at": at.UnixMilli(), "remaining": nil, "resetsAt": nil, "resetCount": nil, "limits": nil, "status": status}
		if status == "ok" && identity != nil && *identity == account.Snapshot["identityKey"] {
			var limits any
			_ = json.Unmarshal(limitsJSON, &limits)
			quota := accountsampler.ProjectWeeklyQuota(limits)
			point["remaining"] = quota.Remaining
			point["resetsAt"] = quota.ResetsAtMillis
			point["resetCount"] = quota.ResetCount
			point["limits"] = limits
		}
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"nodeAccountId": account.NodeAccountID, "accountId": account.AccountID, "account": account.Snapshot["account"], "range": rangeName, "from": from.UnixMilli(), "to": now.UnixMilli(), "intervalMs": SampleInterval.Milliseconds(), "points": points}, nil
}

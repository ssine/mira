package views

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

const SampleInterval = 5 * time.Minute

var HistoryRanges = map[string]time.Duration{
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// RuntimeKey isolates account samples when a Node changes its Codex runtime.
func RuntimeKey(node *nodes.Node) string {
	var codexHome, codexPath any
	if node != nil {
		if value, ok := node.ReportedAppServer["codexHome"].(string); ok {
			codexHome = value
		}
		if value, ok := node.ReportedAppServer["codexPath"].(string); ok {
			codexPath = value
		}
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode([]any{codexHome, codexPath})
	payload := bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// AccountHistory returns the bounded five-minute quota series for the Node's
// latest Codex account. Runtime paths change during upgrades and must not hide
// previous samples for the same account. Other accounts remain visible as gaps.
func (service *Service) AccountHistory(ctx context.Context, node *nodes.Node, rangeName string, current ...time.Time) (map[string]any, error) {
	if rangeName == "" {
		rangeName = "7d"
	}
	duration, ok := HistoryRanges[rangeName]
	if !ok {
		return nil, &foundation.HTTPError{Status: 400, Code: "invalid_request", Message: "range must be 24h, 7d or 30d"}
	}
	if node == nil {
		return nil, &foundation.HTTPError{Status: 404, Code: "not_found", Message: "Node not found"}
	}
	now := service.now()
	if len(current) > 0 {
		now = current[0]
	}
	from := now.Add(-duration)

	var accountType, email, planType *string
	err := service.pool.QueryRow(ctx, `SELECT account_type,email,plan_type
		FROM mira_account_quota_samples WHERE node_id=$1 AND status='ok'
		ORDER BY sampled_at DESC LIMIT 1`, node.NodeID).Scan(&accountType, &email, &planType)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var account any
	if err == nil {
		account = map[string]any{"type": dereference(accountType), "email": dereference(email), "planType": dereference(planType)}
	}

	rows, err := service.pool.Query(ctx, `SELECT sampled_at,status,account_type,email,remaining,resets_at,reset_count::text
		FROM mira_account_quota_samples WHERE node_id=$1 AND sampled_at >= $2 AND sampled_at <= $3
		ORDER BY sampled_at LIMIT 8642`, node.NodeID, from, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []map[string]any{}
	for rows.Next() {
		var sampledAt time.Time
		var status string
		var pointType, pointEmail *string
		var remaining *float64
		var resetsAt *time.Time
		var resetCount *string
		if err := rows.Scan(&sampledAt, &status, &pointType, &pointEmail, &remaining, &resetsAt, &resetCount); err != nil {
			return nil, err
		}
		matches := status == "ok" && accountType != nil && *accountType == "chatgpt" && email != nil && *email != "" &&
			pointType != nil && *pointType == *accountType && pointEmail != nil && *pointEmail == *email
		point := map[string]any{"at": sampledAt.UnixMilli(), "remaining": nil, "resetsAt": nil, "resetCount": nil}
		if matches {
			if remaining != nil {
				point["remaining"] = *remaining
			}
			if resetsAt != nil {
				point["resetsAt"] = resetsAt.UnixMilli()
			}
			if resetCount != nil {
				value, parseErr := postgresTextInt(*resetCount)
				if parseErr != nil {
					return nil, parseErr
				}
				point["resetCount"] = value
			}
		}
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{
		"account": account, "range": rangeName, "from": from.UnixMilli(), "to": now.UnixMilli(),
		"intervalMs": SampleInterval.Milliseconds(), "points": points,
	}, nil
}

func dereference(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

package views

import (
	"context"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

// Ownership is a cheap relational read over execution metadata, independent of
// token parsing. Late turn events, account renames and provider ambiguity take
// effect without replaying canonical history or freezing today's account into
// yesterday's charges. Exact turn identity wins over timestamp intervals.
const accountCostOwnersSQL = `identities AS (
 SELECT btrim(name) AS name,provider FROM mira_codex_accounts
 UNION SELECT btrim(a.name),b.reported#>>'{provider,id}' FROM mira_codex_accounts a JOIN mira_node_codex_accounts b USING(account_id)
 UNION SELECT btrim(a.name),n.reported_app_server#>>'{provider,id}' FROM mira_codex_accounts a
 JOIN mira_node_codex_accounts b USING(account_id) JOIN codex_nodes n USING(node_id) WHERE b.is_default),
 aliases AS (SELECT provider,min(name) AS name FROM identities WHERE provider<>'' GROUP BY provider HAVING count(DISTINCT name)=1),
 turns AS (SELECT DISTINCT ON(e.store_id,e.thread_id,e.generation,e.turn_id)
 e.store_id,e.thread_id,e.generation,e.turn_id,btrim(a.name) AS name
 FROM mira_codex_execution_events e JOIN mira_node_codex_accounts b USING(node_account_id) JOIN mira_codex_accounts a USING(account_id)
 WHERE e.kind IN ('turn/started','turn/completed') AND e.turn_id<>''
 ORDER BY e.store_id,e.thread_id,e.generation,e.turn_id,e.event_seq DESC),
 bindings AS (SELECT e.store_id,e.thread_id,e.generation,btrim(a.name) AS name,e.created_at,
 lead(e.created_at) OVER(PARTITION BY e.store_id,e.thread_id,e.generation ORDER BY e.created_at,e.event_seq) AS until
 FROM mira_codex_execution_events e JOIN mira_node_codex_accounts b USING(node_account_id) JOIN mira_codex_accounts a USING(account_id)
 WHERE e.kind IN ('bound','turn_requested'))`

func (service *Service) AccountCostHistory(ctx context.Context, name, rangeName, zone string) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if rangeName == "" {
		rangeName = "7d"
	}
	if zone == "" {
		zone = "UTC"
	}
	count := map[string]int{"24h": 1, "7d": 7, "30d": 30}[rangeName]
	location, err := time.LoadLocation(zone)
	if name == "" || len(name) > 128 || count == 0 || err != nil {
		return nil, &foundation.HTTPError{Status: 400, Code: "invalid_request", Message: "name, range (24h, 7d, 30d) and a valid timezone are required"}
	}
	now := service.now().In(location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	from := today.AddDate(0, 0, 1-count)
	// Progress and amounts come from one MVCC snapshot, so a concurrent page
	// commit or generation replacement cannot produce a false complete result.
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	var totalThreads, pendingThreads, failedThreads, processedItems, totalItems int64
	err = tx.QueryRow(ctx, `WITH sources AS (`+accountCostSourcesSQL+`)
 SELECT count(*),count(*) FILTER(WHERE c.thread_id IS NULL OR c.generation<>s.active_generation OR c.revision<>$1 OR c.source_key<>s.source_key OR c.item_seq<>s.item_count),
 count(*) FILTER(WHERE c.error_code IS NOT NULL),
 coalesce(sum(CASE WHEN c.generation=s.active_generation AND c.revision=$1 AND c.source_key=s.source_key THEN least(c.item_seq,s.item_count) ELSE 0 END),0)::bigint,
 coalesce(sum(s.item_count),0)::bigint
 FROM sources s LEFT JOIN mira_account_cost_checkpoints c USING(store_id,thread_id)`, accountCostRevision).
		Scan(&totalThreads, &pendingThreads, &failedThreads, &processedItems, &totalItems)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `WITH sources AS (`+accountCostSourcesSQL+`),`+accountCostOwnersSQL+`, facts AS (
 SELECT f.*,t.name IS NULL AND b.name IS NULL AS historical,
 to_char(f.happened_at AT TIME ZONE $5,'YYYY-MM-DD') AS day,
 date_trunc('hour',f.happened_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS hour
 FROM mira_account_cost_entries f JOIN mira_account_cost_checkpoints c USING(store_id,thread_id)
 JOIN sources s USING(store_id,thread_id)
 LEFT JOIN turns t ON t.store_id=f.store_id AND t.thread_id=f.thread_id AND t.generation=c.generation AND t.turn_id=f.turn_id
 LEFT JOIN bindings b ON t.name IS NULL AND b.store_id=f.store_id AND b.thread_id=f.thread_id AND b.generation=c.generation
 AND f.happened_at>=b.created_at AND (b.until IS NULL OR f.happened_at<b.until)
 LEFT JOIN aliases a ON a.provider=f.provider
 WHERE c.generation=s.active_generation AND c.revision=$1 AND c.source_key=s.source_key AND f.item_seq<=s.item_count
 AND f.happened_at>=$3 AND f.happened_at<=$4 AND coalesce(t.name,b.name,a.name)=$2)
 SELECT day,hour,historical,models,reasons,bool_or(observed),sum(priced)::bigint,sum(unpriced)::bigint,sum(long_requests)::bigint,
 sum(input)::text,sum(cached)::text,sum(write)::text,sum(output)::text
 FROM facts GROUP BY day,hour,historical,models,reasons`, accountCostRevision, name, from, now, zone)
	if err != nil {
		return nil, err
	}
	days := make([]map[string]any, count)
	totals := make([]costTotals, count)
	hours := make([]map[int64]*costTotals, count)
	for i := range days {
		start := from.AddDate(0, 0, i)
		days[i] = map[string]any{"date": start.Format("2006-01-02"), "at": start.UnixMilli(), "end": start.AddDate(0, 0, 1).UnixMilli()}
		hours[i] = map[int64]*costTotals{}
	}
	for rows.Next() {
		var day string
		var hour time.Time
		var historical bool
		var numbers accountCostNumbers
		if err := rows.Scan(&day, &hour, &historical, &numbers.Models, &numbers.Reasons, &numbers.Observed, &numbers.Priced, &numbers.Unpriced, &numbers.LongRequests,
			&numbers.Input, &numbers.Cached, &numbers.Write, &numbers.Output); err != nil {
			rows.Close()
			return nil, err
		}
		delta, err := numbers.totals()
		if err != nil {
			rows.Close()
			return nil, err
		}
		if historical {
			incomplete(&delta, "historical_provider_attribution")
		}
		for i, value := range days {
			if value["date"] != day {
				continue
			}
			mergeAccountCost(&totals[i], delta)
			bucket := hour.Add(time.Hour)
			if end := time.UnixMilli(value["end"].(int64)); bucket.After(end) {
				bucket = end
			}
			if bucket.After(now) {
				bucket = now
			}
			key := bucket.UnixMilli()
			if hours[i][key] == nil {
				hours[i][key] = &costTotals{}
			}
			mergeAccountCost(hours[i][key], delta)
			break
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	points := []map[string]any{}
	var total costTotals
	for i := range days {
		if pendingThreads > 0 {
			incomplete(&totals[i], "projection_pending")
		}
		estimate := pricedEstimate(&totals[i])
		days[i]["amount"], days[i]["status"] = estimate["amount"], estimate["status"]
		mergeAccountCost(&total, totals[i])
		for at, value := range hours[i] {
			if pendingThreads > 0 {
				incomplete(value, "projection_pending")
			}
			estimate := pricedEstimate(value)
			points = append(points, map[string]any{"at": at, "amount": estimate["amount"], "status": estimate["status"], "date": days[i]["date"]})
		}
	}
	sort.Slice(points, func(i, j int) bool { return points[i]["at"].(int64) < points[j]["at"].(int64) })
	status := "ready"
	if pendingThreads > 0 {
		status = "updating"
	}
	if failedThreads > 0 {
		status = "retrying"
	}
	return map[string]any{"name": name, "range": rangeName, "timezone": zone, "from": from.UnixMilli(), "to": now.UnixMilli(),
		"days": days, "points": points, "estimate": pricedEstimate(&total), "basis": "standard", "currency": "USD",
		"projection": map[string]any{"status": status, "pendingThreads": pendingThreads, "totalThreads": totalThreads, "failedThreads": failedThreads,
			"processedItems": processedItems, "totalItems": totalItems}}, nil
}

func mergeAccountCost(target *costTotals, value costTotals) {
	target.input.Add(&target.input, &value.input)
	target.cached.Add(&target.cached, &value.cached)
	target.write.Add(&target.write, &value.write)
	target.output.Add(&target.output, &value.output)
	target.observed = target.observed || value.observed
	target.priced += value.priced
	target.unpriced += value.unpriced
	target.longRequests += value.longRequests
	for _, model := range value.models {
		target.models = appendUnique(target.models, model)
	}
	for _, reason := range value.reasons {
		incomplete(target, reason)
	}
}

func accountCostDelta(before costTotals, after costTotals) costTotals {
	var delta costTotals
	delta.input.Sub(&after.input, &before.input)
	delta.cached.Sub(&after.cached, &before.cached)
	delta.write.Sub(&after.write, &before.write)
	delta.output.Sub(&after.output, &before.output)
	delta.priced, delta.unpriced = after.priced-before.priced, after.unpriced-before.unpriced
	delta.longRequests = after.longRequests - before.longRequests
	delta.models = after.models
	delta.observed = delta.priced > 0 || delta.unpriced > 0 || !before.observed && after.observed
	if delta.observed || len(after.reasons) > len(before.reasons) {
		delta.reasons = after.reasons
	}
	return delta
}

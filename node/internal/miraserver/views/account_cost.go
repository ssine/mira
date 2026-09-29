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

// AccountCostSnapshot is immutable after loading. One database aggregation serves
// every account and supported range in a timezone. History only slices/merges
// these bounded daily/hourly totals; it never reads PostgreSQL.
type AccountCostSnapshot struct {
	now, from                                                               time.Time
	zone                                                                    string
	accounts                                                                map[string][]accountCostDay
	totalThreads, pendingThreads, failedThreads, processedItems, totalItems int64
}

type accountCostDay struct {
	total costTotals
	hours map[int64]*costTotals
}

func (service *Service) AccountCostHistory(ctx context.Context, name, rangeName, zone string) (map[string]any, error) {
	if _, err := accountCostRange(name, rangeName); err != nil {
		return nil, err
	}
	snapshot, err := service.LoadAccountCostSnapshot(ctx, zone)
	if err != nil {
		return nil, err
	}
	return snapshot.History(name, rangeName)
}

func accountCostRange(name, rangeName string) (int, error) {
	if rangeName == "" {
		rangeName = "7d"
	}
	count := map[string]int{"24h": 1, "7d": 7, "30d": 30}[rangeName]
	if name = strings.TrimSpace(name); name == "" || len(name) > 128 || count == 0 {
		return 0, &foundation.HTTPError{Status: 400, Code: "invalid_request", Message: "name and range (24h, 7d, 30d) are required"}
	}
	return count, nil
}

func (service *Service) LoadAccountCostSnapshot(ctx context.Context, zone string) (*AccountCostSnapshot, error) {
	if zone == "" {
		zone = "UTC"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, &foundation.HTTPError{Status: 400, Code: "invalid_request", Message: "a valid timezone is required"}
	}
	now := service.now().In(location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	from := today.AddDate(0, 0, -29)
	snapshot := &AccountCostSnapshot{now: now, from: from, zone: zone, accounts: map[string][]accountCostDay{}}
	// Progress and amounts come from one MVCC snapshot, so a concurrent page
	// commit or generation replacement cannot produce a false complete result.
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	err = tx.QueryRow(ctx, `WITH sources AS (`+accountCostSourcesSQL+`)
 SELECT count(*),count(*) FILTER(WHERE c.thread_id IS NULL OR c.generation<>s.active_generation OR c.revision<>$1 OR c.source_key<>s.source_key OR c.item_seq<>s.item_count),
 count(*) FILTER(WHERE c.error_code IS NOT NULL),
 coalesce(sum(CASE WHEN c.generation=s.active_generation AND c.revision=$1 AND c.source_key=s.source_key THEN least(c.item_seq,s.item_count) ELSE 0 END),0)::bigint,
 coalesce(sum(s.item_count),0)::bigint
 FROM sources s LEFT JOIN mira_account_cost_checkpoints c USING(store_id,thread_id)`, accountCostRevision).
		Scan(&snapshot.totalThreads, &snapshot.pendingThreads, &snapshot.failedThreads, &snapshot.processedItems, &snapshot.totalItems)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `WITH sources AS (`+accountCostSourcesSQL+`),`+accountCostOwnersSQL+`, facts AS (
 SELECT f.*,coalesce(t.name,b.name,a.name) AS name,t.name IS NULL AND b.name IS NULL AS historical,
 to_char(f.happened_at AT TIME ZONE $4,'YYYY-MM-DD') AS day,
 date_trunc('hour',f.happened_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS hour
 FROM mira_account_cost_entries f JOIN mira_account_cost_checkpoints c USING(store_id,thread_id)
 JOIN sources s USING(store_id,thread_id)
 LEFT JOIN turns t ON t.store_id=f.store_id AND t.thread_id=f.thread_id AND t.generation=c.generation AND t.turn_id=f.turn_id
 LEFT JOIN bindings b ON t.name IS NULL AND b.store_id=f.store_id AND b.thread_id=f.thread_id AND b.generation=c.generation
 AND f.happened_at>=b.created_at AND (b.until IS NULL OR f.happened_at<b.until)
 LEFT JOIN aliases a ON a.provider=f.provider
 WHERE c.generation=s.active_generation AND c.revision=$1 AND c.source_key=s.source_key AND f.item_seq<=s.item_count
 AND f.happened_at>=$2 AND f.happened_at<=$3 AND coalesce(t.name,b.name,a.name) IS NOT NULL)
 SELECT name,day,hour,historical,models,reasons,bool_or(observed),sum(priced)::bigint,sum(unpriced)::bigint,sum(long_requests)::bigint,
 sum(input)::text,sum(cached)::text,sum(write)::text,sum(output)::text
 FROM facts GROUP BY name,day,hour,historical,models,reasons`, accountCostRevision, from, now, zone)
	if err != nil {
		return nil, err
	}
	dayIndex := make(map[string]int, 30)
	for i := 0; i < 30; i++ {
		dayIndex[from.AddDate(0, 0, i).Format("2006-01-02")] = i
	}
	for rows.Next() {
		var name, day string
		var hour time.Time
		var historical bool
		var numbers accountCostNumbers
		if err := rows.Scan(&name, &day, &hour, &historical, &numbers.Models, &numbers.Reasons, &numbers.Observed, &numbers.Priced, &numbers.Unpriced, &numbers.LongRequests,
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
		i, ok := dayIndex[day]
		if !ok {
			continue
		}
		days := snapshot.accounts[name]
		if days == nil {
			days = make([]accountCostDay, 30)
			snapshot.accounts[name] = days
		}
		value := &days[i]
		mergeAccountCost(&value.total, delta)
		bucket := hour.Add(time.Hour)
		if end := from.AddDate(0, 0, i+1); bucket.After(end) {
			bucket = end
		}
		if bucket.After(now) {
			bucket = now
		}
		key := bucket.UnixMilli()
		if value.hours == nil {
			value.hours = map[int64]*costTotals{}
		}
		if value.hours[key] == nil {
			value.hours[key] = &costTotals{}
		}
		mergeAccountCost(value.hours[key], delta)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (snapshot *AccountCostSnapshot) History(name, rangeName string) (map[string]any, error) {
	count, err := accountCostRange(name, rangeName)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if rangeName == "" {
		rangeName = "7d"
	}
	from := snapshot.from.AddDate(0, 0, 30-count)
	days := make([]map[string]any, count)
	points := []map[string]any{}
	var total costTotals
	for i := range days {
		start := from.AddDate(0, 0, i)
		date := start.Format("2006-01-02")
		var value accountCostDay
		if account := snapshot.accounts[name]; account != nil {
			value = account[30-count+i]
		}
		// Copy totals before adding presentation-only completeness reasons. A
		// caller must never mutate the snapshot shared by other requests.
		var dayTotal costTotals
		mergeAccountCost(&dayTotal, value.total)
		if snapshot.pendingThreads > 0 {
			incomplete(&dayTotal, "projection_pending")
		}
		estimate := pricedEstimate(&dayTotal)
		days[i] = map[string]any{"date": date, "at": start.UnixMilli(), "end": start.AddDate(0, 0, 1).UnixMilli(),
			"amount": estimate["amount"], "status": estimate["status"]}
		mergeAccountCost(&total, dayTotal)
		for at, hour := range value.hours {
			var hourTotal costTotals
			mergeAccountCost(&hourTotal, *hour)
			if snapshot.pendingThreads > 0 {
				incomplete(&hourTotal, "projection_pending")
			}
			estimate := pricedEstimate(&hourTotal)
			points = append(points, map[string]any{"at": at, "amount": estimate["amount"], "status": estimate["status"], "date": date})
		}
	}
	sort.Slice(points, func(i, j int) bool { return points[i]["at"].(int64) < points[j]["at"].(int64) })
	status := "ready"
	if snapshot.pendingThreads > 0 {
		status = "updating"
	}
	if snapshot.failedThreads > 0 {
		status = "retrying"
	}
	return map[string]any{"name": name, "range": rangeName, "timezone": snapshot.zone, "from": from.UnixMilli(), "to": snapshot.now.UnixMilli(),
		"days": days, "points": points, "estimate": pricedEstimate(&total), "basis": "standard", "currency": "USD",
		"projection": map[string]any{"status": status, "pendingThreads": snapshot.pendingThreads, "totalThreads": snapshot.totalThreads, "failedThreads": snapshot.failedThreads,
			"processedItems": snapshot.processedItems, "totalItems": snapshot.totalItems}}, nil
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

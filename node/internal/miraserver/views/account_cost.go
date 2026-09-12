package views

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // Calendar-day views also work on hosts without zoneinfo.

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

const accountCostPredicate = `payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(session_meta|turn_context|thread_settings_applied|token_count)"'`

// AccountCostHistory is a read projection. Account names combine Node bindings,
// while every canonical request is priced only once in its own generation.
func (service *Service) AccountCostHistory(ctx context.Context, name, rangeName, zone string) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if rangeName == "" {
		rangeName = "7d"
	}
	count := map[string]int{"24h": 1, "7d": 7, "30d": 30}[rangeName]
	if zone == "" {
		zone = "UTC"
	}
	location, err := time.LoadLocation(zone)
	if name == "" || len(name) > 128 || count == 0 || err != nil {
		return nil, &foundation.HTTPError{Status: 400, Code: "invalid_request", Message: "name, range (24h, 7d, 30d) and a valid timezone are required"}
	}
	// Older history predates named execution accounts. Only an unambiguous
	// provider-to-name mapping can recover its ownership; never use today's route.
	providers := []string{}
	providerRows, err := service.pool.Query(ctx, `WITH identities AS (
	 SELECT btrim(name) AS name,provider FROM mira_codex_accounts
	 UNION SELECT btrim(a.name),b.reported#>>'{provider,id}' FROM mira_codex_accounts a JOIN mira_node_codex_accounts b USING(account_id)
	 UNION SELECT btrim(a.name),n.reported_app_server#>>'{provider,id}' FROM mira_codex_accounts a
	 JOIN mira_node_codex_accounts b USING(account_id) JOIN codex_nodes n USING(node_id) WHERE b.is_default)
	 SELECT provider FROM identities WHERE provider<>'' GROUP BY provider HAVING count(DISTINCT name)=1 AND min(name)=$1`, name)
	if err != nil {
		return nil, err
	}
	for providerRows.Next() {
		var provider string
		if err := providerRows.Scan(&provider); err != nil {
			providerRows.Close()
			return nil, err
		}
		providers = append(providers, provider)
	}
	err = providerRows.Err()
	providerRows.Close()
	if err != nil {
		return nil, err
	}
	now := service.now().In(location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	from := today.AddDate(0, 0, 1-count)
	days := make([]map[string]any, count)
	totals := make([]costTotals, count)
	hours := make([]map[int64]*costTotals, count)
	for i := range days {
		start := from.AddDate(0, 0, i)
		days[i] = map[string]any{"date": start.Format("2006-01-02"), "at": start.UnixMilli(), "end": start.AddDate(0, 0, 1).UnixMilli()}
		hours[i] = map[int64]*costTotals{}
	}
	afterStore, afterThread := "", ""
	for {
		rows, err := service.pool.Query(ctx, `SELECT p.store_id,p.thread_id,p.active_generation,p.item_count,p.state #>> '{createdThread,forked_from_id}'
		 FROM codex_thread_projections p WHERE (p.store_id,p.thread_id)>($2,$3) AND EXISTS(
		 SELECT 1 FROM mira_codex_execution_events e JOIN mira_node_codex_accounts b USING(node_account_id)
		 JOIN mira_codex_accounts a USING(account_id) WHERE e.store_id=p.store_id AND e.thread_id=p.thread_id
		 AND e.generation=p.active_generation AND btrim(a.name)=$1)
		 OR (p.store_id,p.thread_id)>($2,$3) AND cardinality($4::text[])>0 AND EXISTS(
		 SELECT 1 FROM codex_thread_events h WHERE h.store_id=p.store_id AND h.thread_id=p.thread_id AND h.generation=p.active_generation
		 AND h.payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(session_meta|thread_settings_applied)"'
		 AND EXISTS(SELECT 1 FROM unnest($4::text[]) provider WHERE strpos(h.payload::text,to_json(provider)::text)>0))
		 ORDER BY p.store_id,p.thread_id LIMIT 128`, name, afterStore, afterThread, providers)
		if err != nil {
			return nil, err
		}
		type candidate struct {
			store  string
			thread Thread
		}
		page := []candidate{}
		for rows.Next() {
			var value candidate
			if err := rows.Scan(&value.store, &value.thread.ThreadID, &value.thread.Generation, &value.thread.ItemCount, &value.thread.ForkedFromID); err != nil {
				rows.Close()
				return nil, err
			}
			page = append(page, value)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, value := range page {
			state := NewCostProjection(value.thread.ForkedFromID != nil && *value.thread.ForkedFromID != "", []string{})
			err := service.accountCostRecords(ctx, value.store, value.thread, state, name, providers, from, now, func(at time.Time, delta costTotals) {
				date := at.In(location).Format("2006-01-02")
				for i, day := range days {
					if day["date"] != date {
						continue
					}
					mergeAccountCost(&totals[i], delta)
					// Hour buckets keep responses bounded, including long conversations.
					bucket := at.Truncate(time.Hour).Add(time.Hour)
					if end := time.UnixMilli(day["end"].(int64)); bucket.After(end) {
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
			})
			if err != nil {
				return nil, err
			}
			afterStore, afterThread = value.store, value.thread.ThreadID
		}
		if len(page) < 128 {
			break
		}
	}
	points := []map[string]any{}
	var total costTotals
	for i := range days {
		estimate := pricedEstimate(&totals[i])
		days[i]["amount"], days[i]["status"] = estimate["amount"], estimate["status"]
		mergeAccountCost(&total, totals[i])
		for at, value := range hours[i] {
			point := pricedEstimate(value)
			points = append(points, map[string]any{"at": at, "amount": point["amount"], "status": point["status"], "date": days[i]["date"]})
		}
	}
	sort.Slice(points, func(i, j int) bool { return points[i]["at"].(int64) < points[j]["at"].(int64) })
	return map[string]any{"name": name, "range": rangeName, "timezone": zone, "from": from.UnixMilli(), "to": now.UnixMilli(),
		"days": days, "points": points, "estimate": pricedEstimate(&total), "basis": "standard", "currency": "USD"}, nil
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

func (service *Service) accountCostRecords(ctx context.Context, store string, thread Thread, state *CostProjection, name string, providers []string, from, to time.Time, consume func(time.Time, costTotals)) error {
	cursor := int64(0)
	var cachedTurn string
	var cachedOwner, bindingOwner *string
	var cachedTurnSet, bindingCached bool
	var bindingFrom, bindingUntil *time.Time
	var provider string
	var hasExecution bool
	if err := service.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_codex_execution_events WHERE store_id=$1 AND thread_id=$2 AND generation=$3)`, store, thread.ThreadID, thread.Generation).Scan(&hasExecution); err != nil {
		return err
	}
	for cursor < thread.ItemCount {
		rows, err := service.pool.Query(ctx, `SELECT item_seq,payload,created_at FROM codex_thread_events WHERE store_id=$1 AND thread_id=$2 AND generation=$3
		 AND item_seq>$4 AND item_seq<=$5 AND `+accountCostPredicate+` ORDER BY item_seq LIMIT 256`, store, thread.ThreadID, thread.Generation, cursor, thread.ItemCount)
		if err != nil {
			return err
		}
		page := []transcriptRow{}
		for rows.Next() {
			var raw []byte
			var created time.Time
			if err := rows.Scan(&cursor, &raw, &created); err != nil {
				rows.Close()
				return err
			}
			record, err := decodeObject(raw)
			if err != nil {
				rows.Close()
				return err
			}
			page = append(page, transcriptRow{cursor, record, &created})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		recordedAt, err := service.restoreImportedTimestamps(ctx, store, thread.ThreadID, page)
		if err != nil {
			return err
		}
		for _, row := range page {
			record := row.payload
			payload := object(record["payload"])
			if stringValue(record["type"]) == "session_meta" {
				provider = stringValue(payload["model_provider"])
			} else if stringValue(record["type"]) == "event_msg" && stringValue(payload["type"]) == "thread_settings_applied" {
				if value := stringValue(object(payload["thread_settings"])["model_provider_id"]); value != "" {
					provider = value
				}
			}
			before := cloneCostTotals(state.costTotals)
			ApplyCostRecord(state, record, thread.ThreadID)
			if !state.scopeStarted {
				continue
			}
			delta := accountCostDelta(before, state.costTotals)
			if !delta.observed && len(delta.reasons) == 0 {
				continue
			}
			timestamp := recordTimestamp(record)
			if timestamp == "" {
				timestamp = recordedAt[row.sequence]
			}
			at, err := time.Parse(time.RFC3339Nano, timestamp)
			if err != nil || at.Before(from) || at.After(to) {
				continue
			}
			// Exact turn ownership survives account switches and clock skew. A
			// timestamp fallback is only for bound child runtimes lacking turn events.
			var owner *string
			if hasExecution {
				if !cachedTurnSet || cachedTurn != state.turnID {
					cachedTurn, cachedTurnSet, cachedOwner = state.turnID, true, nil
					err = service.pool.QueryRow(ctx, `SELECT btrim(a.name) FROM mira_codex_execution_events e
				 JOIN mira_node_codex_accounts b USING(node_account_id) JOIN mira_codex_accounts a USING(account_id)
				 WHERE e.store_id=$1 AND e.thread_id=$2 AND e.generation=$3 AND e.turn_id=$4 AND $4<>''
				 AND e.kind IN ('turn/started','turn/completed') ORDER BY e.event_seq DESC LIMIT 1`,
						store, thread.ThreadID, thread.Generation, state.turnID).Scan(&cachedOwner)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						return err
					}
				}
				owner = cachedOwner
				if owner == nil {
					if !bindingCached || bindingFrom != nil && at.Before(*bindingFrom) || bindingUntil != nil && !at.Before(*bindingUntil) {
						err = service.pool.QueryRow(ctx, `SELECT previous.name,previous.created_at,following.created_at FROM (SELECT 1) seed
					 LEFT JOIN LATERAL (SELECT btrim(a.name) AS name,e.created_at FROM mira_codex_execution_events e
					 JOIN mira_node_codex_accounts b USING(node_account_id) JOIN mira_codex_accounts a USING(account_id)
					 WHERE e.store_id=$1 AND e.thread_id=$2 AND e.generation=$3 AND e.kind IN ('bound','turn_requested') AND e.created_at<=$4
					 ORDER BY e.created_at DESC,e.event_seq DESC LIMIT 1) previous ON true
					 LEFT JOIN LATERAL (SELECT min(created_at) AS created_at FROM mira_codex_execution_events
					 WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND kind IN ('bound','turn_requested') AND created_at>$4) following ON true`,
							store, thread.ThreadID, thread.Generation, at).Scan(&bindingOwner, &bindingFrom, &bindingUntil)
						if err != nil {
							return err
						}
						bindingCached = true
					}
					owner = bindingOwner
				}
			}
			if owner == nil && includes(providers, provider) {
				owner = &name
				incomplete(&delta, "historical_provider_attribution")
			}
			if owner != nil && *owner == name {
				consume(at, delta)
			}
		}
		if len(page) < 256 {
			break
		}
	}
	return nil
}

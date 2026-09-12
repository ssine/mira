package views

import "context"

type costSum struct {
	total     costTotals
	available bool
}

func (sum *costSum) add(state *CostProjection, estimate map[string]any) {
	total := &sum.total
	total.input.Add(&total.input, &state.input)
	total.cached.Add(&total.cached, &state.cached)
	total.write.Add(&total.write, &state.write)
	total.output.Add(&total.output, &state.output)
	total.priced += state.priced
	total.unpriced += state.unpriced
	total.longRequests += state.longRequests
	for _, model := range state.models {
		total.models = appendUnique(total.models, model)
	}
	for _, reason := range estimate["reasons"].([]string) {
		incomplete(total, reason)
	}
	if estimate["status"] == "unavailable" {
		incomplete(total, "missing_usage")
	}
	sum.available = sum.available || estimate["amount"] != nil
}

func (sum *costSum) estimate() map[string]any {
	estimate := pricedEstimate(&sum.total)
	if sum.available {
		estimate["amount"] = totalDollars(&sum.total)
		estimate["status"] = "complete"
		if len(sum.total.reasons) > 0 {
			estimate["status"] = "partial"
		}
	} else {
		estimate["amount"] = nil
		estimate["status"] = "unavailable"
	}
	return estimate
}

// Sum each thread's own canonical usage once, including archived descendants.
// Do not sum already-aggregated child estimates or copied pre-fork requests.
func (service *Service) includeSubagentCosts(ctx context.Context, storeID string, root Thread, own *CostProjection) (map[string]any, error) {
	self := CostEstimate(own, root)
	var total, children costSum
	total.add(own, self)
	count, after := 0, ""
	for {
		page, err := service.costDescendants(ctx, storeID, root.ThreadID, after)
		if err != nil {
			return nil, err
		}
		for _, child := range page {
			state, err := service.costProjection(ctx, storeID, child)
			if err != nil {
				return nil, err
			}
			estimate := CostEstimate(state, child)
			total.add(state, estimate)
			children.add(state, estimate)
			count++
			after = child.ThreadID
		}
		if len(page) < 256 {
			break
		}
	}
	if count == 0 {
		return self, nil
	}
	estimate := total.estimate()
	estimate["scope"] = self["scope"]
	estimate["includesSubagents"] = true
	estimate["subagentCount"] = count
	estimate["selfAmount"] = self["amount"]
	estimate["subagentAmount"] = children.estimate()["amount"]
	return estimate, nil
}

func (service *Service) costDescendants(ctx context.Context, storeID, rootID, after string) ([]Thread, error) {
	// UNION deduplicates identities and terminates even if imported parent links
	// contain a cycle. Page metadata so large trees never buffer their histories.
	rows, err := service.pool.Query(ctx, `WITH RECURSIVE family(thread_id) AS (
	 SELECT $2::text UNION
	 SELECT child.thread_id FROM codex_thread_projections child JOIN family parent ON child.parent_thread_id=parent.thread_id
	 WHERE child.store_id=$1
	) SELECT p.thread_id,p.active_generation,p.item_count,p.state #>> '{createdThread,forked_from_id}',
	 p.state #> '{metadata,token_usage}' FROM family JOIN codex_thread_projections p USING(thread_id)
	 WHERE p.store_id=$1 AND p.thread_id<>$2 AND p.thread_id>$3 ORDER BY p.thread_id LIMIT 256`, storeID, rootID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := []Thread{}
	for rows.Next() {
		var thread Thread
		var usage []byte
		if err := rows.Scan(&thread.ThreadID, &thread.Generation, &thread.ItemCount, &thread.ForkedFromID, &usage); err != nil {
			return nil, err
		}
		thread.TokenUsage, err = decodeObject(usage)
		if err != nil {
			return nil, err
		}
		page = append(page, thread)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return service.addTokenUsage(ctx, storeID, page)
}

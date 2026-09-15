package views

import "math/big"

var tokenSummaryFields = []string{"inputTokens", "cachedInputTokens", "outputTokens"}

// ownTokenUsage uses cumulative counters, not a sum of cumulative snapshots or
// priced requests. Known usage remains available even with an unknown model or
// missing per-request prices. Forks subtract the copied prefix at the durable
// child-owned settings boundary; an unprovable boundary is never counted twice.
func ownTokenUsage(state *CostProjection, thread Thread) map[string]any {
	result := map[string]any{}
	positions := []int{0, 1, 3}
	for index, field := range tokenSummaryFields {
		value, known := safeInteger(thread.TokenUsage[field])
		known = known && value >= 0
		if !known && thread.TokenUsage == nil && (state.observed || state.awaitForkBoundary && state.scopeStarted) {
			value, known = state.last[positions[index]], true
		}
		if state.awaitForkBoundary {
			if !state.scopeStarted || state.forkUsageReset {
				known = false
			} else {
				value -= state.forkUsageBaseline[positions[index]]
				known = known && value >= 0
			}
		}
		result[field] = nil
		if known {
			result[field] = value
		}
	}
	return result
}

type tokenSum struct {
	values [3]big.Int
	known  [3]int
	count  int
}

func (sum *tokenSum) add(usage map[string]any) {
	sum.count++
	for index, field := range tokenSummaryFields {
		if value, ok := safeInteger(usage[field]); ok && value >= 0 {
			sum.values[index].Add(&sum.values[index], big.NewInt(value))
			sum.known[index]++
		}
	}
}

func (sum *tokenSum) summary() map[string]any {
	result := map[string]any{}
	complete, available := true, false
	for index, field := range tokenSummaryFields {
		result[field] = nil
		value := &sum.values[index]
		// JSON consumers must receive exact safe integers, including after a
		// large tree sum; do not silently round or overflow counters.
		if (sum.known[index] > 0 || sum.count == 0) && value.IsInt64() && value.Int64() <= 9_007_199_254_740_991 {
			result[field] = value.Int64()
			available = true
		} else {
			complete = false
		}
		if sum.known[index] != sum.count {
			complete = false
		}
	}
	result["status"] = "complete"
	if !available {
		result["status"] = "unavailable"
	} else if !complete {
		result["status"] = "partial"
	}
	return result
}

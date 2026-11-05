package views

import (
	"math/big"
	"strconv"
	"testing"
)

func usage(input, cached, output int64, write ...int64) map[string]any {
	value := map[string]any{"input_tokens": input, "cached_input_tokens": cached, "output_tokens": output}
	if len(write) > 0 {
		value["cache_write_input_tokens"] = write[0]
	}
	return value
}

func contextRecord(model, turnID string) map[string]any {
	payload := map[string]any{"model": model}
	if turnID != "" {
		payload["turn_id"] = turnID
	}
	return map[string]any{"type": "turn_context", "payload": payload}
}

func usageRecord(total, last map[string]any, turnID string) map[string]any {
	payload := map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": total, "last_token_usage": last}}
	if turnID != "" {
		payload["turn_id"] = turnID
	}
	return map[string]any{"type": "event_msg", "payload": payload}
}

func closeFloat(left, right float64) bool {
	delta := left - right
	if delta < 0 {
		delta = -delta
	}
	return delta < 1e-12
}

func TestDeepSeekPeakPriceIncludesCacheAndReasoningOutput(t *testing.T) {
	for _, model := range []string{"DeepSeek-V4.1-Flash", "DeepSeek-V4.1-Flash-Alt", "deepseek-flash"} {
		state := NewCostProjection(false, nil)
		ApplyCostRecord(state, contextRecord(model, "deepseek"), "")
		counts := usage(1_000_000, 800_000, 100_000)
		counts["reasoning_output_tokens"] = int64(60_000) // Already included in output.
		ApplyCostRecord(state, usageRecord(counts, counts, ""), "")
		value := CostEstimate(state, Thread{})
		if !closeFloat(value["amount"].(float64), .1848) || value["basis"] != "standard-peak" || value["pricingSource"] != DeepSeekPricingSource || value["longRequests"] != int64(0) {
			t.Fatalf("DeepSeek peak estimate: %#v", value)
		}
		ApplyCostRecord(state, contextRecord("gpt-6.1-sol", "other"), "")
		ApplyCostRecord(state, usageRecord(usage(1_001_000, 800_000, 100_100), usage(1000, 0, 100), ""), "")
		mixed := CostEstimate(state, Thread{})
		if mixed["basis"] != "mixed-standard" || len(mixed["pricingSources"].([]string)) != 2 {
			t.Fatalf("mixed provider sources: %#v", mixed)
		}
	}
}

func TestCostClonesKeepEveryRequestDeltaIndependent(t *testing.T) {
	state := NewCostProjection(false, nil)
	ApplyCostRecord(state, contextRecord("gpt-6-astra", "turn"), "")
	var total costTotals
	for i := int64(1); i <= 40; i++ {
		before := cloneCostTotals(state.costTotals)
		clone := state.clone()
		ApplyCostRecord(state, usageRecord(usage(i*100000, i*80000, i*1000), usage(100000, 80000, 1000), ""), "")
		delta := accountCostDelta(before, state.costTotals)
		if value := pricedEstimate(&delta); !closeFloat(value["amount"].(float64), .33) {
			t.Fatalf("request %d lost its delta: %#v", i, value)
		}
		if before.input.Cmp(&clone.input) != 0 || before.cached.Cmp(&clone.cached) != 0 || before.output.Cmp(&clone.output) != 0 {
			t.Fatal("sampling mutated the previous cached projection")
		}
		mergeAccountCost(&total, delta)
	}
	if value := pricedEstimate(&total); !closeFloat(value["amount"].(float64), 13.2) {
		t.Fatalf("request total: %#v", value)
	}
}

func TestCostProjectionPricesModelsTurnsAndRepeatedSnapshots(t *testing.T) {
	state := NewCostProjection(false, nil)
	ApplyCostRecord(state, contextRecord("gpt-6-astra", "turn-a"), "")
	first := usageRecord(usage(100000, 80000, 1000), usage(100000, 80000, 1000), "")
	ApplyCostRecord(state, first, "")
	ApplyCostRecord(state, first, "")
	ApplyCostRecord(state, contextRecord("gpt-5.6-sol", "turn-b"), "")
	ApplyCostRecord(state, usageRecord(usage(150000, 120000, 1500), usage(50000, 40000, 500), ""), "")
	estimate := CostEstimate(state, Thread{})
	if !closeFloat(estimate["amount"].(float64), .396) || estimate["status"] != "complete" || estimate["pricedRequests"] != int64(2) {
		t.Fatalf("unexpected estimate: %#v", estimate)
	}
	turns := TurnCostEstimates(state)
	if !closeFloat(turns["turn-a"]["amount"].(float64), .33) || !closeFloat(turns["turn-b"]["amount"].(float64), .066) {
		t.Fatalf("unexpected turns: %#v", turns)
	}
}

func TestCostProjectionPricesCurrentSolAndLunaModels(t *testing.T) {
	for _, fixture := range []struct {
		model   string
		amounts [4]float64
	}{
		{"gpt-6.1-sol", [4]float64{.058, .0605, .1765, .348004}},
		{"gpt-6-sol", [4]float64{.066, .0685, .1965, .388004}},
		{"gpt-6-luna", [4]float64{.0033, .003425, .009825, .0194002}},
	} {
		t.Run(fixture.model, func(t *testing.T) {
			state := NewCostProjection(false, nil)
			var cumulative [4]int64
			var expected float64
			for index, request := range [][4]int64{
				{100000, 80000, 0, 1000},
				{100000, 80000, 5000, 1000},
				{272000, 200000, 5000, 1000},
				{272001, 200000, 5000, 1000},
			} {
				turn := "turn-" + strconv.Itoa(index)
				ApplyCostRecord(state, contextRecord(fixture.model, turn), "")
				for component, count := range request {
					cumulative[component] += count
				}
				record := usageRecord(usage(cumulative[0], cumulative[1], cumulative[3], cumulative[2]),
					usage(request[0], request[1], request[3], request[2]), turn)
				ApplyCostRecord(state, record, "")
				ApplyCostRecord(state, record, "") // Repeated snapshots must not double the price.
				estimate := TurnCostEstimates(state)[turn]
				amount, ok := estimate["amount"].(float64)
				if !ok || estimate["status"] != "complete" || !closeFloat(amount, fixture.amounts[index]) || estimate["pricedRequests"] != int64(1) {
					t.Fatalf("turn %s: %#v", turn, estimate)
				}
				expected += fixture.amounts[index]
			}
			estimate := CostEstimate(state, Thread{})
			amount, ok := estimate["amount"].(float64)
			if !ok || estimate["status"] != "complete" || !closeFloat(amount, expected) || estimate["pricedRequests"] != int64(4) || estimate["longRequests"] != int64(1) {
				t.Fatalf("thread: %#v", estimate)
			}
		})
	}
}

func TestCostProjectionPreservesZeroPartialUnavailableAndForkScope(t *testing.T) {
	zero := NewCostProjection(false, nil)
	ApplyCostRecord(zero, contextRecord("gpt-6-astra", "zero"), "")
	ApplyCostRecord(zero, usageRecord(usage(0, 0, 0), usage(0, 0, 0), ""), "")
	if estimate := CostEstimate(zero, Thread{}); estimate["amount"] != float64(0) || estimate["status"] != "complete" {
		t.Fatalf("zero: %#v", estimate)
	}
	partial := NewCostProjection(false, nil)
	ApplyCostRecord(partial, contextRecord("gpt-6-astra", ""), "")
	ApplyCostRecord(partial, usageRecord(usage(200000, 160000, 2000), usage(100000, 80000, 1000), ""), "")
	if estimate := CostEstimate(partial, Thread{}); !closeFloat(estimate["amount"].(float64), .33) || estimate["status"] != "partial" {
		t.Fatalf("partial: %#v", estimate)
	}
	threadID := "20000000-0000-4000-8000-000000000002"
	fork := NewCostProjection(true, nil)
	ApplyCostRecord(fork, contextRecord("gpt-6-astra", ""), threadID)
	ApplyCostRecord(fork, usageRecord(usage(100000, 80000, 1000), usage(100000, 80000, 1000), ""), threadID)
	missing := CostEstimate(fork, Thread{})
	if missing["amount"] != nil || missing["status"] != "unavailable" || missing["scope"] != "fork" {
		t.Fatalf("missing boundary: %#v", missing)
	}
	ApplyCostRecord(fork, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "thread_settings_applied", "thread_id": threadID, "thread_settings": map[string]any{"model": "gpt-5.6-sol"}}}, threadID)
	if estimate := CostEstimate(fork, Thread{TokenUsage: map[string]any{"inputTokens": int64(100000), "cachedInputTokens": int64(80000), "outputTokens": int64(1000)}}); estimate["amount"] != float64(0) || estimate["status"] != "complete" {
		t.Fatalf("empty fork: %#v", estimate)
	}
}

func TestEmptyForkAfterBoundaryIsKnownZeroWithoutCopiedUsage(t *testing.T) {
	threadID := "20000000-0000-4000-8000-000000000002"
	state := NewCostProjection(true, nil)
	ApplyCostRecord(state, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "thread_settings_applied", "thread_id": threadID, "thread_settings": map[string]any{"model": "gpt-6-astra"}}}, threadID)
	estimate := CostEstimate(state, Thread{})
	if estimate["status"] != "complete" || estimate["amount"] != float64(0) {
		t.Fatalf("empty fork: %#v", estimate)
	}
}

func TestUsageValidationAndLongContext(t *testing.T) {
	if _, ok := usageCounts(usage(10, 8, 1, 3)); ok {
		t.Fatal("cache read plus write may not exceed input")
	}
	if _, ok := usageCounts(map[string]any{"input_tokens": 10, "output_tokens": 1}); ok {
		t.Fatal("cached input is required")
	}
	counts, _ := usageCounts(usage(272001, 200000, 1000))
	price, ok := priceUsage("gpt-6-astra", counts)
	if !ok {
		t.Fatal("price missing")
	}
	sum := new(big.Int).Add(&price.input, &price.cached)
	sum.Add(sum, &price.output)
	if !closeFloat(dollars(sum), 1.91502) {
		t.Fatalf("long price %v", dollars(sum))
	}
}

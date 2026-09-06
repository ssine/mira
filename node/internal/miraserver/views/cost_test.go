package views

import (
	"math/big"
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

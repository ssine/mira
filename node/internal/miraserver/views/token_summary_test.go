package views

import "testing"

func TestOwnTokenUsageExcludesInheritedForkCounters(t *testing.T) {
	state := NewCostProjection(true, nil)
	inherited := usageRecord(usage(100000, 80000, 1000), usage(100000, 80000, 1000), "inherited")
	ApplyCostRecord(state, inherited, "child")
	if got := ownTokenUsage(state, Thread{}); got["inputTokens"] != nil {
		t.Fatalf("unproven fork boundary counted inherited usage: %#v", got)
	}
	ApplyCostRecord(state, record("event_msg", map[string]any{"type": "thread_settings_applied", "thread_id": "child", "thread_settings": map[string]any{"model": "unknown-model"}}), "child")
	if got := ownTokenUsage(state, Thread{}); got["inputTokens"] != int64(0) || got["outputTokens"] != int64(0) {
		t.Fatalf("empty fork: %#v", got)
	}
	appended := usageRecord(usage(150000, 120000, 1500), usage(50000, 40000, 500), "child-turn")
	ApplyCostRecord(state, appended, "child")
	ApplyCostRecord(state, appended, "child")
	got := ownTokenUsage(state, Thread{})
	if got["inputTokens"] != int64(50000) || got["cachedInputTokens"] != int64(40000) || got["outputTokens"] != int64(500) {
		t.Fatalf("fork, duplicate snapshot or unknown model changed counts: %#v", got)
	}
	// Metadata-only updates remain current without changing immutable history.
	thread := Thread{TokenUsage: NormalizeTokenUsage(usage(180000, 140000, 1800))}
	if got := ownTokenUsage(state.clone(), thread); got["inputTokens"] != int64(80000) || got["outputTokens"] != int64(800) {
		t.Fatalf("metadata-only update: %#v", got)
	}
	// A counter reset invalidates baseline subtraction even if a later counter
	// exceeds the inherited value again.
	ApplyCostRecord(state, usageRecord(usage(10, 8, 1), usage(10, 8, 1), "reset"), "child")
	ApplyCostRecord(state, usageRecord(usage(200000, 160000, 2000), usage(199990, 159992, 1999), "reset"), "child")
	if got := ownTokenUsage(state, Thread{}); got["inputTokens"] != nil {
		t.Fatalf("reset counters falsely appeared complete: %#v", got)
	}
}

func TestTokenSummaryPreservesKnownUsageWithoutRequestPrices(t *testing.T) {
	state := NewCostProjection(false, nil)
	ApplyCostRecord(state, usageRecord(usage(100, 80, 10), nil, "turn"), "root")
	got := ownTokenUsage(state, Thread{})
	if got["inputTokens"] != int64(100) || got["outputTokens"] != int64(10) {
		t.Fatalf("cumulative usage incorrectly depended on prices or last request: %#v", got)
	}
	partial := Thread{TokenUsage: NormalizeTokenUsage(map[string]any{"input_tokens": 150})}
	if got := ownTokenUsage(state, partial); got["inputTokens"] != int64(150) || got["outputTokens"] != nil {
		t.Fatalf("partial metadata silently mixed snapshots: %#v", got)
	}
}

func TestTokenSummaryUnknownZeroPartialAndOverflow(t *testing.T) {
	var sum tokenSum
	if got := sum.summary(); got["status"] != "complete" || got["inputTokens"] != int64(0) {
		t.Fatalf("empty descendant set: %#v", got)
	}
	sum.add(nil)
	if got := sum.summary(); got["status"] != "unavailable" || got["inputTokens"] != nil {
		t.Fatalf("missing counters became zero: %#v", got)
	}
	sum.add(NormalizeTokenUsage(usage(100, 80, 10)))
	if got := sum.summary(); got["status"] != "partial" || got["inputTokens"] != int64(100) {
		t.Fatalf("partial aggregate: %#v", got)
	}
	var overflow tokenSum
	for i := 0; i < 2048; i++ {
		overflow.add(NormalizeTokenUsage(usage(9_007_199_254_740_991, 0, 0)))
	}
	if got := overflow.summary(); got["status"] != "partial" || got["inputTokens"] != nil || got["outputTokens"] != int64(0) {
		t.Fatalf("unsafe integer rounded or overflowed: %#v", got)
	}
}

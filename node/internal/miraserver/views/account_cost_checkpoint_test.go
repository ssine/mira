package views

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAccountCostCheckpointAcrossForkAndUsageResets(t *testing.T) {
	items := []map[string]any{
		contextRecord("gpt-6-astra", "parent"),
		usageRecord(usage(100000, 80000, 1000), usage(100000, 80000, 1000), "parent"),
		record("event_msg", map[string]any{"type": "thread_settings_applied", "thread_id": "child", "thread_settings": map[string]any{"model": "gpt-6-astra"}}),
		usageRecord(usage(100000, 80000, 1000), usage(100000, 80000, 1000), "child-turn"),
		usageRecord(usage(200000, 160000, 2000), usage(100000, 80000, 1000), "child-turn"),
		contextRecord("unknown", "new-turn"),
		usageRecord(usage(300000, 240000, 3000), usage(100000, 80000, 1000), "new-turn"),
		contextRecord("gpt-6-astra", "reset"),
		usageRecord(usage(100000, 80000, 1000), usage(100000, 80000, 1000), "reset"),
	}
	baseline := NewCostProjection(true, []string{})
	for _, item := range items {
		ApplyCostRecord(baseline, item, "child")
	}
	for split := range len(items) + 1 {
		state := NewCostProjection(true, []string{})
		for _, item := range items[:split] {
			ApplyCostRecord(state, item, "child")
		}
		raw, err := json.Marshal(checkpointCost(state, "fixture-provider"))
		if err != nil {
			t.Fatal(err)
		}
		var saved accountCostCheckpoint
		if err := json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		state, err = saved.restore()
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items[split:] {
			ApplyCostRecord(state, item, "child")
		}
		if !reflect.DeepEqual(checkpointCost(state, saved.Provider), checkpointCost(baseline, "fixture-provider")) {
			t.Fatalf("checkpoint at %d changed fork accounting", split)
		}
	}
}

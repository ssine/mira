package miraserver

import (
	"encoding/json"
	"testing"
)

func TestBuildHistoryPlanPreservesAppendGenerationAndReplacesDivergence(t *testing.T) {
	previous := map[string]any{"histories": map[string]any{"one": []any{map[string]any{"id": "a"}}}}
	next := map[string]any{"histories": map[string]any{
		"one": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}},
		"two": []any{},
	}}
	manifest, appends, err := buildHistoryPlan(previous, next, map[string]historyEntry{"one": {Generation: 4, ItemCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if manifest["one"] != (historyEntry{Generation: 4, ItemCount: 2}) || manifest["two"] != (historyEntry{Generation: 1, ItemCount: 0}) {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
	if len(appends) != 1 || appends[0].Generation != 4 || appends[0].ItemSeq != 2 {
		t.Fatalf("unexpected appends: %#v", appends)
	}

	diverged := map[string]any{"histories": map[string]any{"one": []any{map[string]any{"id": "other"}}}}
	manifest, appends, err = buildHistoryPlan(previous, diverged, map[string]historyEntry{"one": {Generation: 4, ItemCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if manifest["one"].Generation != 5 || len(appends) != 1 || appends[0].ItemSeq != 1 {
		t.Fatalf("replacement did not create a generation: %#v %#v", manifest, appends)
	}
}

func TestBuildHistoryPlanAcceptsCanonicalStoreHistoryType(t *testing.T) {
	previous := map[string]any{"histories": map[string][]any{
		"one": {map[string]any{"id": "a"}},
	}}
	next := map[string]any{"histories": map[string]any{
		"one": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}},
	}}
	manifest, appends, err := buildHistoryPlan(previous, next, map[string]historyEntry{"one": {Generation: 3, ItemCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if manifest["one"] != (historyEntry{Generation: 3, ItemCount: 2}) || len(appends) != 1 || appends[0].ItemSeq != 2 {
		t.Fatalf("canonical history was not treated as an append: %#v %#v", manifest, appends)
	}
}

func TestStateDeltaRoundTripPreservesUnknownFields(t *testing.T) {
	before := map[string]any{"metadata_updates": map[string]any{"thread": map[string]any{"future": json.Number("9007199254740991")}}, "opaque": []any{"keep"}}
	after, err := cloneObject(before)
	if err != nil {
		t.Fatal(err)
	}
	after["metadata_updates"].(map[string]any)["thread"].(map[string]any)["title"] = "new"
	changes := stateDelta(before, after)
	if len(changes) != 1 || changes[0].Mode != "set" {
		t.Fatalf("unexpected delta: %#v", changes)
	}
	copy, _ := cloneObject(before)
	writePath(copy, requestedStateChange{Path: changes[0].Path, Mode: changes[0].Mode, Value: changes[0].Value})
	if !jsonEqual(copy, after) {
		t.Fatalf("delta did not round trip: %#v != %#v", copy, after)
	}
}

func TestParseStateChangesRequiresReleasedConflictPolicy(t *testing.T) {
	base := map[string]any{
		"path": []any{"metadata_updates", "thread", "title"}, "mode": "set", "value": "x",
		"expected": map[string]any{"exists": false}, "conflictPolicy": "lastWriteWins",
	}
	if _, err := parseStateChanges([]any{base}); err == nil {
		t.Fatal("accepted LWW for a compare-and-swap path")
	}
	base["conflictPolicy"] = "compareAndSwap"
	if _, err := parseStateChanges([]any{base}); err != nil {
		t.Fatal(err)
	}
	base["path"] = []any{"metadata_updates", "thread", "updated_at"}
	if _, err := parseStateChanges([]any{base}); err == nil {
		t.Fatal("accepted CAS for the released LWW path")
	}
}

func TestCanonicalDigestSortsObjectKeys(t *testing.T) {
	left, err := digestJSON(map[string]any{"b": 2, "a": map[string]any{"d": 4, "c": 3}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := digestJSON(map[string]any{"a": map[string]any{"c": 3, "d": 4}, "b": 2})
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("canonical digests differ: %s %s", left, right)
	}
}

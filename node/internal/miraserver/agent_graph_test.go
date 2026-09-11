package miraserver

import (
	"context"
	"net/http"
	"reflect"
	"sort"
	"testing"
)

func TestCanonicalAgentGraphLifecycle(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	uuid := func() string {
		id, err := randomUUID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	root, child, sibling, grandchild, missing := uuid(), uuid(), uuid(), uuid(), uuid()
	store := "graph-test"
	state, histories := []any{}, []any{}
	for _, id := range []string{root, child, sibling, grandchild} {
		state = append(state, map[string]any{"path": []any{"created_threads", id}, "mode": "set", "conflictPolicy": "compareAndSwap", "expected": map[string]any{"exists": false}, "value": map[string]any{"thread_id": id}})
		histories = append(histories, map[string]any{"threadId": id, "mode": "append", "expectedGeneration": 0, "expectedItemCount": 0, "items": []any{map[string]any{"type": "future", "payload": "original"}}})
	}
	created, err := CommitDelta(ctx, pool, store, map[string]any{"expectedVersion": 0, "stateChanges": state, "historyChanges": histories}, http.Header{"X-Codex-Operation-Id": []string{uuid()}})
	if err != nil || created.Status != 200 {
		t.Fatalf("create: %+v %v", created, err)
	}
	mutate := func(body agentGraphRequest) {
		t.Helper()
		if _, err := agentGraphOperation(ctx, pool, store, body, uuid()); err != nil {
			t.Fatal(err)
		}
	}
	list := func(operation, status string) []string {
		t.Helper()
		result, err := agentGraphOperation(ctx, pool, store, agentGraphRequest{Operation: operation, ParentID: root, Status: status}, "")
		if err != nil {
			t.Fatal(err)
		}
		return result["threadIds"].([]string)
	}
	mutate(agentGraphRequest{Operation: "upsert", ParentID: root, ChildID: child, Status: "open"})
	mutate(agentGraphRequest{Operation: "upsert", ParentID: root, ChildID: sibling, Status: "open"})
	mutate(agentGraphRequest{Operation: "upsert", ParentID: child, ChildID: grandchild, Status: "open"})
	direct := []string{child, sibling}
	sort.Strings(direct)
	want := append(append([]string{}, direct...), grandchild)
	if got := list("children", ""); !reflect.DeepEqual(got, direct) {
		t.Fatalf("direct children: %v", got)
	}
	if got := list("descendants", ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("breadth first: %v", got)
	}
	if _, err := agentGraphOperation(ctx, pool, store, agentGraphRequest{Operation: "upsert", ParentID: grandchild, ChildID: root, Status: "open"}, uuid()); err == nil {
		t.Fatal("accepted a cycle")
	}
	closeID := uuid()
	close := agentGraphRequest{Operation: "status", ChildID: child, Status: "closed"}
	if _, err := agentGraphOperation(ctx, pool, store, close, closeID); err != nil {
		t.Fatal(err)
	}
	if got := list("descendants", "open"); !reflect.DeepEqual(got, []string{sibling}) {
		t.Fatalf("closed ancestor traversed: %v", got)
	}
	if got := list("descendants", ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("closed edge missing from all: %v", got)
	}
	mutate(agentGraphRequest{Operation: "status", ChildID: missing, Status: "closed"})
	if _, err := pool.Exec(ctx, "DELETE FROM mira_agent_graph_edges WHERE store_id=$1", store); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "SELECT mira_rebuild_agent_graph($1)", store); err != nil {
		t.Fatal(err)
	}
	if got := list("descendants", "open"); !reflect.DeepEqual(got, []string{sibling}) {
		t.Fatalf("rebuild lost closed status: %v", got)
	}
	mutate(agentGraphRequest{Operation: "status", ChildID: child, Status: "open"})
	if replay, err := agentGraphOperation(ctx, pool, store, close, closeID); err != nil || replay["duplicate"] != true {
		t.Fatalf("replay: %v %v", replay, err)
	}
	if got := list("descendants", "open"); !reflect.DeepEqual(got, want) {
		t.Fatalf("replay reapplied old close: %v", got)
	}
	close.Status = "open"
	if _, err := agentGraphOperation(ctx, pool, store, close, closeID); err == nil {
		t.Fatal("UUID accepted a different request")
	}
	// A canonical replacement advances the generation; old graph edges must
	// remain in the event log without attaching to the replacement thread.
	replaced, err := CommitDelta(ctx, pool, store, map[string]any{"expectedVersion": 1, "stateChanges": []any{}, "historyChanges": []any{map[string]any{"threadId": child, "mode": "replace", "expectedGeneration": 1, "expectedItemCount": 1, "items": []any{map[string]any{"type": "future", "payload": "replacement"}}}}}, http.Header{"X-Codex-Operation-Id": []string{uuid()}})
	if err != nil || replaced.Status != 200 {
		t.Fatalf("replace: %+v %v", replaced, err)
	}
	if got := list("descendants", ""); !reflect.DeepEqual(got, []string{sibling}) {
		t.Fatalf("old generation resurrected: %v", got)
	}
}

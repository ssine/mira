package miraserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type claudeExecutionInvoker struct {
	online bool
	call   func(map[string]any) (any, error)
}

func (i claudeExecutionInvoker) IsConnected(string) bool { return i.online }
func (i claudeExecutionInvoker) Invoke(_ context.Context, _, _ string, params map[string]any, _ time.Duration) (any, error) {
	return i.call(params)
}

func reserveClaudeExecution(t *testing.T, f *claudeFixture) (claudeExecutionTarget, map[string]string) {
	t.Helper()
	id, turn, headers := f.reserved()
	_, err := f.server.pool.Exec(context.Background(), `UPDATE mira_claude_turns SET runtime_id='owner',status='running' WHERE turn_id=$1`, turn)
	if err != nil {
		t.Fatal(err)
	}
	return claudeExecutionTarget{session: id, turn: turn, node: f.nodeID, runtime: "owner", revision: 1, protocol: true}, headers
}

func TestClaudeExecutionReconciliation(t *testing.T) {
	f := newClaudeFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, state, runtime, proof string
		online, protocol, failed    bool
		want                        string
	}{
		{"live", "running", "owner", "", true, true, false, "running"},
		{"offline", "stopped", "owner", "turn_fenced", false, true, false, "unknown"},
		{"timeout", "stopped", "owner", "turn_fenced", true, true, true, "unknown"},
		{"restarted_without_proof", "unknown", "new", "", true, true, false, "unknown"},
		{"unsealed_absence", "stopped", "owner", "", true, true, false, "unknown"},
		{"wrong_proof", "stopped", "new", "turn_fenced", true, true, false, "unknown"},
		{"missing_runtime", "stopped", "", "runtime_closed", true, true, false, "unknown"},
		{"legacy_absence", "stopped", "owner", "", true, false, false, "unknown"},
		{"legacy_live", "running", "owner", "", true, false, false, "running"},
		{"fenced_exit", "stopped", "owner", "turn_fenced", true, true, false, "stopped"},
		{"shutdown_receipt", "stopped", "new", "runtime_closed", true, true, false, "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, headers := reserveClaudeExecution(t, f)
			target.protocol = tc.protocol
			invoker := claudeExecutionInvoker{online: tc.online, call: func(p map[string]any) (any, error) {
				if tc.failed {
					return nil, errors.New("timeout")
				}
				return map[string]any{"runtimeId": tc.runtime, "expectedRuntimeId": p["runtimeId"], "turnId": p["turnId"], "state": tc.state, "proof": tc.proof, "active": tc.state == "running"}, nil
			}}
			state, err := f.server.reconcileClaudeExecution(ctx, invoker, target)
			if (err != nil) != tc.failed || state["state"] != tc.want {
				t.Fatalf("%v %v", state, err)
			}
			s := f.call("GET", "/v1/claude/sessions/"+target.session, nil)
			if (s["activeTurn"] == nil) != (tc.want == "stopped") {
				t.Fatalf("reservation: %v", s)
			}
			if tc.want != "stopped" {
				return
			}
			if s["persistence"] != "incomplete" || s["historyAcknowledgementRequired"] != true {
				t.Fatal(s)
			}
			// Repeating recovery is idempotent; a late native success is retained
			// as raw evidence but cannot rewrite the already-finalized failure.
			_, _ = f.server.reconcileClaudeExecution(ctx, invoker, target)
			status, body := f.request("POST", "/v1/claude/sessions/"+target.session+"/events", map[string]any{"eventId": uuidClaude(), "payload": map[string]any{"type": "mira_completed"}}, headers)
			if status != 200 {
				t.Fatalf("late event: %d %s", status, body)
			}
			var events int
			var terminal string
			if err = f.server.pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM mira_claude_events WHERE turn_id=$1 AND event_type='mira_execution_stopped') FROM mira_claude_turns WHERE turn_id=$1`, target.turn).Scan(&terminal, &events); err != nil || terminal != "failed" || events != 1 {
				t.Fatalf("terminal=%s events=%d error=%v", terminal, events, err)
			}
		})
	}
}

func TestClaudeExecutionProbeCannotFinalizeNewOwnership(t *testing.T) {
	f := newClaudeFixture(t)
	target, _ := reserveClaudeExecution(t, f)
	ctx := context.Background()
	invoker := claudeExecutionInvoker{online: true, call: func(p map[string]any) (any, error) {
		_, err := f.server.pool.Exec(ctx, `UPDATE mira_claude_sessions SET revision=revision+1 WHERE session_id=$1`, target.session)
		return map[string]any{"runtimeId": "owner", "expectedRuntimeId": "owner", "turnId": target.turn, "state": "stopped", "proof": "turn_fenced"}, err
	}}
	if _, err := f.server.reconcileClaudeExecution(ctx, invoker, target); err != nil {
		t.Fatal(err)
	}
	if f.call("GET", "/v1/claude/sessions/"+target.session, nil)["activeTurn"] == nil {
		t.Fatal("stale probe cleared a new owner")
	}
}

func TestClaudeExecutionPagesRunWithoutBrowserAndPreserveOffline(t *testing.T) {
	f := newClaudeFixture(t)
	ctx := context.Background()
	_, err := f.server.pool.Exec(ctx, `UPDATE codex_nodes SET capabilities=capabilities||'{"claudeExecutionStatusV1":true}' WHERE node_id=$1`, f.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 67; i++ {
		reserveClaudeExecution(t, f)
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	invoker := claudeExecutionInvoker{online: true, call: func(p map[string]any) (any, error) {
		mu.Lock()
		seen[p["turnId"].(string)] = true
		mu.Unlock()
		return map[string]any{"runtimeId": "owner", "expectedRuntimeId": "owner", "turnId": p["turnId"], "state": "running"}, nil
	}}
	cursor, err := f.server.reconcileClaudeExecutionPage(ctx, invoker, "")
	if err != nil || cursor == "" || len(seen) != 64 {
		t.Fatalf("first page: %s %d %v", cursor, len(seen), err)
	}
	cursor, err = f.server.reconcileClaudeExecutionPage(ctx, invoker, cursor)
	if err != nil || cursor != "" || len(seen) != 67 {
		t.Fatalf("next page: %s %d %v", cursor, len(seen), err)
	}
	// The real channel is offline. Even a recent cached running observation
	// must not turn an offline reservation into an assertion of liveness.
	for session, observation := range f.server.claudeExecution {
		s := f.call("GET", "/v1/claude/conversations/"+session, nil)
		activity := s["activity"].(map[string]any)
		if activity["state"] != "unknown" || activity["reason"] != "offline" || activity["turnId"] != observation.turn {
			t.Fatal(activity)
		}
		break
	}
}

func TestClaudeExecutionStartGraceAndConcurrentFinalization(t *testing.T) {
	f := newClaudeFixture(t)
	ctx := context.Background()
	target, _ := reserveClaudeExecution(t, f)
	if _, err := f.server.pool.Exec(ctx, `UPDATE mira_claude_turns SET status='starting' WHERE turn_id=$1`, target.turn); err != nil {
		t.Fatal(err)
	}
	invoker := claudeExecutionInvoker{online: true, call: func(map[string]any) (any, error) {
		t.Error("fresh start was probed before its dispatch budget")
		return nil, nil
	}}
	if _, err := f.server.reconcileClaudeExecutionPage(ctx, invoker, ""); err != nil {
		t.Fatal(err)
	}
	// Simultaneous periodic/manual probes may finish in either order. Exactly
	// one event and one terminal transition must be committed.
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := f.server.finalizeClaudeExecution(ctx, target, "turn_fenced"); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	var count int
	if err := f.server.pool.QueryRow(ctx, `SELECT count(*) FROM mira_claude_events WHERE turn_id=$1 AND event_type='mira_execution_stopped'`, target.turn).Scan(&count); err != nil || count != 1 {
		t.Fatalf("events=%d: %v", count, err)
	}
}

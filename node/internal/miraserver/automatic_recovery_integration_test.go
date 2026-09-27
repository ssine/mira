package miraserver

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/ssine/mira/node/internal/miraserver/channel"
)

func TestAutomaticRecoveryClaimsCanonicalFailureOnce(t *testing.T) {
	pool := accountTestDatabase(t)
	server := &Server{pool: pool}
	ctx := context.Background()
	for _, mode := range []string{"v1", "v2", "upload"} {
		t.Run(mode, func(t *testing.T) {
			f := newExecutionHistoryFixture(t, pool)
			failure := map[string]any{"type": "event_msg", "payload": map[string]any{"type": "error", "message": `unexpected status 409 Conflict: {"error":{"code":"unknown_reasoning_pool"}}`}}
			f.append(mode, lifecycle("task_started", "failed-turn"), failure)
			if job, err := server.claimAutomaticRecovery(ctx); err != nil || job != nil {
				t.Fatalf("running failure claimed: %+v %v", job, err)
			}
			f.append(mode, lifecycle("task_complete", "failed-turn"))
			// No preference row: all existing and new threads default on.
			var wg sync.WaitGroup
			results := make(chan *automaticRecoveryJob, 8)
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					job, err := server.claimAutomaticRecovery(ctx)
					if err != nil {
						t.Error(err)
					}
					if job != nil {
						results <- job
					}
				}()
			}
			wg.Wait()
			close(results)
			if len(results) != 1 {
				t.Fatalf("claims=%d, want 1", len(results))
			}
			job := <-results
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err = channel.CheckAutomaticRecovery(ctx, tx, f.store, f.thread, job.Failure); err != nil {
				t.Fatal(err)
			}
			tx.Rollback(ctx)
			f.exec(`UPDATE mira_codex_execution_routes SET state='starting',turn_id=NULL WHERE store_id=$1 AND thread_id=$2`, f.store, f.thread)
			tx, err = pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err = channel.CheckAutomaticRecovery(ctx, tx, f.store, f.thread, job.Failure); err == nil {
				t.Fatal("new user turn did not supersede retry")
			}
			tx.Rollback(ctx)
		})
	}
}

func TestAutomaticRecoveryScopeAndRetryLimit(t *testing.T) {
	pool := accountTestDatabase(t)
	server := &Server{pool: pool}
	ctx := context.Background()
	for _, kind := range []string{"disabled", "archived", "credentials", "generation", "ordinary-error", "tool-error"} {
		t.Run(kind, func(t *testing.T) {
			f := newExecutionHistoryFixture(t, pool)
			f.append("v2", lifecycle("task_started", "failed-turn"))
			message := `{"error":{"code":"invalid_encrypted_content"}}`
			recordType := "event_msg"
			if kind == "ordinary-error" {
				message = "temporary network error"
			}
			if kind == "tool-error" {
				recordType = "response_item"
			}
			f.append("v2", map[string]any{"type": recordType, "payload": map[string]any{"type": "error", "message": message}}, lifecycle("task_complete", "failed-turn"))
			switch kind {
			case "disabled":
				f.exec(`INSERT INTO mira_codex_recovery_preferences VALUES($1,$2,1,false,now())`, f.store, f.thread)
			case "archived":
				f.exec(`INSERT INTO mira_thread_actions(store_id,thread_id,generation,action,operation_id) VALUES($1,$2,1,'archive',gen_random_uuid())`, f.store, f.thread)
			case "credentials":
				f.exec(`UPDATE mira_node_codex_accounts SET credential_revision=2 WHERE node_account_id=$1::uuid`, f.binding)
			case "generation":
				f.exec(`UPDATE codex_thread_projections SET active_generation=2 WHERE store_id=$1`, f.store)
			}
			if job, err := server.claimAutomaticRecovery(ctx); err != nil || job != nil {
				t.Fatalf("unsafe recovery claimed: %+v %v", job, err)
			}
		})
	}
}

func TestAutomaticRecoveryCountsFailuresUntilHistoryProgress(t *testing.T) {
	pool := accountTestDatabase(t)
	server := &Server{pool: pool}
	ctx := context.Background()
	f := newExecutionHistoryFixture(t, pool)
	encrypted := map[string]any{"type": "response_item", "payload": map[string]any{"type": "reasoning", "encrypted_content": "opaque"}}
	f.append("v2", encrypted)
	failure := map[string]any{"type": "event_msg", "payload": map[string]any{"type": "error", "message": `{"error":{"code":"invalid_encrypted_content"}}`}}
	fail := func(turn string, want int) {
		t.Helper()
		f.append("v2", lifecycle("task_started", turn),
			map[string]any{"type": "turn_context", "payload": map[string]any{"turn_id": turn}},
			map[string]any{"type": "event_msg", "payload": map[string]any{"type": "token_count"}},
			failure, lifecycle("task_complete", turn))
		job, err := server.claimAutomaticRecovery(ctx)
		if err != nil || job == nil {
			t.Fatalf("claim: %+v %v", job, err)
		}
		plan := inputRecoveryPlan{Generation: 1, ThroughItemSeq: f.count}
		got, err := server.automaticRecoveryFailureCount(ctx, *job, plan)
		if err != nil || got != want {
			t.Fatalf("failure count=%d want=%d err=%v", got, want, err)
		}
		// A reconstructed Server uses the same durable count.
		got, err = (&Server{pool: pool}).automaticRecoveryFailureCount(ctx, *job, plan)
		if err != nil || got != want {
			t.Fatalf("restart count=%d want=%d err=%v", got, want, err)
		}
	}
	for n := 1; n <= 20; n++ {
		fail(fmt.Sprintf("failure-%d", n), n)
	}
	// A new runtime-injected policy message is not task progress either.
	f.append("v2", map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "developer", "content": []any{}}})
	fail("still-same-history", 20)
	for _, record := range []any{
		encrypted,
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "output": "done"}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{}}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{}}},
		map[string]any{"type": "compacted", "payload": map[string]any{"replacement_history": []any{}}},
	} {
		f.append("v2", record)
		fail(fmt.Sprintf("progress-%d", f.count), 1)
		fail(fmt.Sprintf("unchanged-%d", f.count), 2)
	}
}

func TestAutomaticRecoveryNoticesCountDispatchedRetriesPerTask(t *testing.T) {
	pool := accountTestDatabase(t)
	server := &Server{pool: pool}
	ctx := context.Background()
	f := newExecutionHistoryFixture(t, pool)
	request := func(failure string) {
		f.exec(`INSERT INTO mira_codex_execution_events(operation_id,store_id,thread_id,generation,node_account_id,runtime_id,revision,kind,detail)
 VALUES(gen_random_uuid(),$1,$2,1,$3::uuid,$4,1,'turn_requested',jsonb_build_object('recoveryFailureId',$5::text))`, f.store, f.thread, f.binding, f.runtime, failure)
	}
	fail := func(turn string) *automaticRecoveryJob {
		f.append("v2", lifecycle("task_started", turn), map[string]any{"type": "event_msg", "payload": map[string]any{"type": "error", "message": `{"code":"invalid_encrypted_content"}`}}, lifecycle("task_complete", turn))
		job, err := server.claimAutomaticRecovery(ctx)
		if err != nil || job == nil {
			t.Fatalf("claim: %+v %v", job, err)
		}
		return job
	}
	check := func(want map[string]int) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		got, err := automaticRecoveryNotices(ctx, tx, f.store, f.thread, 1, []string{"first", "second", "new-task", "unrelated"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("notices: %+v", got)
		}
		for turn, count := range want {
			if got[turn].RetryCount != count {
				t.Fatalf("%s retries=%d want=%d", turn, got[turn].RetryCount, count)
			}
		}
	}
	request("")
	first := fail("first")
	check(map[string]int{"first": 0}) // Claim alone is not a retry.
	request(first.Failure)
	second := fail("second")
	check(map[string]int{"first": 1, "second": 1})
	request(second.Failure)
	check(map[string]int{"first": 1, "second": 2})
	request("")
	third := fail("new-task")
	check(map[string]int{"first": 1, "second": 2, "new-task": 0})
	request(third.Failure)
	check(map[string]int{"first": 1, "second": 2, "new-task": 1})
}

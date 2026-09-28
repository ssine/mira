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

func TestAutomaticRecoveryDeferredPreparationDoesNotConsumeRetry(t *testing.T) {
	pool := accountTestDatabase(t)
	server := &Server{pool: pool}
	ctx := context.Background()
	f := newExecutionHistoryFixture(t, pool)
	f.append("v2", lifecycle("task_started", "failed"), map[string]any{"type": "event_msg", "payload": map[string]any{"type": "error", "message": `{"error":{"code":"invalid_encrypted_content"}}`}}, lifecycle("task_complete", "failed"))
	job, err := server.claimAutomaticRecovery(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim: %v %v", job, err)
	}
	f.exec(`UPDATE mira_codex_recovery_attempts SET reason=$2,updated_at=now()-interval '10 seconds' WHERE failure_id=$1::uuid`, job.Failure, errAutomaticRecoveryDeferred.Error())
	var wg sync.WaitGroup
	claims := make(chan *automaticRecoveryJob, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next, err := server.claimAutomaticRecovery(ctx)
			if err != nil {
				t.Error(err)
			}
			if next != nil {
				claims <- next
			}
		}()
	}
	wg.Wait()
	close(claims)
	if len(claims) != 1 {
		t.Fatalf("deferred claim count=%d", len(claims))
	}
	if next := <-claims; next.Failure != job.Failure {
		t.Fatal("deferred preparation created a different failure")
	}
	count, err := server.automaticRecoveryFailureCount(ctx, *job, inputRecoveryPlan{Generation: 1, ThroughItemSeq: f.count})
	if err != nil || count != 1 {
		t.Fatalf("preparation spent model retry budget: %d %v", count, err)
	}
	// An ambiguous dispatch must never enter the preparatory queue again.
	f.exec(`UPDATE mira_codex_recovery_attempts SET status='dispatching',reason=$2,updated_at=now()-interval '10 seconds' WHERE failure_id=$1::uuid`, job.Failure, errAutomaticRecoveryDeferred.Error())
	if next, err := server.claimAutomaticRecovery(ctx); err != nil || next != nil {
		t.Fatalf("dispatch replayed: %v %v", next, err)
	}
}

func TestAutomaticRecoveryCannotReviveClosedChild(t *testing.T) {
	pool := accountTestDatabase(t)
	server := &Server{pool: pool}
	ctx := context.Background()
	for _, afterClaim := range []bool{false, true} {
		t.Run(fmt.Sprint(afterClaim), func(t *testing.T) {
			f := newExecutionHistoryFixture(t, pool)
			f.append("v2", lifecycle("task_started", "failed"), map[string]any{"type": "event_msg", "payload": map[string]any{"type": "error", "message": `{"code":"invalid_encrypted_content"}`}}, lifecycle("task_complete", "failed"))
			var job *automaticRecoveryJob
			if afterClaim {
				var err error
				job, err = server.claimAutomaticRecovery(ctx)
				if err != nil || job == nil {
					t.Fatalf("claim: %v %v", job, err)
				}
			}
			f.exec(`WITH event AS (
 INSERT INTO mira_agent_graph_events(store_id,operation_id,request_sha256,child_thread_id,parent_thread_id,child_generation,parent_generation,status)
 VALUES($1,gen_random_uuid(),repeat('0',64),$2,'parent',1,1,'closed') RETURNING event_seq)
 INSERT INTO mira_agent_graph_edges(store_id,child_thread_id,parent_thread_id,child_generation,parent_generation,status,event_seq)
 SELECT $1,$2,'parent',1,1,'closed',event_seq FROM event`, f.store, f.thread)
			if !afterClaim {
				if next, err := server.claimAutomaticRecovery(ctx); err != nil || next != nil {
					t.Fatalf("closed child claimed: %v %v", next, err)
				}
				return
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err := channel.CheckAutomaticRecovery(ctx, tx, f.store, f.thread, job.Failure); err == nil {
				t.Fatal("parent closure did not supersede the pending child recovery")
			}
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

func TestAutomaticRecoveryNoticesCountRetriesAtEachHistoryPosition(t *testing.T) {
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
		got, err := automaticRecoveryNotices(ctx, tx, f.store, f.thread, 1, []string{"first", "second", "new-position", "new-task", "unrelated"})
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
	// Tool progress starts a new conversation position even with no new user turn.
	f.append("v2", map[string]any{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "output": "done"}})
	third := fail("new-position")
	check(map[string]int{"first": 1, "second": 2, "new-position": 0})
	request(third.Failure)
	check(map[string]int{"first": 1, "second": 2, "new-position": 1})
	// A shutdown cancels the Server observer, not the dispatched model turn.
	f.exec(`UPDATE mira_codex_recovery_attempts SET status='stopped',reason='context canceled' WHERE failure_id=$1::uuid`, third.Failure)
	f.append("v2", lifecycle("task_started", "retry-after-restart"))
	status := func(want string) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		got, err := automaticRecoveryNotices(ctx, tx, f.store, f.thread, 1, []string{"new-position"})
		if err != nil || got["new-position"].Status != want || got["new-position"].Reason != "" || got["new-position"].RetryCount != 1 {
			t.Fatalf("restart notice: %+v err=%v want=%s", got, err, want)
		}
	}
	status("dispatching")
	f.append("v2", lifecycle("task_complete", "retry-after-restart"))
	status("completed")
	request("")
	f.append("v2", map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{}}})
	fourth := fail("new-task")
	check(map[string]int{"first": 1, "second": 2, "new-position": 1, "new-task": 0})
	request(fourth.Failure)
	check(map[string]int{"first": 1, "second": 2, "new-position": 1, "new-task": 1})
}

func TestAutomaticRecoveryFromTerminalErrorWithoutLiveNotification(t *testing.T) {
	pool := accountTestDatabase(t)
	server := &Server{pool: pool}
	ctx := context.Background()
	for _, mode := range []string{"v1", "v2", "upload"} {
		for _, end := range []string{"task_complete", "turn_complete"} {
			t.Run(mode+"/"+end, func(t *testing.T) {
				f := newExecutionHistoryFixture(t, pool)
				f.append(mode, lifecycle("task_started", "failed"), map[string]any{"type": "event_msg", "payload": map[string]any{
					"type": end, "turn_id": "failed", "error": map[string]any{"codex_error_info": "other", "message": `{"error":{"code":"invalid_encrypted_content"}}`},
				}})
				f.route("idle", "failed")
				job, err := server.claimAutomaticRecovery(ctx)
				if err != nil || job == nil || job.Thread != f.thread {
					t.Fatalf("terminal failure: %+v %v", job, err)
				}
				if job, err := server.claimAutomaticRecovery(ctx); err != nil || job != nil {
					t.Fatalf("duplicate terminal claim: %+v %v", job, err)
				}
			})
		}
	}
}

func TestAutomaticRecoveryRebuildsMissedTerminalFailure(t *testing.T) {
	pool := accountTestDatabase(t)
	server := &Server{pool: pool}
	ctx := context.Background()
	for _, guard := range []string{"recover", "ordinary-error", "aborted", "missing-turn", "new-turn", "generation", "runtime"} {
		t.Run(guard, func(t *testing.T) {
			f := newExecutionHistoryFixture(t, pool)
			kind, turn, message := "task_complete", "failed", `{"error":{"code":"invalid_encrypted_content"}}`
			if guard == "ordinary-error" {
				message = "Permission denied"
			}
			if guard == "aborted" {
				kind = "turn_aborted"
			}
			if guard == "missing-turn" {
				turn = ""
			}
			f.append("v2", lifecycle("task_started", "failed"), map[string]any{"type": "event_msg", "payload": map[string]any{
				"type": kind, "turn_id": turn, "error": map[string]any{"message": message},
			}})
			// Simulate the derived projection produced by older Servers.
			f.exec(`DELETE FROM mira_codex_execution_events WHERE store_id=$1 AND kind='invalid_encrypted_content'`, f.store)
			switch guard {
			case "new-turn":
				f.append("v2", lifecycle("task_started", "new"))
			case "generation":
				f.exec(`UPDATE codex_thread_projections SET active_generation=2 WHERE store_id=$1`, f.store)
			case "runtime":
				f.exec(`UPDATE mira_node_codex_accounts SET reported='{}' WHERE node_account_id=$1::uuid`, f.binding)
			}
			for range 2 {
				if err := server.reconcileCompletedRecoveryFailures(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM mira_codex_execution_events WHERE store_id=$1 AND kind='invalid_encrypted_content'`, f.store).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if guard == "recover" {
				want = 1
			}
			if count != want {
				t.Fatalf("reconciled failures=%d want=%d", count, want)
			}
			job, err := server.claimAutomaticRecovery(ctx)
			if err != nil || (job != nil) != (want == 1) {
				t.Fatalf("rebuild claim: %+v %v", job, err)
			}
		})
	}
}

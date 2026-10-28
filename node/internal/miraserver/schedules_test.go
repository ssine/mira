package miraserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduleTimezoneAndValidation(t *testing.T) {
	now := time.Date(2026, 10, 28, 21, 0, 0, 0, time.UTC)
	task := scheduledTask{ID: "00000000-0000-4000-8000-000000000001", NodeID: "00000000-0000-4000-8000-000000000002", Name: "daily", scriptSpec: scriptSpec{Cron: "30 6 * * *", Timezone: "Asia/Shanghai", Command: "/bin/sh", CWD: "/tmp"}}
	if err := task.validate(now); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 28, 22, 30, 0, 0, time.UTC); !task.NextRunAt.Equal(want) {
		t.Fatalf("next=%s want=%s", task.NextRunAt, want)
	}
	for _, spec := range []scriptSpec{{Cron: "@every 1s", Timezone: "UTC"}, {Cron: "CRON_TZ=UTC 0 1 * * *", Timezone: "UTC"}, {Cron: "* * * * *", Timezone: "Local"}, {Cron: "* * * * *", Timezone: "Bogus"}} {
		if _, err := spec.schedule(); err == nil {
			t.Fatalf("accepted %+v", spec)
		}
	}
	task.Cron = "0 0 31 2 *"
	if err := task.validate(now); err == nil {
		t.Fatal("accepted impossible calendar")
	}
	// A DST gap is skipped, and a repeated local time remains valid cron behavior.
	spec := scriptSpec{Cron: "30 2 * * *", Timezone: "America/New_York"}
	s, err := spec.schedule()
	if err != nil {
		t.Fatal(err)
	}
	next := s.Next(time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC))
	if next.Day() != 9 {
		t.Fatalf("DST gap not skipped: %s", next)
	}
}

func TestScheduledScriptsDurabilityAndNoReplay(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	nodeID, _ := randomUUID()
	taskID, _ := randomUUID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status,approved_at) VALUES($1::uuid,$1::text,'scheduler-test','linux','amd64','linux','test','{"processes":true}','[]','approved',now())`, nodeID)
	spec := scriptSpec{Cron: "30 6 * * *", Timezone: "Asia/Shanghai", Command: "/bin/sh", Args: []string{"job.sh"}, CWD: "/tmp"}
	raw, _ := json.Marshal(spec)
	now := time.Now().UTC()
	exec(`INSERT INTO mira_scheduled_tasks(task_id,name,node_id,spec,next_run_at) VALUES($1,'test',$2,$3,$4)`, taskID, nodeID, raw, now.Add(-72*time.Hour))
	online := false
	starts := atomic.Int32{}
	polls := atomic.Int32{}
	mode := "running"
	scheduler := &scriptScheduler{pool: pool, online: func(string) bool { return online }, invoke: func(_ context.Context, _ string, p map[string]any) (any, error) {
		if p["action"] == "start" {
			starts.Add(1)
			if mode == "lost-start" {
				return nil, errors.New("lost acknowledgement")
			}
		} else {
			polls.Add(1)
			if mode == "missing" {
				return nil, errors.New("managed process not found")
			}
			if mode == "offline" {
				return nil, errors.New("channel disconnected")
			}
		}
		running := mode != "done"
		var code *int
		if !running {
			zero := 0
			code = &zero
		}
		return map[string]any{"processId": "process-1", "running": running, "exitCode": code, "output": map[string]any{"cursor": 100000, "lostOutput": true, "chunks": []any{map[string]any{"text": strings.Repeat("录", 30000) + "\x00end"}}}}, nil
	}}
	tick := func() {
		t.Helper()
		if err := scheduler.tick(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM mira_scheduled_runs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	current := func() *scheduledRun {
		t.Helper()
		r, err := scanScheduledRun(pool.QueryRow(ctx, `SELECT `+runColumns+` FROM mira_scheduled_runs ORDER BY created_at DESC LIMIT 1`))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	tick()
	if count() != 0 {
		t.Fatal("offline automatic job dispatched")
	}
	online = true
	tick()
	if count() != 1 || current().Status != "queued" {
		t.Fatal("missed occurrences not coalesced")
	}
	tick()
	r := current()
	if starts.Load() != 1 || r.Status != "running" || len(r.Output) > scheduleOutputLimit || !r.Truncated || strings.ContainsRune(r.Output, 0) {
		t.Fatalf("bad running result: starts=%d status=%s", starts.Load(), r.Status)
	}
	// Manual retry is idempotent; a different request cannot overlap.
	server := &Server{pool: pool}
	if replay, err := server.queueScheduledRun(ctx, taskID, r.ID); err != nil || replay.ID != r.ID {
		t.Fatal("replay failed", err)
	}
	another, _ := randomUUID()
	if _, err := server.queueScheduledRun(ctx, taskID, another); err == nil {
		t.Fatal("overlap accepted")
	}
	// A fresh scheduler observes the persisted process ID, never starts it again.
	restarted := *scheduler
	scheduler = &restarted
	tick()
	if starts.Load() != 1 || polls.Load() != 1 {
		t.Fatal("restart replayed start")
	}
	mode = "offline"
	tick()
	if current().Status != "running" {
		t.Fatal("transient poll failure terminated run")
	}
	mode = "missing"
	tick()
	if current().Status != "uncertain" {
		t.Fatal("lost process not blocked")
	}
	exec(`UPDATE mira_scheduled_tasks SET next_run_at=$2 WHERE task_id=$1`, taskID, now.Add(-time.Hour))
	tick()
	if count() != 1 {
		t.Fatal("uncertainty allowed another run")
	}
	exec(`UPDATE mira_scheduled_runs SET status='acknowledged' WHERE task_id=$1`, taskID)
	mode = "lost-start"
	tick()
	tick()
	if starts.Load() != 2 || current().Status != "uncertain" {
		t.Fatal("lost start not blocked")
	}
	tick()
	if starts.Load() != 2 {
		t.Fatal("lost start replayed")
	}
	// A crash after recording dispatch intent is uncertain even with no process ID.
	exec(`UPDATE mira_scheduled_runs SET status='acknowledged' WHERE status='uncertain'`)
	id, _ := randomUUID()
	_, err := server.queueScheduledRun(ctx, taskID, id)
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE mira_scheduled_runs SET status='dispatching' WHERE run_id=$1`, id)
	tick()
	if current().Status != "uncertain" || starts.Load() != 2 {
		t.Fatal("interrupted dispatch replayed")
	}
	exec(`UPDATE mira_scheduled_runs SET status='acknowledged' WHERE status='uncertain'`)
	mode = "done"
	id, _ = randomUUID()
	_, err = server.queueScheduledRun(ctx, taskID, id)
	if err != nil {
		t.Fatal(err)
	}
	tick()
	if current().Status != "succeeded" {
		t.Fatal("exit not persisted")
	}
}

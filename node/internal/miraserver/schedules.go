package miraserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"
	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

const scheduleOutputLimit = 64 * 1024
const schedulerLock int64 = 0x4d495241534348

// No environment values or AI policy are stored in a task. Scripts load their
// own credentials on the execution Node. Arguments are passed without a shell.
type scriptSpec struct {
	Cron             string   `json:"cron"`
	Timezone         string   `json:"timezone"`
	Command          string   `json:"command"`
	Args             []string `json:"args"`
	CWD              string   `json:"cwd"`
	ExecutionContext string   `json:"executionContext,omitempty"`
}
type scheduledTask struct {
	ID     string `json:"taskId"`
	Name   string `json:"name"`
	NodeID string `json:"nodeId"`
	scriptSpec
	Enabled    bool      `json:"enabled"`
	Revision   int64     `json:"revision"`
	NextRunAt  time.Time `json:"nextRunAt"`
	State      string    `json:"state"`
	LastStatus string    `json:"lastStatus"`
}
type scheduledRun struct {
	ID           string     `json:"runId"`
	TaskID       string     `json:"taskId"`
	NodeID       string     `json:"nodeId"`
	Spec         scriptSpec `json:"spec"`
	ScheduledFor time.Time  `json:"scheduledFor"`
	Status       string     `json:"status"`
	ProcessID    string     `json:"processId"`
	Cursor       int64      `json:"outputCursor"`
	Output       string     `json:"output"`
	Truncated    bool       `json:"outputTruncated"`
	ExitCode     *int       `json:"exitCode"`
	Detail       string     `json:"detail"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	FinishedAt   *time.Time `json:"finishedAt"`
}

func scheduleError(status int, message string) error {
	return &HTTPError{Status: status, Code: "schedule_error", Message: message}
}
func (spec scriptSpec) schedule() (cron.Schedule, error) {
	if len(strings.Fields(spec.Cron)) != 5 || len(spec.Cron) > 128 || len(spec.Timezone) > 100 {
		return nil, fmt.Errorf("cron requires five fields and an IANA timezone")
	}
	zone, err := time.LoadLocation(spec.Timezone)
	if err != nil || spec.Timezone == "" || spec.Timezone == "Local" {
		return nil, fmt.Errorf("use an explicit IANA timezone")
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	value, err := parser.Parse(spec.Cron)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression")
	}
	value.(*cron.SpecSchedule).Location = zone
	return value, nil
}
func validScriptText(value string, limit int) bool {
	return len(value) <= limit && !strings.ContainsRune(value, 0) && utf8.ValidString(value)
}
func (task *scheduledTask) validate(now time.Time) error {
	if !uuidPattern.MatchString(task.ID) || !uuidPattern.MatchString(task.NodeID) {
		return scheduleError(400, "taskId and nodeId must be UUIDs")
	}
	if strings.TrimSpace(task.Name) == "" || !validScriptText(task.Name, 120) {
		return scheduleError(400, "name requires 1–120 bytes")
	}
	if task.Command == "" || !validScriptText(task.Command, 4096) || !validScriptText(task.CWD, 32768) || task.CWD == "" || len(task.Args) > 128 {
		return scheduleError(400, "command, cwd or arguments are invalid")
	}
	size := len(task.Command) + len(task.CWD)
	for _, arg := range task.Args {
		if !validScriptText(arg, 32768) {
			return scheduleError(400, "invalid argument")
		}
		size += len(arg)
	}
	if size > 48*1024 {
		return scheduleError(400, "command and arguments exceed 48 KiB")
	}
	if task.ExecutionContext != "" && task.ExecutionContext != "user" && task.ExecutionContext != "system" {
		return scheduleError(400, "executionContext must be user or system")
	}
	schedule, err := task.schedule()
	if err != nil {
		return scheduleError(400, err.Error())
	}
	task.NextRunAt = schedule.Next(now)
	if task.NextRunAt.IsZero() {
		return scheduleError(400, "cron has no occurrence within five years")
	}
	if task.Args == nil {
		task.Args = []string{}
	}
	return nil
}

const taskColumns = `t.task_id::text,t.name,t.node_id::text,t.spec,t.enabled,t.revision,t.next_run_at,
 COALESCE((SELECT status FROM mira_scheduled_runs r WHERE r.task_id=t.task_id ORDER BY created_at DESC,run_id DESC LIMIT 1),'')`

func scanScheduledTask(row pgx.Row) (*scheduledTask, error) {
	t := &scheduledTask{}
	var spec []byte
	err := row.Scan(&t.ID, &t.Name, &t.NodeID, &spec, &t.Enabled, &t.Revision, &t.NextRunAt, &t.LastStatus)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(spec, &t.scriptSpec)
	return t, err
}

const runColumns = `run_id::text,task_id::text,node_id::text,spec,scheduled_for,status,process_id,output_cursor,output,output_truncated,exit_code,detail,created_at,updated_at,finished_at`

func scanScheduledRun(row pgx.Row) (*scheduledRun, error) {
	r := &scheduledRun{}
	var spec []byte
	err := row.Scan(&r.ID, &r.TaskID, &r.NodeID, &spec, &r.ScheduledFor, &r.Status, &r.ProcessID, &r.Cursor, &r.Output, &r.Truncated, &r.ExitCode, &r.Detail, &r.CreatedAt, &r.UpdatedAt, &r.FinishedAt)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(spec, &r.Spec)
	return r, err
}
func (server *Server) getScheduledTask(ctx context.Context, id string) (*scheduledTask, error) {
	t, err := scanScheduledTask(server.pool.QueryRow(ctx, `SELECT `+taskColumns+` FROM mira_scheduled_tasks t WHERE task_id=$1 AND deleted_at IS NULL`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, scheduleError(404, "task not found")
	}
	if err != nil {
		return nil, err
	}
	t.State = "ready"
	switch t.LastStatus {
	case "queued", "dispatching", "running", "uncertain":
		t.State = t.LastStatus
	}
	if t.State == "ready" {
		if !t.Enabled {
			t.State = "paused"
		} else if !server.channel.IsConnected(t.NodeID) {
			t.State = "offline"
		}
	}
	return t, nil
}

// Trusted Nodes share the same command authority as process/start. Browser
// mutations retain administrator session/CSRF checks through authorize.
func (server *Server) routeSchedules(ctx context.Context, w http.ResponseWriter, r *http.Request) (bool, error) {
	if r.URL.Path != "/v1/schedules" && !strings.HasPrefix(r.URL.Path, "/v1/schedules/") {
		return false, nil
	}
	actor, err := server.authorize(ctx, w, r, "trusted", authOptions{ClientType: "cli"})
	if err != nil || actor == nil {
		return true, err
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/schedules"), "/")
	id, action := "", ""
	if len(parts) > 1 {
		id = parts[1]
		if !uuidPattern.MatchString(id) {
			return true, scheduleError(400, "invalid task UUID")
		}
	}
	if len(parts) > 2 {
		action = parts[2]
	}
	if len(parts) > 3 {
		return true, scheduleError(404, "route not found")
	}
	limit := boundedQueryInteger(r.URL.Query().Get("limit"), 50, 1, 100)
	if r.Method == "GET" {
		if id == "" {
			after := r.URL.Query().Get("after")
			if after != "" && !uuidPattern.MatchString(after) {
				return true, scheduleError(400, "invalid cursor")
			}
			rows, err := server.pool.Query(ctx, `SELECT task_id::text FROM mira_scheduled_tasks WHERE deleted_at IS NULL AND task_id::text>$1 ORDER BY task_id::text LIMIT $2`, after, limit+1)
			if err != nil {
				return true, err
			}
			ids := []string{}
			for rows.Next() {
				var key string
				if err = rows.Scan(&key); err != nil {
					break
				}
				ids = append(ids, key)
			}
			rowErr := rows.Err()
			rows.Close()
			if err != nil {
				return true, err
			}
			if rowErr != nil {
				return true, rowErr
			}
			more := len(ids) > int(limit)
			if more {
				ids = ids[:limit]
			}
			result := []*scheduledTask{}
			for _, key := range ids {
				t, err := server.getScheduledTask(ctx, key)
				if err != nil {
					return true, err
				}
				result = append(result, t)
			}
			next := ""
			if more {
				next = ids[len(ids)-1]
			}
			return true, writeJSON(w, 200, map[string]any{"data": result, "hasMore": more, "nextCursor": next})
		}
		if action == "" {
			t, err := server.getScheduledTask(ctx, id)
			if err != nil {
				return true, err
			}
			return true, writeJSON(w, 200, t)
		}
		if action == "runs" {
			before := r.URL.Query().Get("before")
			if before != "" && !uuidPattern.MatchString(before) {
				return true, scheduleError(400, "invalid cursor")
			}
			rows, err := server.pool.Query(ctx, `SELECT `+runColumns+` FROM mira_scheduled_runs WHERE task_id=$1 AND ($2='' OR (created_at,run_id)<(SELECT created_at,run_id FROM mira_scheduled_runs WHERE run_id::text=$2 AND task_id=$1)) ORDER BY created_at DESC,run_id DESC LIMIT $3`, id, before, limit+1)
			if err != nil {
				return true, err
			}
			defer rows.Close()
			runs := []*scheduledRun{}
			for rows.Next() {
				run, err := scanScheduledRun(rows)
				if err != nil {
					return true, err
				}
				runs = append(runs, run)
			}
			if err = rows.Err(); err != nil {
				return true, err
			}
			more := len(runs) > int(limit)
			if more {
				runs = runs[:limit]
			}
			next := ""
			if more {
				next = runs[len(runs)-1].ID
			}
			return true, writeJSON(w, 200, map[string]any{"data": runs, "hasMore": more, "nextCursor": next})
		}
		return true, scheduleError(404, "route not found")
	}
	var body struct {
		scheduledTask
		ExpectedRevision int64  `json:"expectedRevision"`
		RequestID        string `json:"requestId"`
		RunID            string `json:"runId"`
		ConfirmStopped   bool   `json:"confirmStopped"`
	}
	if err = foundation.ReadJSON(r, &body, 65536); err != nil {
		return true, err
	}
	// Log intent before mutation. No commands, arguments, outputs or secrets enter
	// the audit log; execution records are a separate authenticated resource.
	if err = foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "schedule.manage", Principal: actor, Request: r, Metadata: map[string]any{"method": r.Method, "action": action, "taskId": id}}, server.config.Foundation.TrustProxyHeaders); err != nil {
		return true, err
	}
	if (id == "" && r.Method == "POST") || (id != "" && action == "" && r.Method == "PUT") {
		t := body.scheduledTask
		if id != "" {
			t.ID = id
		}
		if err = t.validate(time.Now()); err != nil {
			return true, err
		}
		node, err := server.nodes.Get(ctx, t.NodeID, false)
		if err != nil {
			return true, err
		}
		if node == nil || node.Capabilities["processes"] != true {
			return true, scheduleError(400, "target must be an approved Node with process support")
		}
		spec, _ := json.Marshal(t.scriptSpec)
		if id == "" {
			result, err := server.pool.Exec(ctx, `INSERT INTO mira_scheduled_tasks(task_id,name,node_id,spec,enabled,next_run_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(task_id) DO NOTHING`, t.ID, t.Name, t.NodeID, spec, t.Enabled, t.NextRunAt)
			if err != nil {
				return true, err
			}
			if result.RowsAffected() == 0 {
				return true, scheduleError(409, "task already exists")
			}
		} else {
			result, err := server.pool.Exec(ctx, `UPDATE mira_scheduled_tasks SET name=$2,node_id=$3,spec=$4,enabled=$5,next_run_at=$6,revision=revision+1,updated_at=now() WHERE task_id=$1 AND revision=$7 AND deleted_at IS NULL`, id, t.Name, t.NodeID, spec, t.Enabled, t.NextRunAt, body.ExpectedRevision)
			if err != nil {
				return true, err
			}
			if result.RowsAffected() == 0 {
				return true, scheduleError(409, "task changed; refresh before editing")
			}
		}
		t2, err := server.getScheduledTask(ctx, t.ID)
		if err != nil {
			return true, err
		}
		return true, writeJSON(w, 200, t2)
	}
	if id == "" {
		return true, scheduleError(405, "method not allowed")
	}
	if action == "run" && r.Method == "POST" {
		if !uuidPattern.MatchString(body.RequestID) {
			return true, scheduleError(400, "requestId must be a UUID; reuse it after a lost response")
		}
		run, err := server.queueScheduledRun(ctx, id, body.RequestID)
		if err != nil {
			return true, err
		}
		return true, writeJSON(w, 202, run)
	}
	if action == "acknowledge" && r.Method == "POST" {
		if !body.ConfirmStopped || !uuidPattern.MatchString(body.RunID) {
			return true, scheduleError(400, "verify the previous script and its external effects have stopped, then set confirmStopped and runId")
		}
		result, err := server.pool.Exec(ctx, `UPDATE mira_scheduled_runs SET status='acknowledged',detail='Operator verified the previous execution has stopped; future runs unblocked',updated_at=now(),finished_at=now() WHERE task_id=$1 AND run_id=$2 AND status='uncertain'`, id, body.RunID)
		if err != nil {
			return true, err
		}
		if result.RowsAffected() == 0 {
			return true, scheduleError(409, "no matching uncertain execution")
		}
		return true, writeJSON(w, 200, map[string]any{"status": "acknowledged"})
	}
	if action == "" && (r.Method == "PATCH" || r.Method == "DELETE") {
		tx, err := server.pool.Begin(ctx)
		if err != nil {
			return true, err
		}
		defer tx.Rollback(ctx)
		var revision int64
		var spec scriptSpec
		var raw []byte
		err = tx.QueryRow(ctx, `SELECT revision,spec FROM mira_scheduled_tasks WHERE task_id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&revision, &raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return true, scheduleError(404, "task not found")
		}
		if err != nil {
			return true, err
		}
		if revision != body.ExpectedRevision {
			return true, scheduleError(409, "task changed; refresh before editing")
		}
		if r.Method == "DELETE" {
			var active bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_scheduled_runs WHERE task_id=$1 AND status IN ('queued','dispatching','running','uncertain'))`, id).Scan(&active)
			if err != nil {
				return true, err
			}
			if active {
				return true, scheduleError(409, "finish or resolve the active execution before deleting")
			}
			_, err = tx.Exec(ctx, `UPDATE mira_scheduled_tasks SET deleted_at=now(),enabled=false,revision=revision+1,updated_at=now() WHERE task_id=$1`, id)
		} else {
			if err = json.Unmarshal(raw, &spec); err != nil {
				return true, err
			}
			s, err := spec.schedule()
			if err != nil {
				return true, err
			}
			_, err = tx.Exec(ctx, `UPDATE mira_scheduled_tasks SET enabled=$2,next_run_at=$3,revision=revision+1,updated_at=now() WHERE task_id=$1`, id, body.Enabled, s.Next(time.Now()))
		}
		if err != nil {
			return true, err
		}
		if err = tx.Commit(ctx); err != nil {
			return true, err
		}
		return true, writeJSON(w, 200, map[string]any{"status": "updated"})
	}
	return true, scheduleError(405, "method not allowed")
}
func (server *Server) queueScheduledRun(ctx context.Context, taskID, runID string) (*scheduledRun, error) {
	tx, err := server.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	t, err := scanScheduledTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM mira_scheduled_tasks t WHERE task_id=$1 AND deleted_at IS NULL FOR UPDATE`, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, scheduleError(404, "task not found")
	}
	if err != nil {
		return nil, err
	}
	existing, err := scanScheduledRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM mira_scheduled_runs WHERE run_id=$1`, runID))
	if err == nil {
		if existing.TaskID != taskID {
			return nil, scheduleError(409, "requestId belongs to another task")
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	spec, _ := json.Marshal(t.scriptSpec)
	result, err := tx.Exec(ctx, `INSERT INTO mira_scheduled_runs(run_id,task_id,node_id,spec,scheduled_for,status) VALUES($1,$2,$3,$4,now(),'queued') ON CONFLICT DO NOTHING`, runID, t.ID, t.NodeID, spec)
	if err != nil {
		return nil, err
	}
	if result.RowsAffected() == 0 {
		return nil, scheduleError(409, "task already has an active or uncertain execution")
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return scanScheduledRun(server.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM mira_scheduled_runs WHERE run_id=$1`, runID))
}

type scriptScheduler struct {
	pool   *pgxpool.Pool
	online func(string) bool
	invoke func(context.Context, string, map[string]any) (any, error)
}

func (server *Server) startScriptScheduler(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	scheduler := &scriptScheduler{pool: server.pool, online: server.channel.IsConnected, invoke: func(ctx context.Context, node string, params map[string]any) (any, error) {
		return server.channel.Capabilities().Invoke(ctx, &foundation.Principal{Kind: "node", NodeID: node}, node, "process", params, channel.InvokeContext{Timeout: 10 * time.Second, AuditMetadata: map[string]any{"source": "scheduler"}})
	}}
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			if err := scheduler.tick(ctx, time.Now()); err != nil && ctx.Err() == nil {
				server.config.Logger.Printf("script scheduler: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

// A session advisory lock serializes complete ticks, including bounded dispatch
// calls. After a lost lock/session, an already-dispatched run is never reissued.
// This follows the single-Server v1 deployment contract, not distributed fencing.
func (s *scriptScheduler) tick(ctx context.Context, now time.Time) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, schedulerLock).Scan(&locked); err != nil || !locked {
		return err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, schedulerLock); err != nil {
			_ = conn.Conn().Close(unlockCtx)
		}
	}()
	if _, err = conn.Exec(ctx, `UPDATE mira_scheduled_runs SET status='uncertain',detail='Dispatch was interrupted; check the Node before acknowledging',updated_at=now() WHERE status='dispatching'`); err != nil {
		return err
	}
	// Resume observation of known processes after a Server restart. Limit the
	// number of simultaneous Node requests; offline jobs do not block other jobs.
	rows, err := conn.Query(ctx, `SELECT `+runColumns+` FROM mira_scheduled_runs WHERE status IN ('queued','running') ORDER BY updated_at LIMIT 32`)
	if err != nil {
		return err
	}
	runs := []*scheduledRun{}
	for rows.Next() {
		r, e := scanScheduledRun(rows)
		if e != nil {
			rows.Close()
			return e
		}
		runs = append(runs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	errs := make(chan error, len(runs))
	for _, r := range runs {
		if !s.online(r.NodeID) {
			if _, err := conn.Exec(ctx, `UPDATE mira_scheduled_runs SET updated_at=now() WHERE run_id=$1`, r.ID); err != nil {
				errs <- err
			}
			continue
		}
		slots <- struct{}{}
		wg.Add(1)
		go func(r *scheduledRun) { defer wg.Done(); defer func() { <-slots }(); errs <- s.observe(ctx, r) }(r)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			return e
		}
	}
	// Coalesce missed occurrences into one run. A unique partial index also
	// covers manual requests; uncertain executions block subsequent occurrences.
	rows, err = conn.Query(ctx, `SELECT `+taskColumns+` FROM mira_scheduled_tasks t WHERE enabled AND deleted_at IS NULL AND next_run_at<=$1 AND NOT EXISTS(SELECT 1 FROM mira_scheduled_runs r WHERE r.task_id=t.task_id AND status IN ('queued','dispatching','running','uncertain')) ORDER BY t.updated_at,t.task_id LIMIT 32`, now)
	if err != nil {
		return err
	}
	tasks := []*scheduledTask{}
	for rows.Next() {
		t, e := scanScheduledTask(rows)
		if e != nil {
			rows.Close()
			return e
		}
		tasks = append(tasks, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if !s.online(t.NodeID) {
			if _, err := conn.Exec(ctx, `UPDATE mira_scheduled_tasks SET updated_at=now() WHERE task_id=$1`, t.ID); err != nil {
				return err
			}
			continue
		}
		schedule, e := t.schedule()
		if e != nil {
			return e
		}
		next := schedule.Next(now)
		if next.IsZero() {
			continue
		}
		id, e := randomUUID()
		if e != nil {
			return e
		}
		tx, e := conn.Begin(ctx)
		if e != nil {
			return e
		}
		// Recheck revision under lock: pausing/editing cannot race a due selection.
		var revision int64
		e = tx.QueryRow(ctx, `SELECT revision FROM mira_scheduled_tasks WHERE task_id=$1 AND enabled AND deleted_at IS NULL FOR UPDATE`, t.ID).Scan(&revision)
		if errors.Is(e, pgx.ErrNoRows) || (e == nil && revision != t.Revision) {
			_ = tx.Rollback(ctx)
			continue
		}
		if e != nil {
			_ = tx.Rollback(ctx)
			return e
		}
		raw, _ := json.Marshal(t.scriptSpec)
		result, e := tx.Exec(ctx, `INSERT INTO mira_scheduled_runs(run_id,task_id,node_id,spec,scheduled_for,status) VALUES($1,$2,$3,$4,$5,'queued') ON CONFLICT DO NOTHING`, id, t.ID, t.NodeID, raw, t.NextRunAt)
		if e == nil && result.RowsAffected() > 0 {
			_, e = tx.Exec(ctx, `UPDATE mira_scheduled_tasks SET next_run_at=$2 WHERE task_id=$1`, t.ID, next)
		}
		if e != nil {
			_ = tx.Rollback(ctx)
			return e
		}
		if e = tx.Commit(ctx); e != nil {
			return e
		}
	}
	return nil
}
func (s *scriptScheduler) observe(ctx context.Context, r *scheduledRun) error {
	params := map[string]any{"action": "poll", "processId": r.ProcessID, "cursor": r.Cursor}
	if r.Status == "queued" {
		result, err := s.pool.Exec(ctx, `UPDATE mira_scheduled_runs SET status='dispatching',updated_at=now() WHERE run_id=$1 AND status='queued'`, r.ID)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return nil
		}
		args := make([]any, len(r.Spec.Args))
		for i, a := range r.Spec.Args {
			args[i] = a
		}
		params = map[string]any{"action": "start", "command": r.Spec.Command, "args": args, "cwd": r.Spec.CWD}
		if r.Spec.ExecutionContext != "" {
			params["executionContext"] = r.Spec.ExecutionContext
		}
	}
	result, err := s.invoke(ctx, r.NodeID, params)
	if err != nil {
		if r.Status == "queued" || strings.Contains(err.Error(), "managed process not found") || strings.Contains(err.Error(), "revoked") {
			_, saveErr := s.pool.Exec(ctx, `UPDATE mira_scheduled_runs SET status='uncertain',detail=$2,updated_at=now() WHERE run_id=$1`, r.ID, "Execution could not be confirmed; inspect the Node and script state before acknowledging. "+strings.ToValidUTF8(strings.ReplaceAll(truncate(err.Error(), 1500), "\x00", "�"), "�"))
			return saveErr
		}
		// A failed read is safe to retry, but never replay a start.
		_, saveErr := s.pool.Exec(ctx, `UPDATE mira_scheduled_runs SET detail='Waiting to reconnect to the process',updated_at=now() WHERE run_id=$1`, r.ID)
		return saveErr
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	var view struct {
		ProcessID string `json:"processId"`
		Running   *bool  `json:"running"`
		ExitCode  *int   `json:"exitCode"`
		Output    struct {
			Cursor int64 `json:"cursor"`
			Lost   bool  `json:"lostOutput"`
			Chunks []struct {
				Text string `json:"text"`
			} `json:"chunks"`
		} `json:"output"`
	}
	if err = json.Unmarshal(payload, &view); err != nil || view.ProcessID == "" || view.Running == nil || (!*view.Running && view.ExitCode == nil) {
		_, err = s.pool.Exec(ctx, `UPDATE mira_scheduled_runs SET status='uncertain',detail='Invalid process response; inspect the Node before acknowledging',updated_at=now() WHERE run_id=$1`, r.ID)
		return err
	}
	output := r.Output
	for _, chunk := range view.Output.Chunks {
		output += chunk.Text
	}
	output = strings.ReplaceAll(output, "\x00", "�")
	truncated := r.Truncated || view.Output.Lost
	if len(output) > scheduleOutputLimit {
		output = output[len(output)-scheduleOutputLimit:]
		for !utf8.ValidString(output) && len(output) > 0 {
			output = output[1:]
		}
		truncated = true
	}
	status := "running"
	var finished *time.Time
	if !*view.Running {
		status = "failed"
		if *view.ExitCode == 0 {
			status = "succeeded"
		}
		now := time.Now()
		finished = &now
	}
	_, err = s.pool.Exec(ctx, `UPDATE mira_scheduled_runs SET status=$2,process_id=$3,output_cursor=$4,output=$5,output_truncated=$6,exit_code=$7,detail='',updated_at=now(),finished_at=$8 WHERE run_id=$1`, r.ID, status, view.ProcessID, view.Output.Cursor, output, truncated, view.ExitCode, finished)
	return err
}

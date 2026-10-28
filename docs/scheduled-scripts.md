# Scheduled scripts

Mira schedules commands on approved Nodes without starting an AI runtime. Manage
jobs under **Devices → Scheduled tasks** (设备 → 定时任务), or `mira schedule --help`.
Scripts and credentials live on the target Node. Commands use the existing bounded
process capability and execute without an implicit shell; set `/bin/sh` and an
explicit script argument when a shell is needed. Windows defaults to the active
interactive user; LocalSystem is an explicit selection under the existing policy.

Example task JSON (use with `mira schedule create --file task.json`):

```json
{
  "name": "Daily transcription",
  "nodeId": "home-server",
  "cron": "30 6 * * *",
  "timezone": "Asia/Shanghai",
  "command": "/usr/bin/python3",
  "args": ["/home/user/jobs/transcribe.py"],
  "cwd": "/home/user/jobs",
  "enabled": true
}
```

Cron uses five fields (minute, hour, day, month, weekday), with explicit IANA
timezone data embedded in the Server. Standard cron DST behavior applies: missing
local times are skipped and repeated local times may run twice. An offline Node
retains its due time; missed occurrences coalesce into one run when it reconnects.
The Server checks every five seconds, with bounded batches and at most eight
simultaneous process requests. This is not a real-time scheduling guarantee.

PostgreSQL schema 42 owns tasks, immutable command snapshots per run, process IDs,
exit codes and the last 64 KiB of output. Lists and histories paginate. Schema
changes are additive and preserve rollback to the preceding Mira release. The
previous release simply does not execute these schedules.

Runs progress from queued to dispatching to running to succeeded/failed. An
acknowledged process ID is polled after a Server restart. If dispatch was
interrupted, its acknowledgement was lost, or the Node no longer retains that
process, the result is **uncertain** and blocks all later runs of that task.
Mira never retries a process start. After verifying both the script and any
external work it triggered have stopped, an operator may acknowledge that run
and allow future executions. A failed exit permits the next regular occurrence;
it does not cause an immediate retry or launch AI.

Pause affects future automatic triggers, not already queued/running work. Resume
or editing recalculates the next time from now. Run-now also works on paused tasks
and does not move their regular schedule. Deletion is soft and rejects active or
uncertain runs; history remains accessible by task UUID. Two different tasks may
run simultaneously; scripts accessing the same external resource should also use
an application lock and recovery state. There is no scheduler-enforced wall-time
kill: long jobs remain observed until they exit. Node shutdown follows the normal
managed-process termination policy. Scripts should persist durable progress and
not assume an HTTP disconnect cancels backend work.

The scheduler uses a PostgreSQL advisory lock for each complete dispatch/poll tick
and a unique active-run index. It follows Mira v1's single-Server contract; it is
not distributed lease fencing or an exactly-once external side-effect guarantee.
Node revocation prevents new capability calls. If a process can no longer be
queried, verify its state before unblocking it.

API: `GET/POST /v1/schedules`; `GET/PUT/PATCH/DELETE /v1/schedules/{UUID}`;
`POST .../run`; `GET .../runs`; `POST .../acknowledge`. Trusted Node credentials
have the existing process authority; browser mutations require administrator
cookies and CSRF. Create accepts a client-generated taskId; run-now requires a
requestId UUID, which is also the execution ID. Reuse these identifiers after a
lost response. Updates, pause/resume and deletion require expectedRevision. PUT
replaces the full configuration. PATCH changes enabled. Acknowledgement requires
runId and confirmStopped=true. No AI configuration or environment secrets are
accepted into the task specification.

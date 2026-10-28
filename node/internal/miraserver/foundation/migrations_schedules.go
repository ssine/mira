package foundation

// Schedules and bounded execution records are independent of Agent threads.
// Additive tables preserve the previous Server's rollback contract.
const scheduledScriptsSQL = `
CREATE TABLE mira_scheduled_tasks (
  task_id UUID PRIMARY KEY,
  name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 120),
  node_id UUID NOT NULL REFERENCES codex_nodes(node_id) ON DELETE RESTRICT,
  spec JSONB NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT true,
  revision BIGINT NOT NULL DEFAULT 1,
  next_run_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at TIMESTAMPTZ
);
CREATE INDEX mira_scheduled_tasks_due ON mira_scheduled_tasks(next_run_at) WHERE enabled AND deleted_at IS NULL;
CREATE TABLE mira_scheduled_runs (
  run_id UUID PRIMARY KEY,
  task_id UUID NOT NULL REFERENCES mira_scheduled_tasks(task_id) ON DELETE RESTRICT,
  node_id UUID NOT NULL REFERENCES codex_nodes(node_id) ON DELETE RESTRICT,
  spec JSONB NOT NULL,
  scheduled_for TIMESTAMPTZ NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('queued','dispatching','running','succeeded','failed','uncertain','acknowledged')),
  process_id TEXT NOT NULL DEFAULT '',
  output_cursor BIGINT NOT NULL DEFAULT 0,
  output TEXT NOT NULL DEFAULT '' CHECK(octet_length(output) <= 65536),
  output_truncated BOOLEAN NOT NULL DEFAULT false,
  exit_code INTEGER,
  detail TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX mira_scheduled_runs_one_active ON mira_scheduled_runs(task_id)
 WHERE status IN ('queued','dispatching','running','uncertain');
CREATE INDEX mira_scheduled_runs_history ON mira_scheduled_runs(task_id, created_at DESC, run_id DESC);
`

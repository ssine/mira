package foundation

// Keep the previous Server's INSERT/UPDATE contract intact. PostgreSQL derives
// this scalar on existing rows and whenever state changes, including writes by
// a rolled-back Server. It is rebuildable from state, not authoritative history.
// This rewrites the projection table once; it does not scan canonical events.
const accountCostSourceSQL = `
ALTER TABLE codex_thread_projections ADD COLUMN cost_forked_from_id TEXT
  GENERATED ALWAYS AS (coalesce(state#>>'{createdThread,forked_from_id}','')) STORED;
`

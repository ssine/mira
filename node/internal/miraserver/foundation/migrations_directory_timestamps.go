package foundation

// Rebuildable, timezone-independent inputs to the directory's existing date
// validation. Old Servers keep these current by writing state as before. Only
// the projection table is rewritten; canonical history is never touched.
const directoryTimestampsSQL = `
ALTER TABLE codex_thread_projections ADD COLUMN directory_timestamps TEXT[]
  GENERATED ALWAYS AS (ARRAY[state#>>'{metadata,updated_at}',
    state#>>'{metadata,advance_recency_at}',state#>>'{metadata,created_at}',
    state#>>'{createdThread,metadata,timestamp}']) STORED;
`

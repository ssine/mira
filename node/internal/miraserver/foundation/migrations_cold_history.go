package foundation

// Schema 49: cold history. Large, display-oriented Codex payloads may move to
// an S3-compatible bucket. Every history row stays in codex_thread_events with
// its ordering, commit and hash; a frozen row carries a small typed
// placeholder and cold_sha256, the content address of the exact original JSON
// bytes. Expand-only: the column is NULL until an operator enables freezing,
// so the previous Server keeps its SQL contract. Once rows are frozen, a Server
// older than this schema would read placeholders instead of bodies. The new
// column has no CHECK constraint, which would scan the whole (very large)
// table under an exclusive lock; the freezer writes only hex SHA-256 values.
const coldHistorySQL = `ALTER TABLE codex_thread_events ADD COLUMN cold_sha256 TEXT;
CREATE INDEX codex_thread_events_cold_idx ON codex_thread_events (cold_sha256) WHERE cold_sha256 IS NOT NULL;
CREATE OR REPLACE VIEW codex_thread_events_versioned AS
  SELECT items.store_id, items.thread_id, items.generation, items.item_seq, items.event_format_version,
    items.codex_version, items.payload, items.payload_sha256, items.created_at, items.operation_id,
    commits.event_seq AS store_event_seq, items.cold_sha256
  FROM codex_thread_events items JOIN codex_store_events commits USING(store_id, operation_id);
CREATE TABLE mira_cold_objects (
  sha256 TEXT PRIMARY KEY CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  raw_bytes BIGINT NOT NULL CHECK (raw_bytes >= 0),
  stored_bytes BIGINT NOT NULL CHECK (stored_bytes >= 0),
  uploaded_at TIMESTAMPTZ,
  deleting BOOLEAN NOT NULL DEFAULT false,
  orphaned_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX mira_cold_objects_orphaned_idx ON mira_cold_objects (orphaned_at) WHERE orphaned_at IS NOT NULL;
CREATE TABLE mira_cold_thread_scans (
  store_id TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  generation BIGINT NOT NULL CHECK (generation > 0),
  item_count BIGINT NOT NULL CHECK (item_count >= 0),
  scanned_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (store_id, thread_id)
);`

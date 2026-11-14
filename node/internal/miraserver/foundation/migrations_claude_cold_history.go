package foundation

// Schema 50: cold history for Claude. Large "user" records (tool results) in
// mira_claude_events and mira_claude_entries may hold a placeholder and
// cold_sha256, the SHA-256 of the exact original JSON text, like schema 49 for
// Codex. Expand-only: columns are NULL until the freezer runs. Adding a
// nullable column without a default or CHECK does not rewrite the table.
const claudeColdHistorySQL = `ALTER TABLE mira_claude_events ADD COLUMN cold_sha256 TEXT;
ALTER TABLE mira_claude_entries ADD COLUMN cold_sha256 TEXT;
CREATE INDEX mira_claude_events_cold_idx ON mira_claude_events (cold_sha256) WHERE cold_sha256 IS NOT NULL;
CREATE INDEX mira_claude_entries_cold_idx ON mira_claude_entries (cold_sha256) WHERE cold_sha256 IS NOT NULL;
CREATE TABLE mira_cold_claude_scans (
  session_id UUID PRIMARY KEY REFERENCES mira_claude_sessions ON DELETE CASCADE,
  events_seq BIGINT NOT NULL,
  entries_seq BIGINT NOT NULL,
  scanned_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

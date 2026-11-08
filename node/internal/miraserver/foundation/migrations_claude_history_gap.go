package foundation

// ClaudeHistoryGapPredicate identifies the Mira lifecycle records that open a
// Claude history gap: a native mirror_error or a degraded Mira completion/stop.
// The acknowledgement projection must use this exact expression so PostgreSQL
// can prove it implies the partial index predicate below. Without the index,
// every session read regex-scanned the whole raw event table.
const ClaudeHistoryGapPredicate = `((event_type='system' AND payload::text ~ '"subtype"[[:space:]]*:[[:space:]]*"mirror_error"') OR (event_type IN ('mira_completed','mira_execution_stopped') AND payload::text ~ '"degraded"[[:space:]]*:[[:space:]]*true'))`

const claudeHistoryGapSQL = `CREATE INDEX mira_claude_events_history_gap ON mira_claude_events(session_id,turn_id) WHERE ` + ClaudeHistoryGapPredicate + `;
CREATE INDEX mira_claude_turns_session ON mira_claude_turns(session_id,revision);
-- Native record times per transcript, so each subagent shows its own start and
-- latest activity. A rebuildable projection of mira_claude_entries.
ALTER TABLE mira_claude_transcripts ADD COLUMN started_at TIMESTAMPTZ, ADD COLUMN updated_at TIMESTAMPTZ;
CREATE FUNCTION mira_claude_entry_time(payload json) RETURNS TIMESTAMPTZ LANGUAGE plpgsql STABLE AS $$
BEGIN
 RETURN (payload->>'timestamp')::timestamptz;
EXCEPTION WHEN others THEN
 RETURN NULL; -- Unprojectable raw JSON (for example escaped NUL) stays authoritative.
END $$;
UPDATE mira_claude_transcripts t SET
 started_at=(SELECT min(mira_claude_entry_time(e.payload)) FROM (SELECT payload FROM mira_claude_entries
   WHERE session_id=t.session_id AND subpath=t.subpath ORDER BY seq LIMIT 32) e),
 updated_at=(SELECT max(mira_claude_entry_time(e.payload)) FROM (SELECT payload FROM mira_claude_entries
   WHERE session_id=t.session_id AND subpath=t.subpath ORDER BY seq DESC LIMIT 32) e);`

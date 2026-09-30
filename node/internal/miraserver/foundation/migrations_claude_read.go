package foundation

// A Claude session's read position is the highest event seq the administrator
// has acknowledged. Existing sessions start read so the upgrade marks nothing new.
const claudeReadStateSQL = `
ALTER TABLE mira_claude_sessions ADD COLUMN read_seq BIGINT NOT NULL DEFAULT 0;
UPDATE mira_claude_sessions s SET read_seq=COALESCE((SELECT max(seq) FROM mira_claude_events e WHERE e.session_id=s.session_id),0);
`

package foundation

// Claude native records are independent of Codex ThreadStore generations.
// JSON (not JSONB) preserves escaped NUL and future native fields.
const claudeSessionsSQL = `
CREATE TABLE mira_claude_sessions (
 session_id UUID PRIMARY KEY, request_id UUID NOT NULL UNIQUE, create_digest TEXT NOT NULL DEFAULT '',
 node_id UUID NOT NULL REFERENCES codex_nodes(node_id), cwd TEXT NOT NULL,
 title TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', effort TEXT NOT NULL DEFAULT '',
 archived BOOLEAN NOT NULL DEFAULT FALSE, revision BIGINT NOT NULL DEFAULT 1,
 active_turn UUID, persistence TEXT NOT NULL DEFAULT 'pending',
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX mira_claude_sessions_list ON mira_claude_sessions(archived, updated_at DESC, session_id);
CREATE TABLE mira_claude_turns (
 turn_id UUID PRIMARY KEY, session_id UUID NOT NULL REFERENCES mira_claude_sessions ON DELETE CASCADE,
 node_id UUID NOT NULL, revision BIGINT NOT NULL, runtime_id TEXT NOT NULL DEFAULT '', request JSON NOT NULL, status TEXT NOT NULL DEFAULT 'starting',
 error TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now(), completed_at TIMESTAMPTZ
);
CREATE TABLE mira_claude_transcripts (
 session_id UUID NOT NULL REFERENCES mira_claude_sessions ON DELETE CASCADE, subpath TEXT NOT NULL DEFAULT '',
 thread_id UUID NOT NULL UNIQUE, parent_thread_id UUID, source_kind TEXT NOT NULL DEFAULT 'claude_subagent', next_seq BIGINT NOT NULL DEFAULT 1,
 PRIMARY KEY(session_id,subpath)
);
CREATE TABLE mira_claude_entries (
 session_id UUID NOT NULL, subpath TEXT NOT NULL, seq BIGINT NOT NULL, entry_uuid TEXT, payload JSON NOT NULL,
 PRIMARY KEY(session_id,subpath,seq), UNIQUE(session_id,subpath,entry_uuid),
 FOREIGN KEY(session_id,subpath) REFERENCES mira_claude_transcripts ON DELETE CASCADE
);
CREATE TABLE mira_claude_operations (
 operation_id UUID PRIMARY KEY, session_id UUID NOT NULL REFERENCES mira_claude_sessions ON DELETE CASCADE,
 digest TEXT NOT NULL
);
CREATE TABLE mira_claude_events (
 seq BIGSERIAL PRIMARY KEY, event_id UUID NOT NULL UNIQUE,
 session_id UUID NOT NULL REFERENCES mira_claude_sessions ON DELETE CASCADE,
 turn_id UUID NOT NULL REFERENCES mira_claude_turns ON DELETE CASCADE, event_type TEXT NOT NULL, payload JSON NOT NULL
);
CREATE INDEX mira_claude_events_session ON mira_claude_events(session_id,seq);
CREATE INDEX mira_claude_events_transcript ON mira_claude_events(session_id,seq) WHERE event_type<>'stream_event';
`

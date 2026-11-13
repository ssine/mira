package foundation

// Schema 48: a rebuildable conversation index. Canonical Codex rollout rows and
// Claude events stay authoritative; these tables only hold display-sized
// copies (message prose, turn lifecycle and tool metadata) so full-transcript
// outlines, search, message navigation and timelines avoid detoasting tool
// output. Expand-only: the previous Server never reads or writes them.
//
// pg_trgm is an optional accelerator. A database that cannot create the
// contrib extension still migrates and searches with sequential scans.
const conversationIndexSQL = `DO $$ BEGIN
  BEGIN
    CREATE EXTENSION IF NOT EXISTS pg_trgm;
  EXCEPTION WHEN others THEN
    RAISE NOTICE 'pg_trgm unavailable; conversation search uses sequential scans';
  END;
END $$;
CREATE TABLE mira_conversation_index_state (
  store_id TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  generation BIGINT NOT NULL CHECK (generation > 0),
  indexed_count BIGINT NOT NULL DEFAULT 0 CHECK (indexed_count >= 0),
  projector_version INTEGER NOT NULL,
  carry JSONB NOT NULL DEFAULT '{}'::jsonb,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (store_id, thread_id)
);
CREATE TABLE mira_conversation_records (
  store_id TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  generation BIGINT NOT NULL CHECK (generation > 0),
  item_seq BIGINT NOT NULL CHECK (item_seq > 0),
  recorded_at TIMESTAMPTZ,
  kind TEXT NOT NULL DEFAULT '',
  record JSON NOT NULL,
  PRIMARY KEY (store_id, thread_id, generation, item_seq)
);
CREATE INDEX mira_conversation_records_turns ON mira_conversation_records (store_id, thread_id, generation, item_seq)
  WHERE kind IN ('turn_context', 'event_msg:task_started', 'event_msg:turn_started');
CREATE TABLE mira_conversation_messages (
  engine TEXT NOT NULL CHECK (engine IN ('codex','claude')),
  store_id TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  generation BIGINT NOT NULL CHECK (generation > 0),
  item_seq BIGINT NOT NULL CHECK (item_seq > 0),
  role TEXT NOT NULL CHECK (role IN ('user','assistant')),
  body TEXT NOT NULL,
  recorded_at TIMESTAMPTZ,
  PRIMARY KEY (engine, store_id, thread_id, generation, item_seq)
);
CREATE INDEX mira_conversation_messages_recent ON mira_conversation_messages (recorded_at DESC);
CREATE TABLE mira_claude_index_state (
  session_id UUID NOT NULL REFERENCES mira_claude_sessions ON DELETE CASCADE,
  subpath TEXT NOT NULL DEFAULT '',
  indexed_seq BIGINT NOT NULL DEFAULT 0,
  projector_version INTEGER NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, subpath)
);
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm') THEN
    EXECUTE 'CREATE INDEX mira_conversation_messages_trgm ON mira_conversation_messages USING gin (body gin_trgm_ops)';
  END IF;
END $$;`

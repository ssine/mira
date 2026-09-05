// Migration 19 is immutable after release. The partial index and its reader
// share this predicate; future changes require a new migration/index version.
// JSON text matching also handles raw rollout records with escaped NUL. Exact
// structural validation happens in the rebuildable reader, not in this index.
export const threadUpdatePredicate = `payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(user_message|agent_message|item_completed|view_image_tool_call|task_complete|turn_complete|turn_aborted|error|message|function_call_output|custom_tool_call_output)"'`;

export const threadReadStateMigration = `
  CREATE TABLE mira_thread_read_positions (
    store_id TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    item_count BIGINT NOT NULL CHECK (item_count >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (store_id, thread_id)
  );
  -- Existing history predates read tracking. Start with a quiet baseline;
  -- subsequent conversations/updates are unread until actually viewed.
  INSERT INTO mira_thread_read_positions(store_id,thread_id,generation,item_count)
    SELECT store_id,thread_id,active_generation,item_count FROM codex_thread_projections;
  CREATE INDEX codex_thread_events_visible_update_idx
    ON codex_thread_events(store_id,thread_id,generation,item_seq DESC)
    WHERE ${threadUpdatePredicate};
`;

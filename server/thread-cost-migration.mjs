// Canonical payload is JSON and may contain escaped NUL: select candidates as
// text and validate their structure in JavaScript without JSONB conversion.
export const modelEventPredicate = `payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(turn_context|thread_settings_applied)"'`;
export const costEventPredicate = `payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(turn_context|token_count|thread_settings_applied)"'`;
export const costEventMigration = `CREATE INDEX codex_thread_events_cost_idx
  ON codex_thread_events(store_id,thread_id,generation,item_seq)
  WHERE ${costEventPredicate};
CREATE INDEX codex_thread_events_model_idx
  ON codex_thread_events(store_id,thread_id,generation,item_seq)
  WHERE ${modelEventPredicate};`;

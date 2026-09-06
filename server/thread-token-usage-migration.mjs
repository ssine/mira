export const tokenUsagePredicate = `payload::text ~ '"type"[[:space:]]*:[[:space:]]*"token_count"'`;
export const tokenUsageMigration = `CREATE INDEX codex_thread_events_token_usage_idx
  ON codex_thread_events(store_id,thread_id,generation,item_seq DESC)
  WHERE ${tokenUsagePredicate};`;

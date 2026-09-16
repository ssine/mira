package foundation

// Disposable read projections only. The previous Server ignores these tables;
// canonical publication and the rollback SQL contract are unchanged.
const accountCostProjectionSQL = `
CREATE TABLE mira_account_cost_checkpoints (
  store_id TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  generation BIGINT NOT NULL,
  revision TEXT NOT NULL,
  source_key TEXT NOT NULL,
  item_seq BIGINT NOT NULL DEFAULT 0,
  checkpoint JSONB NOT NULL DEFAULT '{}',
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  retry_at TIMESTAMPTZ,
  error_code TEXT,
  PRIMARY KEY(store_id,thread_id),
  FOREIGN KEY(store_id,thread_id) REFERENCES codex_thread_projections ON DELETE CASCADE
);
CREATE TABLE mira_account_cost_entries (
  store_id TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  item_seq BIGINT NOT NULL,
  happened_at TIMESTAMPTZ NOT NULL,
  turn_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  observed BOOLEAN NOT NULL,
  priced BIGINT NOT NULL,
  unpriced BIGINT NOT NULL,
  long_requests BIGINT NOT NULL,
  input NUMERIC NOT NULL,
  cached NUMERIC NOT NULL,
  write NUMERIC NOT NULL,
  output NUMERIC NOT NULL,
  models TEXT[] NOT NULL,
  reasons TEXT[] NOT NULL,
  PRIMARY KEY(store_id,thread_id,item_seq),
  FOREIGN KEY(store_id,thread_id) REFERENCES mira_account_cost_checkpoints ON DELETE CASCADE
);
CREATE INDEX mira_account_cost_entries_time_idx ON mira_account_cost_entries(happened_at);
CREATE INDEX mira_codex_execution_events_cost_turn_idx
  ON mira_codex_execution_events(store_id,thread_id,generation,turn_id,event_seq DESC)
  WHERE kind IN ('turn/started','turn/completed');
CREATE INDEX mira_codex_execution_events_cost_binding_idx
  ON mira_codex_execution_events(store_id,thread_id,generation,created_at,event_seq)
  WHERE kind IN ('bound','turn_requested');
`

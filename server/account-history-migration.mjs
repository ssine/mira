export const accountHistoryMigration = `
  CREATE TABLE mira_account_quota_samples (
    node_id UUID NOT NULL REFERENCES codex_nodes(node_id),
    runtime_key TEXT NOT NULL,
    sampled_at TIMESTAMPTZ NOT NULL,
    sample_slot BIGINT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('ok', 'error')),
    account_type TEXT,
    email TEXT,
    plan_type TEXT,
    remaining DOUBLE PRECISION CHECK (remaining BETWEEN 0 AND 100),
    resets_at TIMESTAMPTZ,
    reset_count BIGINT CHECK (reset_count >= 0),
    PRIMARY KEY (node_id, runtime_key, sample_slot)
  );
  CREATE INDEX mira_account_quota_samples_time_idx
    ON mira_account_quota_samples(node_id, runtime_key, sampled_at DESC);
`;

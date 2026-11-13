package foundation

// Claude.ai plan usage observations. Usage holds only the Node's cleaned
// projection (window percentages and reset times), never credentials or raw
// upstream payloads. Expand-only: older Servers ignore this table.
const claudePlanUsageSQL = `CREATE TABLE mira_claude_account_usage_samples (
  node_account_id UUID NOT NULL REFERENCES mira_claude_accounts(node_account_id) ON DELETE CASCADE,
  sample_slot BIGINT NOT NULL,
  sampled_at TIMESTAMPTZ NOT NULL,
  credential_revision BIGINT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('ok','error')),
  usage JSONB,
  PRIMARY KEY (node_account_id, sample_slot)
);
CREATE INDEX mira_claude_account_usage_samples_time
  ON mira_claude_account_usage_samples(node_account_id, sampled_at);`

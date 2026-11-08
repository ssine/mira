package foundation

const accountEgressSQL = `ALTER TABLE mira_claude_accounts
  ADD COLUMN egress JSONB NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN egress_configured BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE mira_node_codex_accounts
  ADD COLUMN egress JSONB NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN egress_configured BOOLEAN NOT NULL DEFAULT false;`

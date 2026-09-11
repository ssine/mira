package foundation

// Expand only: the previous Server can continue using the singular Node runtime
// and legacy quota tables throughout the rollback window.
const codexAccountsSQL = `
CREATE TABLE mira_codex_accounts (
  account_id UUID PRIMARY KEY,
  name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 128),
  provider TEXT NOT NULL DEFAULT 'openai',
  auth_type TEXT NOT NULL DEFAULT 'chatgpt',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE mira_node_codex_accounts (
  node_account_id UUID PRIMARY KEY,
  node_id UUID NOT NULL REFERENCES codex_nodes(node_id),
  account_id UUID NOT NULL REFERENCES mira_codex_accounts(account_id),
  is_default BOOLEAN NOT NULL DEFAULT false,
  enabled BOOLEAN NOT NULL DEFAULT true,
  revision BIGINT NOT NULL DEFAULT 1,
  credential_revision BIGINT NOT NULL DEFAULT 1,
  desired JSONB NOT NULL DEFAULT '{}',
  reported JSONB NOT NULL DEFAULT '{}',
  account_snapshot JSONB,
  observed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE(node_id,account_id)
);
CREATE UNIQUE INDEX mira_node_codex_accounts_default_idx ON mira_node_codex_accounts(node_id) WHERE is_default;
CREATE TABLE mira_codex_account_protocols (
  node_account_id UUID NOT NULL REFERENCES mira_node_codex_accounts(node_account_id),
  runtime_id UUID NOT NULL,
  protocol INTEGER NOT NULL,
  observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY(node_account_id,runtime_id)
);
INSERT INTO mira_codex_accounts(account_id,name)
  SELECT md5('mira:legacy-account:' || node_id::text)::uuid, '默认账号' FROM codex_nodes
  WHERE capabilities->>'appServer'='true';
INSERT INTO mira_node_codex_accounts(node_account_id,node_id,account_id,is_default)
  SELECT md5('mira:legacy-binding:' || node_id::text)::uuid,node_id,
    md5('mira:legacy-account:' || node_id::text)::uuid,true FROM codex_nodes
  WHERE capabilities->>'appServer'='true';
CREATE TABLE mira_codex_account_samples (
  node_account_id UUID NOT NULL REFERENCES mira_node_codex_accounts(node_account_id),
  account_id UUID NOT NULL REFERENCES mira_codex_accounts(account_id),
  sample_slot BIGINT NOT NULL,
  sampled_at TIMESTAMPTZ NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('ok','error','identity_changed')),
  identity_key TEXT,
  account JSONB,
  limits JSONB,
  PRIMARY KEY(node_account_id,sample_slot)
);
CREATE INDEX mira_codex_account_samples_account_idx ON mira_codex_account_samples(account_id,sampled_at);
CREATE TABLE mira_codex_execution_events (
  event_seq BIGSERIAL PRIMARY KEY,
  operation_id UUID NOT NULL UNIQUE,
  store_id TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  generation BIGINT NOT NULL CHECK(generation>0),
  node_account_id UUID NOT NULL REFERENCES mira_node_codex_accounts(node_account_id),
  runtime_id TEXT NOT NULL,
  revision BIGINT NOT NULL,
  kind TEXT NOT NULL,
  turn_id TEXT,
  detail JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX mira_codex_execution_events_thread_idx ON mira_codex_execution_events(store_id,thread_id,event_seq);
CREATE UNIQUE INDEX mira_codex_encrypted_failure_idx ON mira_codex_execution_events(store_id,thread_id,generation,node_account_id,runtime_id,turn_id)
  WHERE kind='invalid_encrypted_content' AND turn_id IS NOT NULL;
CREATE TABLE mira_codex_execution_routes (
  store_id TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  generation BIGINT NOT NULL CHECK(generation>0),
  node_account_id UUID NOT NULL REFERENCES mira_node_codex_accounts(node_account_id),
  runtime_id TEXT NOT NULL,
  revision BIGINT NOT NULL,
  state TEXT NOT NULL,
  turn_id TEXT,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY(store_id,thread_id)
);
CREATE TABLE mira_codex_input_compatibility (
  decision_id UUID PRIMARY KEY,
  store_id TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  generation BIGINT NOT NULL,
  node_account_id UUID NOT NULL REFERENCES mira_node_codex_accounts(node_account_id),
  model TEXT NOT NULL,
  credential_revision BIGINT NOT NULL DEFAULT 1,
  policy TEXT NOT NULL,
  item_refs JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE mira_codex_thread_runtimes ADD COLUMN node_account_id UUID REFERENCES mira_node_codex_accounts(node_account_id);
ALTER TABLE mira_appserver_thread_start_requests ADD COLUMN node_account_id UUID REFERENCES mira_node_codex_accounts(node_account_id);
ALTER TABLE mira_appserver_thread_start_requests ADD COLUMN fingerprint_version INTEGER NOT NULL DEFAULT 1;
`

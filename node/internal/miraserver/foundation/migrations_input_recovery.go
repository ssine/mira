package foundation

// Preferences belong to one generation; forks/recreated threads default on.
// Attempts are durable dispatch receipts, never a queue that replays turn/start
// after an ambiguous acknowledgement or a Server restart.
const automaticInputRecoverySQL = `
CREATE TABLE mira_codex_recovery_preferences (
 store_id TEXT NOT NULL, thread_id TEXT NOT NULL, generation BIGINT NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT TRUE, updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(store_id,thread_id,generation)
);
CREATE INDEX mira_codex_recovery_enabled_idx ON mira_codex_recovery_preferences(store_id,thread_id,generation) WHERE enabled;
CREATE TABLE mira_codex_recovery_attempts (
 failure_id UUID PRIMARY KEY REFERENCES mira_codex_execution_events(operation_id),
 store_id TEXT NOT NULL, thread_id TEXT NOT NULL, generation BIGINT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('applying','dispatching','completed','stopped')),
 reason TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX mira_codex_recovery_attempts_thread_idx ON mira_codex_recovery_attempts(store_id,thread_id,generation,created_at DESC);
`

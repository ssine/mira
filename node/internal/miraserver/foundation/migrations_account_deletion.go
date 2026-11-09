package foundation

// Keep account IDs and their historical references. Old Servers still honor
// enabled=false and running=false throughout the rollback window.
const accountDeletionSQL = `ALTER TABLE mira_node_codex_accounts ADD COLUMN deleted_at TIMESTAMPTZ;`

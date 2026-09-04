# Codex session import v1

Mira can migrate rollout JSONL files from a desktop/server Node into the PostgreSQL ThreadStore.
Discovery never uploads content: it returns bounded metadata from `APP_SERVER_CODEX_HOME`,
`CODEX_HOME` and the OS user's default `.codex` directory. Android does not advertise this
capability.

## Flow

1. The administrator asks Mira Server to scan one approved online Node.
2. The Node walks only `sessions/` below its detected Codex homes, accepts regular `rollout-*.jsonl`
   files, and returns at most 2,000 summaries.
3. The administrator selects one path. Server reads line-aligned chunks through the reverse Node
   channel, with an 8 MiB chunk limit, 256 MiB total limit and 500,000 record limit.
4. Server rejects a file that changes size during transfer, invalid JSON, invalid rollout envelopes
   or a missing UUID thread ID.
5. In one staging transaction, Server records import provenance and every unmodified source record
   in the append-only `mira_codex_session_import_records` table. A SHA-256 digest identifies the
   exact source version and makes retries idempotent.
6. Server adapts the records to canonical `{type, payload}` ThreadStore items and commits them using
   the normal v2 expected-version, generation and operation-ID rules. The source record remains
   available even when a compatibility normalization is required. A local `paginated` session is
   exposed as `legacy` to the current remote adapter because it does not yet advertise `list_turns`
   / `list_items`; the original history mode and records remain intact in import provenance.
7. The thread becomes visible in `/v1/codex/threads` and can be resumed by a Mira-compatible Codex
   App Server on any selected Node.

## Conflict policy

An absent thread is created. If PostgreSQL contains a strict prefix of the local JSONL, only the
suffix is appended. If the local JSONL is an unchanged or shorter prefix, no history is overwritten.
If both histories contain different items at the same position, the import is marked failed with
`history_diverged`; the staged raw source is retained for diagnosis and PostgreSQL remains unchanged.

Subagent rollout files are imported as independent threads. `parent_thread_id` and source metadata
are projected so the tree can be reconstructed without flattening child history into the parent.

## Trust boundary

Scan and import routes require the administrator session and CSRF proof. A Node only reads files
under detected Codex session directories using its existing OS identity. `codexSessions` is an
administrative migration capability and is intentionally not part of the model-facing
`home_nodes` dynamic tool namespace.

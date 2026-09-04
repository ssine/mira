# Codex session import v1

Mira can migrate rollout JSONL files from a desktop/server Node into the PostgreSQL ThreadStore.
Discovery never uploads content: it returns bounded metadata from `APP_SERVER_CODEX_HOME`,
`CODEX_HOME` and the OS user's default `.codex` directory. Android does not advertise this
capability.

## Flow

1. The administrator asks Mira Server to scan one approved online Node.
2. The Node walks `sessions/` and `archived_sessions/` below its detected Codex homes, accepts regular
   `rollout-*.jsonl` files, and returns at most 2,000 summaries. Summaries include `originator`,
   `clientKind` (`desktop`, `cli`, `ide`, `subagent`, `unknown`), original cwd/version and `archived`.
   `source: vscode` alone does not identify Desktop; `originator: Codex Desktop` does.
3. The administrator selects one path. Server reads binary/base64 chunks through the reverse Node
   channel and incrementally decodes UTF-8/JSONL. Chunk size is a memory/backpressure setting,
   **not a total-size or record-count limit**. A single JSONL record can cross chunk boundaries.
   Older Nodes retain line-aligned UTF-8 reads; update them for large individual records.
4. Server rejects a file that changes size/modification time during transfer, invalid JSON, invalid rollout envelopes
   or a missing UUID thread ID.
5. In one staging transaction, Server records import provenance and every unmodified source record
   in the append-only `mira_codex_session_import_records` table. A SHA-256 digest identifies the
   exact source version and makes retries idempotent.
6. Server adapts the records to canonical `{type, payload}` ThreadStore items and commits them under
   the same store writer lock, generation and operation-ID rules as v2 commits. Staged rows and
   existing history are compared/read in batches; the complete thread is published in one transaction.
   Cancellation rolls back that transaction; it never exposes a partial thread. The source record remains
   available even when a compatibility normalization is required. A local `paginated` session is
   exposed as `legacy` to the current remote adapter because it does not yet advertise `list_turns`
   / `list_items`; the original history mode and records remain intact in import provenance.
7. The thread becomes visible in `/v1/codex/threads` and can be resumed by a Mira-compatible Codex
   App Server on any selected Node. The Web import list offers **Open conversation** after import;
   it selects the source runtime and preserves its original cwd. Import alone does not run a model.

This imports local **Codex Desktop** sessions, not ordinary cloud ChatGPT chats. Desktop source files
are never rewritten or removed. Stop writing the source session while importing it. Fork/subagent
IDs and upstream history-base metadata are retained in provenance. Referenced forks follow upstream
`RolloutLineage`: resolve immutable rollout IDs in the same home, read only `end_byte_offset`, validate
sequential ordinals ending at `end_ordinal_exclusive`, then order ancestor bodies oldest-first before
the child's body. Ancestor `session_meta` records are excluded. Canonical history starts with the
child's metadata adapted to legacy/copy semantics (history base and paginated cutoffs cleared).
Fork/subagent parent IDs remain; only the selected child becomes a live thread. Ancestors need not
already exist in PostgreSQL. Later ancestor turns are never copied into the fork.

`codexSessions` adds `resolve` with `path` (source scope) and `rolloutId` (UUID filename suffix).
It searches sessions/archives independently of the bounded summary page and rejects missing or
ambiguous matches. Cycles, invalid byte/ordinal boundaries and changing sources fail before publication.
Schema 14 adds `source_boundary` and immutable `mira_codex_session_import_segments`. Prefix provenance
is distinct from whole-file imports. Segments retain source import ID, line range and original boundary;
the copied history remains auditable without the source device. Update Nodes before importing forks.

## Progress and cancellation

`POST /v1/nodes/:nodeId/codex-session-imports` keeps the existing JSON response for API clients.
With `Accept: application/x-ndjson`, it streams `progress`, `heartbeat`, then `complete` or `error`:

```json
{"type":"progress","phase":"reading","bytes":4194304,"totalBytes":336087668,"records":213}
{"type":"progress","phase":"publishing","records":100,"totalRecords":83225}
```

Other phases are `scanning`, `resolving` and `validating`. Aborting the fetch/closing the connection requests
cancellation. It takes effect at the next chunk/DB operation boundary. Once the final transaction
commits, cancellation cannot undo it; rescan to resolve a lost completion acknowledgement. A complete
staged source may remain as immutable provenance after a cancelled publish, but it is not a live thread.

Schema migration 13 stores raw records, canonical rollout payloads and compatibility snapshots as
PostgreSQL `JSON`, not `JSONB`: tool output can contain escaped NUL/unpaired UTF-16 surrogates that
JSONB rejects. Original records and SHA-256 remain intact. Metadata and indexed projections remain
JSONB; do not run PostgreSQL JSON extraction on arbitrary raw tool output. See
[PostgreSQL JSON types](https://www.postgresql.org/docs/current/datatype-json.html).
Large imports invalidate the derived whole-store snapshot cache; v1 reads rebuild it on demand.
No file-size ceiling is imposed, but finite disk/RAM and upstream model constraints still apply.
Migration 13 rewrites three columns and acquires table locks: schedule deployment with a backup and
drained writers. It does not change the external v1/v2 JSON protocol.

## Conversation attachments

The Web composer imposes no per-file, combined-size or attachment-count ceiling. Files and images
are uploaded in offset-checked chunks (`file.write`, `append: true`, `offset`) into a unique temporary
batch directory on the selected runtime Node. Images use App Server `localImage` paths; other files
are referenced in the text input. Byte/count progress and cancellation are visible in the composer.
Cancelled/failed uploads remove only their own temporary batch; they do not call `turn/start`.
Nodes must be updated for chunked append support. The 4 MiB file capability chunk limit remains a
transport bound, not a file limit. Download/preview also has progress; closing its dialog cancels
reading, and text preview loads additional pages on demand.

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

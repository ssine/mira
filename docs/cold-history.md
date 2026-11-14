# Cold history: idle conversation bodies in object storage

Implemented by schema 49 (Mira 1.0.114) for Codex and schema 50 (Mira 1.0.116) for Claude.
Disabled unless a bucket is configured.

## Why

On 2026-11-14 the two personal deployments held:

| | Database | `codex_thread_events` | Where the bytes are |
| --- | --- | --- | --- |
| Home Server | 39 GB | 28 GB | 12 GB exact duplicates from forks; 7.4 GB in payloads of 16 KB or more |
| Beta-SG | 112 GB | 107 GB | 96 GB in 60,000 payloads of 64 KB or more: base64 screenshots in code-mode tool output |

More than 95% of those bytes belonged to conversations without writes for 30 days. The Web console
reads display-sized copies (`docs/conversation-index.md`); canonical bodies are needed only for tool
details, images, Codex resume, account handoff and re-indexing.

## Model

- PostgreSQL keeps **every** history row: `(store, thread, generation, item_seq)`, commit,
  `payload_sha256`, `created_at`. Counts, ordering, pagination, joins and generations are unchanged.
- A frozen row stores a small typed placeholder instead of the body and sets `cold_sha256`, the
  SHA-256 of the exact original JSON text:

  ```json
  {"type":"response_item","timestamp":"…","payload":{"type":"custom_tool_call_output"},
   "mira_cold_payload":{"version":1,"sha256":"…","bytes":294516}}
  ```

  Record type, payload type, role and timestamp are kept because SQL filters on them.
- The bucket holds one immutable gzip object per distinct body at
  `<prefix>payloads/<sha[0:2]>/<sha>.json.gz`. Identical bodies (fork copies) share one object.
  gzip recovers most of the base64 overhead of embedded images; images are not re-encoded.

## What freezes

A row qualifies only when all hold:

1. its conversation projection and the row itself are older than `FREEZE_AFTER` (30 days);
2. its stored size is at least `MIN_BYTES` (16 KiB compressed);
3. its text matches none of the metadata predicates used by cost, token usage, model, lifecycle and
   read-state views (`session_meta`, `turn_context`, `thread_settings_applied`, `token_count`,
   task/turn lifecycle, `error`, `agent_message`, `item_completed`), anywhere in the text;
4. it is a `response_item` other than an assistant message, a `compacted` record, or an
   `event_msg` whose type is not one of the above.

Rule 3 makes every metadata view select the same rows with the same bytes before and after
freezing, so sidebar enrichment, costs, account cost rebuilds, activity and execution state need no
cold reads. Search uses `mira_conversation_messages`, which stays in PostgreSQL.

## Claude sessions

Claude history (`mira_claude_events`, `mira_claude_entries`) uses the same bucket, objects and
collector. Only `type: "user"` records move: they carry tool results such as file contents,
command output and screenshots, about half of Claude's bytes on the Home Server. Assistant prose,
results (cost and usage), system, stream and Mira lifecycle records always stay hot, so live usage,
cost projection, unread state and history-gap checks never read the bucket.

A session freezes when it has no active or running turn, no turn activity and no update within
`FREEZE_AFTER`, and a current conversation index for its root and every native subagent
transcript. The placeholder keeps the small top-level fields that cheap filters read (`type`,
`subtype`, `parent_tool_use_id`, `timestamp`, `uuid`, `parentUuid`, `session_id`, `isSidechain`,
`agentId`). Event pages, the root view, native transcript streams (prefetched before the first byte),
timelines and re-indexing hydrate; the native cache prefix protocol compares the content address
directly, which equals the SHA-256 of the canonical bytes, so it never downloads. Deleting a session
cascades its rows and the collector removes the orphaned objects after the grace period.

## Reads

`coldstore.Hydrate` is called by every reader of canonical bodies: Codex ThreadStore history (and
its stream), event listing, snapshot compatibility, delta rebase, Web transcripts and tool details,
transcript images, conversation re-indexing, account handoff and imports. It:

- returns hot rows unchanged;
- serves a verified local cache file, otherwise downloads, decompresses and verifies the SHA-256;
- coalesces concurrent downloads of one object;
- fails with an explicit error when the bucket is unavailable, an object is missing or corrupt, or
  no bucket is configured. It never returns a placeholder as history.

History streams prefetch all frozen bodies (8 in parallel) before writing the first byte, so a
failure is a clean HTTP error, not a truncated body. The cache is bounded (`CACHE_BYTES`, default
2 GiB, least recently used eviction).

## Freezing and collection

The freezer runs in the Server worker 10 minutes after start, so a Supervisor rollback after a
failed health check never meets placeholders. It visits idle conversations whose generation or item
count changed since their last scan and that the conversation index already covers (so the index
backfill after an upgrade reads hot rows instead of downloading), and per row:

1. compresses the body and verifies the compressed bytes decode to the same SHA-256;
2. uploads with `Content-MD5` unless an uploaded object with that address exists;
3. in one transaction, locks the object row, clears its orphan mark and replaces the body with the
   placeholder only if `payload_sha256` still matches and the row is not frozen.

New writes always append to PostgreSQL; resuming a cold conversation never thaws it.

Hourly collection marks objects no row references, and after `OrphanGrace` (24 h) marks them
`deleting`, deletes the object and then its row. The freezer never references an object that is
being deleted, and referencing clears the orphan mark under the same row lock, so collection cannot
remove an object that a committed row points to. Permanent conversation deletion therefore removes
cold bodies within about a day; bucket versioning may retain them for its own retention period.

## Configuration

All variables belong to the Server worker.

| Variable | Default | Meaning |
| --- | --- | --- |
| `MIRA_NODE_COLD_STORAGE_BUCKET` | (off) | Enables cold reads. |
| `MIRA_NODE_COLD_STORAGE_ENDPOINT` | | e.g. `https://oss-cn-beijing.aliyuncs.com` |
| `MIRA_NODE_COLD_STORAGE_REGION` | | SigV4 region, e.g. `oss-cn-beijing` |
| `MIRA_NODE_COLD_STORAGE_PREFIX` | | Per-deployment key prefix, e.g. `homeserver/` |
| `MIRA_NODE_COLD_STORAGE_ACCESS_KEY_ID` | | |
| `MIRA_NODE_COLD_STORAGE_SECRET_ACCESS_KEY` or `_FILE` | | Prefer the file form (mode 0600). |
| `MIRA_NODE_COLD_STORAGE_PATH_STYLE` | `false` | `true` for MinIO-style endpoints. |
| `MIRA_NODE_COLD_STORAGE_CACHE_DIR` | user cache `/mira/cold-history` | Absolute path. |
| `MIRA_NODE_COLD_STORAGE_CACHE_BYTES` | 2 GiB | `0` disables caching. |
| `MIRA_NODE_COLD_STORAGE_FREEZE` | `false` | `true` starts the freezer and collector. |
| `MIRA_NODE_COLD_STORAGE_FREEZE_AFTER` | `720h` | At least `1h`. |
| `MIRA_NODE_COLD_STORAGE_MIN_BYTES` | `16384` | At least 4096. |
| `MIRA_NODE_COLD_STORAGE_UPLOAD_CONCURRENCY` | `4` | Parallel uploads and conversations scanned at once (1–64). |
| `MIRA_NODE_COLD_STORAGE_TCP_CONGESTION` | (host default) | Linux only: congestion control for this client's sockets, e.g. `bbr`. |

The client speaks AWS Signature Version 4 and works with Aliyun OSS, MinIO, Tencent COS and other
S3-compatible stores. Give the Server a credential limited to its prefix: Get/Put/Delete object,
abort multipart upload and list parts. It does not need to delete object versions.

## Operations

- **Rollback.** Once rows are frozen, a Server older than 1.0.114 returns placeholders as history
  (older than 1.0.116 for Claude sessions). Do not roll back below the version that froze them; keep the bucket configuration on newer
  versions. Disabling `FREEZE` stops new freezing; reads still need the bucket.
- **Backups.** `pg_dump` no longer contains frozen bodies. The bucket is part of the backup: keep
  versioning on and do not expire current object versions.
- **Disk space.** Freezing frees space inside each table's TOAST storage, and later writes to the
  same table reuse it, but the data files do not shrink and freed space is not shared across tables.
  Use `pg_repack` online (or `VACUUM FULL` in a maintenance window) to return space to the file
  system.
- **Large migrations.** Schema 49 indexes `codex_thread_events`; on a large table this can exceed
  the Supervisor's candidate health deadline and roll the update back. Apply the identical released
  SQL and its ledger row while the previous Server runs (it is expand-only), then update.
- **Transient failures.** Object requests retry dropped connections, request timeouts, throttling
  and 5xx responses up to four times with exponential backoff (objects are immutable and uploads carry
  `Content-MD5`). A row whose upload still fails stays hot and only its conversation is left
  unscanned for the next pass, which starts after one minute instead of the ten-minute idle wait;
  database errors still stop the pass.
- **Long, lossy paths.** Beta-SG uploads to a Beijing bucket over a cross-border path with about
  16% retransmission: one CUBIC stream reached 13 KB/s, one BBR stream 148 KB/s and eight BBR streams
  932 KB/s (2026-11-14). Set `UPLOAD_CONCURRENCY` and `TCP_CONGESTION=bbr` there; unprivileged
  processes may only select an algorithm listed in `net.ipv4.tcp_allowed_congestion_control`, so add
  `bbr` to that list (and load `tcp_bbr` at boot) without changing the host default.
- **Cost.** Uploads are free on Aliyun OSS. Reading from outside the provider's network is billed as
  outbound traffic, plus the infrequent-access retrieval fee when used. Reads happen only when a
  frozen body is opened.
- **Unthawing.** There is no automatic thaw. Opening, resuming or forking reads bodies through the
  cache; new history stays hot.

## Tests

- `coldstore` unit tests: classification, placeholder shape, integrity, cache tampering, download
  coalescing and SigV4 path encoding. `TestLiveObjectStore` runs against a real bucket when
  `MIRA_COLD_LIVE_*` variables are set.
- `TestClaudeColdHistoryRoundTrip` and `TestClaudeColdHistoryRoutes` (PostgreSQL): only idle tool
  results move, running sessions and assistant records stay hot, event pages, root view, native
  transcript stream and cache prefix responses are byte-identical, one body is shared by event and
  entry, explicit failure without a bucket, collection after session deletion.
- `TestColdHistoryRoundTrip` (PostgreSQL): freezes a conversation with screenshots, duplicates,
  compaction and metadata records, then requires byte-identical ThreadStore history and stream, event
  listing, both transcript readers with tool details, transcript images, the thread list and rebuilt
  index records; verifies explicit failure without a bucket and grace-period collection after
  permanent deletion.

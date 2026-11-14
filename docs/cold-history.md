# Cold history: idle conversation bodies in object storage

Implemented by schema 49 (Mira 1.0.114). Disabled unless a bucket is configured.

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
count changed since their last scan, and per row:

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

The client speaks AWS Signature Version 4 and works with Aliyun OSS, MinIO, Tencent COS and other
S3-compatible stores. Give the Server a credential limited to its prefix: Get/Put/Delete object,
abort multipart upload and list parts. It does not need to delete object versions.

## Operations

- **Rollback.** Once rows are frozen, a Server older than 1.0.114 returns placeholders as history.
  Do not roll back below 1.0.114 after enabling `FREEZE`; keep the bucket configuration on newer
  versions. Disabling `FREEZE` stops new freezing; reads still need the bucket.
- **Backups.** `pg_dump` no longer contains frozen bodies. The bucket is part of the backup: keep
  versioning on and do not expire current object versions.
- **Disk space.** Freezing frees space inside PostgreSQL's TOAST storage for reuse, but the data
  files do not shrink. Use `pg_repack` (or `VACUUM FULL` in a maintenance window) to return space to
  the file system.
- **Cost.** Uploads are free on Aliyun OSS. Reading from outside the provider's network is billed as
  outbound traffic, plus the infrequent-access retrieval fee when used. Reads happen only when a
  frozen body is opened.
- **Unthawing.** There is no automatic thaw. Opening, resuming or forking reads bodies through the
  cache; new history stays hot.

## Tests

- `coldstore` unit tests: classification, placeholder shape, integrity, cache tampering, download
  coalescing and SigV4 path encoding. `TestLiveObjectStore` runs against a real bucket when
  `MIRA_COLD_LIVE_*` variables are set.
- `TestColdHistoryRoundTrip` (PostgreSQL): freezes a conversation with screenshots, duplicates,
  compaction and metadata records, then requires byte-identical ThreadStore history and stream, event
  listing, both transcript readers with tool details, transcript images, the thread list and rebuilt
  index records; verifies explicit failure without a bucket and grace-period collection after
  permanent deletion.

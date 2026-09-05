# Thread storage: raw records and current state

Implemented by schema 17 and the Mira remote adapter for Codex 0.153.1.
PostgreSQL remains the sole durable conversation store. Node-local projections are
in memory; Desktop import source files are read-only.

## What changed

The former `codex_store_events` stored the complete global metadata and history
manifest after every write. Raw rollout items were already separate rows, but
metadata copies and rebuilding every thread projection made storage grow with
both the number of commits and the size of the store. Permanent deletion had to
rewrite all those copies.

Normal appends now insert only new raw items, add a small commit receipt and
history boundary, and update the affected thread's counters. Metadata changes
write only changed state entries and an ordered set/remove log. Unrelated thread
projections are untouched. A raw append does not rewrite the thread's large
metadata JSON either.

The Codex adapter sends the items supplied by `append_items` directly. It reads
only that thread's current metadata and generation/count, without downloading or
diffing prior history. Transient token deltas retain upstream persistence filtering.
Resume/fork and explicit history reads still load the history those operations
need. Metadata operations use scoped in-memory compatibility projections.

## Tables

| Table | Durable contents and purpose |
| --- | --- |
| `codex_thread_events` | One canonical raw JSON rollout item per `(store_id, thread_id, generation, item_seq)`. An operation UUID links it to its commit. Unknown fields and existing raw text remain intact. |
| `codex_store_events` | Small operation receipts: UUID, request digest, publication version, original result version, item count and format/runtime metadata. No full state, manifest or request body. |
| `codex_store_state_changes` | Ordered, thread-scoped metadata changes, including a one-time migration baseline. Values use JSON. Root values/unknown fields are preserved. |
| `codex_store_state_entries` | Rebuildable current metadata, split by top-level field and map entry. A root marker distinguishes missing, scalar and object values. Reverse rollout-path mappings carry explicit thread ownership. |
| `codex_thread_revisions` | Sparse generation/count/active boundaries, recorded only when a thread's history boundary changes. No message bodies or state snapshots. |
| `codex_thread_projections` | Rebuildable current generation/count and list metadata such as title, cwd, source and parent. |
| `codex_store_heads` | Current publication version and earliest retained version (`history_floor`). |
| `mira_thread_actions` | Archive/restore operations and permanent deletion fences. |
| `mira_thread_erasures` | Durable progress/retry state for bounded permanent deletion. |
| `mira_codex_session_imports`, `mira_codex_session_import_records`, `mira_codex_session_import_segments` | Original import provenance and shared Desktop fork lineage. Preserved independently of normalized runtime history. |

`codex_thread_events_versioned` is a join view, not another copy of history.
`codex_thread_store_snapshots` and `codex_store_write_locks` no longer exist.
Other control-plane, authentication, Node and audit tables are unchanged.

Raw rollout/provenance use PostgreSQL `json`, not `jsonb`: real tool output can
contain escaped NUL or surrogate escapes rejected by JSONB. Migration does not
parse/reserialize existing payloads. Indexed current metadata remains JSONB,
as before. The application preserves all upstream fields it receives; it cannot
recover source formatting already lost before import.

## Transaction and concurrency rules

1. Claim the operation UUID. An identical retry returns its original result; a
   different body with that UUID returns conflict. Transient retries reuse the
   exact UUID/body and never repeat tool side effects.
2. Acquire the shared store gate and affected thread locks in sorted order.
   Legacy whole-state operations acquire the store gate exclusively.
3. Validate deletion fences, expected generation/count and metadata CAS. Prepare
   the new raw rows, changed metadata entries, delta log and history boundaries.
4. At the end, briefly lock/increment the store head and assign the publication
   version to the receipt. Commit the transaction.

History rows reference UUIDs, so bulk writes happen before the shared publication
point. Independent threads can prepare concurrently. The numeric version remains
useful for consistent history reads and optimistic concurrency; it no longer
means a saved full snapshot. A PostgreSQL sequence alone would not be sufficient:
allocation order can differ from commit order and expose an incomplete prefix.

Identical no-change operations retain a retry receipt without advancing the head.
Logical history replacement/recreation advances generation. Reads never mix
separate generations. Explicit permanent deletion makes all versions inaccessible.
Writer leases/fencing across network partitions remain a known architectural gap.

## API compatibility and rebuild

- `GET /v2/stores/{storeId}?threadId=...` returns current metadata and manifest for
  that thread, plus root values. Omitting the query retains the whole-store adapter.
- `X-Mira-Thread-Scope` on a commit requests a compact manifest response and is
  validated against all changes. Older clients can keep using the existing v2 body.
- v1 snapshots are assembled on demand in a consistent read transaction; writes
  convert them into deltas. No durable snapshot cache is maintained.
- `/events` exposes lightweight receipts and reconstructs manifests when explicitly
  requested through the compatibility API.
- Projection rebuild replays metadata changes and sparse history boundaries.
  Permanent tombstones prevent deleted threads from returning during rebuild.

## Permanent deletion and import

The foreground transaction publishes a tombstone, removes current list/state/runtime
bindings, and queues erasure. It does not traverse the global event history.
A worker deletes only the target thread's metadata changes and raw history in
bounded transactions, with durable progress and crash retry. Content-free commit
receipts and boundary records can remain for ordering/idempotency.
The Web UI removes the conversation and confirms deletion as soon as the foreground
transaction succeeds. Erasure progress is an internal maintenance concern; it does
not keep the UI busy or trigger cleanup polling.

Import provenance is removed only when no surviving imported fork or other store
still depends on it. Import discovery and transfer never delete the original
Desktop JSONL files. Import staging remains transactional and canonical publication
remains atomic; cancellation cannot publish a partial thread.

## One-time cutover

This migration deliberately uses the administrator-approved idle maintenance
window to discard **old complete metadata/manifest snapshots and old receipts**.
It retains all canonical raw items (including older generations), the current
metadata, current manifests, import provenance and control-plane data.

Each existing store receives one baseline at its previous head. Reads before
`history_floor` return 410; stale writes before that floor return a reload conflict.
Old in-memory Codex processes must restart/reload. Snapshot-only legacy stores or
missing canonical heads cause migration to fail transactionally rather than lose
unconverted history.

The rollout procedure takes a private database backup, stops managed writers,
applies the ordered checksum-verified migration, verifies raw/provenance hashes and
counts, and replaces only the Server container plus the explicit Codex runtime
packages. Restoring an old Server binary alone cannot read schema 17: rollback
requires the pre-cutover database backup and discards post-cutover writes.

The rollback backup is outside the live database. It is not an ongoing version
store and should be handled under the administrator's normal backup retention.

## Acceptance coverage

- `tests/storage_rows_e2e.mjs`: schema 15 to 17 migration; unchanged raw JSON text,
  hashes and timestamps; full projection rebuild; removed snapshot table; retired
  version reads; identical concurrent retries; unrelated projection preservation;
  reversed preparation/publication timing; generation recreation and v1 adapter.
- `tests/thread_management_e2e.mjs`: rename, CAS/retry, archive/restore, permanent
  deletion fences, shared fork provenance, erasure failure/retry and concurrent writes.
- `tests/session_transfer_e2e.mjs`: raw/unknown fields, subagent metadata, imported
  fork lineage, large streamed import and compatibility reads.
- `tests/fork_titles_e2e.mjs`: inherited fork names, numbered collisions (including archived
  threads), concurrent allocation, idempotent retries and preservation of manual edits.
- `tests/thread_fork_e2e.mjs`: real Codex App Server fork, inherited name and fresh-process resume.
- `tests/thread_cli_e2e.mjs`: CLI creation and resume with a fresh `CODEX_HOME`,
  reconstructing the previous model context entirely from PostgreSQL.
- `tests/runtime_reliability_e2e.mjs`: streaming, lost replies, UUID-stable retry,
  cancellation, permanent errors and preservation of imported unknown fields.
- Codex `remote_http` tests verify direct append without downloading 10,000 prior
  records, scoped requests, ordered cancellation-safe writes and retry behavior.

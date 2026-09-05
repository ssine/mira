# Runtime persistence and build reliability

PostgreSQL is still the only durable conversation store. These changes add no
local JSONL mirror, disk outbox, or alternate source of truth.

## Persistence acknowledgement

- Every delta commit gets one operation UUID and one serialized request body.
  Connection failures, response-body transport failures, HTTP 408/429 and 5xx
  retry that exact request. A server restart and an acknowledgement lost after
  commit are both safe: PostgreSQL's operation-ID uniqueness returns the existing
  commit without appending the same items twice.
- Each attempt has a timeout; exponential backoff caps at 8 seconds. Numeric
  `Retry-After` is respected up to 60 seconds. Transient failures keep retrying
  while the runtime is alive, with diagnostic warnings. This is storage retry,
  **not** a retry of a model request or a tool execution.
- Started writes are serialized independently of the requesting task's lifetime.
  Cancelling a turn stops its work but does not discard an already-started write.
  A flush waits for earlier queued writes, including cancelled callers, and only
  succeeds after acknowledgement. It does not download the whole store again.
- Authorization/validation failures, incompatible responses and actual optimistic
  conflicts are not blindly retried. A permanent persistence failure latches a
  runtime storage error. Later writes and durability barriers fail rather than
  pretending the transcript was saved. Restore access, then restart the affected
  runtime; unknown-outcome commits must be inspected before resubmitting work.
- A durability barrier precedes model sampling, so a rejected tool result cannot
  silently become the next model request. Task panics are caught at the existing
  task lifecycle boundary and become a scoped `error` followed by a failed
  `turn/completed`, rather than leaving the UI permanently in progress. Panic
  payloads and HTTP response bodies are not exposed as user-facing errors.

This does not promise zero loss after forcibly killing a process before database
acknowledgement: pending data is only in memory. When PostgreSQL is inaccessible,
an error cannot itself be durably written there. The live error/terminal event and
runtime logs are the available diagnostics. Do not fabricate a previously lost
tool output or automatically repeat its side effects to repair old history.

All Session tasks use the same terminal path, including subagents. Thread-store
tests also cover queued parent/child creation and preservation of parent identity.
They do not claim end-to-end recovery of every subagent orchestration pattern.

## Verification

The upstream patch includes ThreadStore tests for 502/429, lost acknowledgements,
exact request reuse, cancellation and ordering, permanent failures and worker
panics, plus a core lifecycle test for panic/error/completion/idle state.

For the real runtime + disposable PostgreSQL regression (no model account needed):

```sh
CODEX_TEST_BINARY=/absolute/canonical-package/bin/codex \
  node tests/runtime_reliability_e2e.mjs
```

This creates and removes its own database on local PostgreSQL (default port
55432; override `MIRA_TEST_DATABASE_URL`). It uses loopback-only model/store
fixtures, checks exactly-once tool output after a 502 and lost acknowledgement,
V1/V2 history consistency, fresh-process resume, permanent-error termination, and
malformed-history termination in both debug and release builds. It never uses a
production identity or runs the historic failing command.

## CI cache policy

GitHub caches are immutable and ref-scoped. A new release tag cannot read a
sibling tag's caches; it can read default-branch caches. Runtime input changes on
trusted `main` therefore warm the Codex build lane. UI-only changes still do not
build Codex or Nodes. A main build creates artifacts but does not publish a release.

| Layer | Cache identity / behavior |
| --- | --- |
| Rust compilation | sccache's compiler/command/source keys; GHA backend enabled before daemon startup; no idle shutdown during long links |
| Cargo downloads | OS + Cargo.lock; compatible download-only fallback, Cargo still verifies/resolves inputs |
| rusty_v8 | target + profile + pinned V8 version; upstream checksums re-fetched and verified on every restore |
| Linux builder | per-architecture Docker build-layer cache |
| Linux native C | source archives + build script/flags + object rules + actual compiler + installed Alpine package versions + architecture |
| Linux container Go | separate GOCACHE per native toolchain key; Go content keys decide reuse |
| Windows native C | compiler/helper/linker/CMake bytes + CRT/SDK version/library digests + sources + adaptation/build scripts/dispatcher/workflow |

Native archives never replace fresh Go compilation/final linking. The final image
must still have the current version/commit/build time, complete license notices,
and pass package verification. Native input cache misses rebuild normally. Linux
native archive checksums are verified before extraction; a bad cache rebuilds.
The workflow cache namespace uses a revision suffix so expanded Go caches can be
saved without trying to overwrite an immutable entry. Cargo timings and sccache
statistics are uploaded even after failures; native builds print hit/miss and
elapsed time. Cold cache, linker work and hosted-runner variation still cost time.

Cache warmup is not a release promotion policy. Codex patch revisions remain
immutable, and deployment must verify the runtime lock and the complete canonical
package before switching an idle node. Never interrupt active user turns to update.

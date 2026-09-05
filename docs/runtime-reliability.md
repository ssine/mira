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

### Streaming and browser latency

Transient rollout items must be filtered before entering the remote writer. The
adapter checks upstream's persistence policy in both Legacy and Paginated modes;
only an append that is nonpersistent in both modes takes the fast path. A mixed
batch or any potentially durable item still uses the ordered, acknowledged
writer. The fast path also checks the latched storage error.

Previously, even a single text delta entered `mutate`, fetched the store head,
loaded history when necessary, and compared before/after snapshots before
discovering that nothing needed writing. This serialized visible text behind a
remote database round trip per delta. Browser rendering alone could not fix it.

The runtime regression below now streams 500 ready text deltas with 30 ms store
latency. It asserts zero store requests between the first and last delta, exact
text, durable completion and a fresh-process resume. The old runtime makes 499
requests between those deltas and fails this test.

For an opt-in measurement with the real configured model on a local approved
execution Node, use the installed CLI for authentication:

```sh
MIRA_CLI_PATH=/absolute/path/to/mira \
MIRA_STREAM_NODE_ID=<approved-node-id> \
MIRA_STREAM_DIRECT_URL=ws://127.0.0.1:<app-server-port> \
MIRA_STREAM_OUTPUT=/tmp/mira-stream-capture.json \
  node tests/app_server_stream_probe.mjs /absolute/path/to/playwright/index.mjs
```

This creates and archives a diagnostic conversation. It records direct App
Server notifications, the same notifications after Mira's real proxy, browser
receive times, frame observations and long tasks. Frame timestamps are a
conservative next-frame observation, not a physical-display scanout timestamp.
Captures include the diagnostic conversation payloads: do not commit them or use
production credential files as test fixtures. Set `MIRA_BROWSER_EXECUTABLE` if
Playwright's default browser is unavailable.

The real payloads can then exercise the renderer without contacting a model:

```sh
MIRA_STREAM_CAPTURE=/tmp/mira-stream-capture.json \
  node tests/trace_activity_browser.mjs /absolute/path/to/playwright/index.mjs
```

Replay uses 20 times the captured rate, a mobile viewport, 6 times CPU throttling
and long conversation history. It checks lossless output and receive-to-frame
latency, independently of model generation speed. Live text enters the DOM in
the message task; scroll updates and secondary metadata updates share a frame.

A 2026-09-05 sample with the same `gpt-6-astra`, `xhigh`, `priority` configuration
and a 30-line Chinese prompt measured the following. These are sample results,
not a guaranteed model rate:

| Runtime | Characters | First-to-last delta | Characters/sec | Browser receive-to-frame p95 |
| --- | ---: | ---: | ---: | ---: |
| Before | 1,379 | 141.54 s | 9.74 | 32.6 ms |
| Fixed | 1,362 | 33.45 s | 40.72 | 15.4 ms |

All 1,254 / 1,231 deltas were observed in browser frames. The fixed runtime made
zero store HTTP requests between the first and last text delta. Its isolated
candidate used a loopback browser bridge while still writing to the same real
PostgreSQL store; the baseline separately measured Mira's actual relay overhead
at 3.6 ms p95. The later idle-deployment probe repeats the complete managed
runtime/relay/browser path. At 20 times the captured rate with a throttled mobile
CPU and long history, browser replay measured 47.5 ms p95 and preserved all text.

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
| Rust compilation | sccache's compiler/command/source keys in a bounded local cache; one GHA archive per target/attempt, with main snapshots reusable by release tags |
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

### Publish an existing verified runtime without rebuilding

After a trusted `main` Codex runtime run has passed **both platforms and packaging**,
dispatch `promote-codex-release.yml` on `main` with its numeric `source_run` ID.
This is separate from the Mira Node release promotion workflow. The current runtime
lock and patch must still match that source commit, which must be an ancestor of
the promotion checkout. Fork/PR/topic-branch, failed, partial and expired builds are
rejected. The GitHub artifact digest, release checksums, canonical manifests and
every archived file are checked without executing downloaded binaries.

Publication uses the workflow's `GITHUB_TOKEN`, so creating the runtime tag does
not trigger another full build. Assets first enter a draft and are downloaded back
for byte-for-byte verification before publication. A partial upload may resume only
the same source-run draft; no asset is overwritten, and an existing public release
is refused. Codex is always published with `--latest=false`. Nodes must still pass
platform-specific acceptance and an idle check before activation. This workflow
does not deploy nodes, rebuild clients or change any production configuration.

Compiler objects are not uploaded individually to GHA. The initial warmup observed
hundreds of failed cache writes and repeated minutes with about 200 created entries,
matching GitHub's [per-repository cache upload limit](https://docs.github.com/en/actions/reference/limits).
Instead, sccache stores up to 2 GiB locally per target and CI restores/saves that directory
as one immutable archive per run/attempt. Compatible fallback snapshots are safe because
sccache still validates compiler/source/command inputs; direct preprocessor shortcuts are
disabled. A new snapshot can preserve partial compiler work after failure without making
that build publishable. Statistics are captured before stopping the daemon and archiving.
The build step has an earlier deadline than the job so ordinary build timeouts leave room
for cache saving; abrupt runner loss can still lose unsaved work. Cache save/restore failures
do not bypass compilation or release checks. No account storage/billing limit is increased.
The first batched run uses a new cache namespace and must warm it; old per-object GHA
entries are not copied into the local cache. Existing entries are left to normal eviction.
`tests/compiler_cache_snapshot_e2e.mjs` verifies a cold native compile, an archived-cache
restore hit and invalidation after a header change (GCC on Linux, MSVC on Windows).

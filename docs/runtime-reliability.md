# Runtime persistence and build reliability

PostgreSQL is still the only durable conversation store. These changes add no
local JSONL mirror, disk outbox, or alternate source of truth.

## Portable context defaults

Codex runtime `0.155.1-mira.3` enables `features.mira_plaintext_context` by
default for all providers and child threads. New collaboration messages carry
readable text, and all compaction entry points use the existing ordinary
Responses summarizer. The resulting checkpoint contains ordinary messages and
a plaintext summary, with the original canonical history retained. Manual and
automatic compaction share this policy, including when token-budget mode is
enabled. The model, provider and authentication are not changed to select it.
The plaintext request uses the same retry classification as sampling: permanent
input errors stop immediately, while temporary rate limits retain cancellable
backoff and completed work.
This includes the gateway's complete `ratelimiter: tpm acquire project: tpm peek
tpm:project:…: context deadline exceeded` diagnostic when mislabeled HTTP 401;
ordinary authentication errors are not classified as temporary rate limits.

An explicit `features.mira_plaintext_context = false` retains the upstream
encrypted-compaction/token-budget selection for compatibility testing; the
legacy provider-specific plaintext message option still works. Old encrypted
history is not rewritten, and encrypted model reasoning remains an independent
mechanism. Such history can still require the existing scoped recovery flow
after an incompatible account switch.

## Model rate limits

Codex runtime `0.153.1-mira.14` keeps a turn alive after an HTTP or wrapped
WebSocket 429 rejects a model request. Sampling and remote compaction v2 use a
separate, unbounded retry count: wait 5, 10, 20, 40, then 60 seconds, plus up to
one second of jitter. A valid numeric or HTTP-date `Retry-After` takes precedence
(with a five-second minimum); longer server waits are never shortened to the
local backoff cap. Cancellation stops the wait. CLI, App Server and subagents
use the same runtime behavior without a provider configuration change.

Retries stay inside the current turn and preserve completed tool results.
They do not start another turn or replay completed tools. Existing retry
notifications expose the waiting state and delay to clients. Recognized usage,
insufficient-quota and billing failures remain terminal. An unrecognized 429
continues waiting until recovery or cancellation; a generic status alone cannot
identify every provider's billing errors. Other HTTP errors and streamed SSE
failures retain their existing retry policies, subject to permanent input errors
below.

## Encrypted input failures

Runtime `0.155.1-mira.2` treats structured `invalid_encrypted_content` and
`unknown_reasoning_pool` codes as permanent input failures, even when a gateway
labels them HTTP 429 or 5xx. HTTP retries stop immediately, and the API mapping
preserves the code in the terminal error instead of reporting generic server
load. The same rule applies to `response.failed` and nested/flat SSE `error`
events. Message text without a matching structured code does not change retry
behavior.

Mira can then offer its existing encrypted-input recovery action. Recovery still
requires explicit administrator consent and only changes the frozen-prefix
model-input projection; canonical records remain unchanged. It does not restart
the account process or automatically resubmit the failed turn.

Runtime `0.155.1-mira.4` additionally offers an opt-in conversation setting,
`mira_auto_reasoning_recovery`, passed through native thread start/cold-resume
configuration. Enabling it authorizes automatic exclusion of a specifically
identified encrypted reasoning item after a structured `invalid_encrypted_content`
failure. The exclusion is appended to native thread settings and flushed before
another model request. The retry stays in the same turn, retaining completed tool
results and healthy reasoning; normal requests continue to carry encrypted
reasoning. The saved switch and exclusions survive cold resume and account handoff.
Explicit `false` stops further automatic recovery but preserves prior exclusions.
This applies to sampling and plaintext compaction, with at most eight recoveries
per request and 128 retained active exclusions. Unattributable failures, encrypted
compaction checkpoints and exhausted limits still stop through the existing error
path. Canonical response items remain unchanged. See the
[Codex patch guide](../patches/codex/README.md) for the configuration contract.

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
- Started operations are ordered per affected thread independently of the requesting task's lifetime.
  Cancelling a turn stops its work but does not discard an already-started write.
  A flush waits for earlier queued writes, including cancelled callers, and only
  succeeds after acknowledgement. It does not download the whole store again.
  Revision `0.153.1-mira.15` permits up to eight independent scopes concurrently.
  A large history read or upload cannot monopolize the account's writer. Parent
  creation, child creation and graph writes retain their shared-thread ordering;
  a store-wide operation remains a barrier. Slow queue waits and operations emit
  diagnostic timings without message contents.
- Authorization/validation failures, incompatible responses and actual optimistic
  conflicts are not blindly retried. Authorization/protocol failures latch a
  runtime storage error; optimistic conflicts and thread request rejections
  (HTTP 404/410/413) fence writes and durability barriers for the affected thread
  only. Unrelated threads (including subagents) can continue, and canonical history
  remains readable after a thread conflict.
  Runtime revision `0.153.1-mira.15` also scopes a locally generated
  `ThreadNotFound` to its thread. A successful head response omitting a deleted
  thread must not disable reads, appends or thread creation across the account.
  Resolve the conflict, then restart the affected runtime; unknown-outcome commits
  must be inspected before resubmitting work. There is no automatic tool replay.
- Resuming an existing thread only reopens persistence for future appends. The
  caller's replay history cannot replace canonical records or recreate a thread
  deleted while the history request was in flight. Check existence against the
  current scoped metadata before applying resume metadata.
- Administrator deletion checks managed execution reservations on the Server,
  including `starting` before `turn/started` reaches a browser. The check shares
  the turn-start storage lock, and execution claims recheck deletion after taking
  that lock. A stopped or replaced runtime does not leave a stale reservation
  blocking deletion. Late writes from a deleted thread remain rejected.
- Keep the raw canonical state alongside the typed in-memory projection. Determine
  intended leaf changes from the typed before/after snapshots, but build CAS
  expectations from the original raw JSON. Missing optional fields are different
  from synthesized `null`; timestamps may deserialize into a different spelling.
  Update only changed canonical leaves after acknowledgement, retaining unknown
  fields and platform-filtered threads. This applies to native and imported history
  equally; imports do not need destructive normalization to the current runtime.
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

### Web reconciliation and error lifetime

Metadata-only thread reads retain their thread scope independently of whether
history is requested. Restoring a tree of subagents must not download the whole
store for every child. History reads retain generation/version validation.

The remote adapter retains up to eight scoped in-memory projections with a
2 GiB serialized-payload retention budget (since `0.153.1-mira.17`). Every reuse
validates the current canonical head. Unrelated global version changes and metadata-only changes retain
unchanged histories; acknowledged local appends extend a validated cached prefix.
A changed generation, deleted thread or externally changed history boundary
requires a fresh canonical read. Cache eviction never limits conversation size
and does not create a local durable history store. The budget describes serialized
payloads, not an exact bound on process heap usage.

Revision `0.153.1-mira.16` invalidates the selected thread's cache when its
session shuts down. Account input-recovery consent changes independently of the
canonical history version, so the next resume reloads the history response and
its current input policy. Other threads retain their caches, and canonical
encrypted records remain unchanged.

Web resume keeps one request per thread/socket until acknowledgement or
disconnect. Elapsed time changes the visible waiting message instead of dropping
the request and queuing another resume. Users can cancel the wait while retaining
their draft; this closes the browser channel, not the accepted runtime operation.
Connection probes do not queue behind an in-flight resume and mistake that delay
for a dead connection. An explicit offline event closes the stale channel.

When a browser disconnects, Node drains accepted account requests until their
replies arrive or the runtime connection closes. The existing account request
capacity bounds retained sockets. A ten-second deadline cannot safely discard
these acknowledgements: slow resumes can still be executing after that deadline.
An actual local transport failure retains unknown requests and continues to
block handoff; it does not infer success from an idle thread observation.

Active-turn reconciliation and stop checks read the Server activity projection
rather than Codex `thread/turns/list`, whose legacy implementation reconstructs
full history even with `itemsView: notLoaded`. The native interrupt still validates
the expected turn ID. `tests/active_turn_resume_browser.mjs` exercises warm
resume, repeated heartbeats, stop targeting and completion reconciliation without
runtime history queries; `tests/thread_interrupt_test.mjs` covers stale turns and
request deadlines.

Cold per-thread cost scans share two Server-wide reader slots across all clients.
Cached totals bypass the slots; cancelled queued requests never acquire a database
connection. This keeps statistics from filling the ten-connection pool needed by
conversation reads, writes and health checks. The cost-capacity PostgreSQL test
holds both scans open while a separate request still obtains a connection.
Conversation totals restore compatible account-cost checkpoints in bounded
descendant batches and scan only the remaining tail. This avoids replaying an
entire subagent tree after a restart or cache eviction. Every completed scan page
also retains its parser state and cursor in the bounded memory cache, so a later
request cancellation does not force that thread to restart from zero. Checkpoints
must match the generation, pricing/parser revision and fork/import source, and
cannot be ahead of the requested snapshot. Per-turn buckets use a separate cache
because the durable account checkpoint stores aggregate parser state only.

Messages arrive over the App Server WebSocket. The 25-second connection probe
does not download history. Initial selection, reconnection/foreground recovery,
older-page scrolling and completion reconciliation can fetch canonical history.
Completion bursts now coalesce into one tail read after 250 ms, instead of three
fixed reads. Unchanged canonical tail versions skip Markdown/DOM reconstruction.
Any live items not yet represented by that snapshot remain visible; future
reconnection/selection reconciles again without a perpetual history polling loop.

Errors are deduplicated per thread/turn and kept in page memory independently of
the canonical transcript until explicitly dismissed, logout or page close. They
survive canonical refresh, reconnection and switching away/back in the same page,
including failures received for a background thread. This is an operational
diagnostic, not a persistent history mirror: if saving failed, reloading the whole
page cannot recover that unsaved error. Closing a diagnostic never retries work.

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

The native Go store tests cover V1/V2 history consistency, idempotent operation IDs,
generation replacement and conflict handling. The Codex `remote_http` tests remain
the source of truth for exactly-once retries, cancellation and permanent-error behavior.

Fixture cleanup waits for PostgreSQL's backends to disconnect after `pool.end()`
and only then drops its uniquely named test database, without `FORCE`. A pool's
end promise can resolve before the underlying sockets have finished closing;
forcing a database drop in that gap can fail the process after all assertions
passed. Leaked connections time out and fail cleanup rather than being hidden.
`node --test tests/runtime_fixture_cleanup_test.mjs` covers this ordering and
failure handling without requiring a compiler or model.

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
not trigger another full build. The tag targets the promotion checkout on `main`,
whose Codex baseline/patch/lock are verified identical to the original build. The
release notes distinguish the original build SHA from the publication SHA; the
binary bytes and embedded build identity are never relabeled. This also avoids
GitHub's [additional workflow permission requirement for historical targets](https://docs.github.com/en/rest/releases/releases#create-a-release)
whose workflow files differ from the default branch. No broader token is installed.
Assets first enter a draft and are downloaded back
for byte-for-byte verification before publication. A partial upload may resume only
the same source-run/publication-commit draft; no asset is overwritten, and an existing public release
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
Completed canonical candidate packages are retained as diagnostic artifacts even
when subsequent acceptance fails. A retained candidate from a failed run is not a
release: packaging/promotion still require all platform jobs to pass. This avoids
losing the evidence needed to diagnose a post-compilation failure.
`tests/compiler_cache_snapshot_e2e.mjs` verifies a cold native compile, an archived-cache
restore hit and invalidation after a header change (GCC on Linux, MSVC on Windows).

## Managed Codex memory residency

The Node owns one `codexMemoryBudget` / `MIRA_NODE_CODEX_MEMORY_BUDGET` across all
managed accounts. `auto` uses 20% up to 4 GiB, then the greater of 0.8 GiB and 10%
of effective memory. Fixed byte capacities and percentages are supported. Linux
uses the minimum of physical memory and visible ancestor cgroup hard limits;
Windows uses physical memory. Accounting sums managed App Server RSS/working set,
including caches and process overhead, but not separately launched tools or CLI.

Every five seconds a bounded, cancellable controller samples memory and renews
short runtime leases through local `mira/thread/residency`. Polling reads only
loaded runtime metadata, never durable history. Monitor connections do not
subscribe to new threads. Below budget the normal idle timer is deferred. On
pressure the Node selects the oldest eligible candidate across accounts, schedules
at most one unload per 15 seconds, and resamples. Reclamation stops at 90% of the
budget. Activity, subscriptions and loaded family relationships are checked again
in the runtime; leaves unload before parents. Opaque candidate revisions reject
stale idle periods and process replacements. Explicit account handoff still uses
its scoped unload/flush acknowledgement and canonical history stays unchanged.

This is a soft retention target: running/observed conversations and allocator
retention can exceed it. When memory accounting fails, Node stops renewing leases;
after at most 60 seconds the runtime's configured idle timeout can unload eligible
threads again. Runtime methods unavailable in older packages are reported as
unsupported. Controller RPC failures are reported as unavailable, not healthy
memory control. The control loop is independent of the Server WebSocket and does
not block heartbeats. The setting is local to each execution Node and takes effect
on restart. Warm resume with identical injected developer instructions and the
same reasoning effort reuses the loaded session; changed or unknown overrides
retain upstream reload behavior.

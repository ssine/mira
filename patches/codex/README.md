# Codex patch

Mira currently needs a small patch on top of the official Codex source tree so that CLI and
App Server processes can use the remote PostgreSQL-backed ThreadStore adapter.

- Upstream: <https://github.com/openai/codex>
- Base tag: `rust-v0.153.1` (pinned in repository-root `CODEX_VERSION`)
- Base commit: `9856412`
- Patch source commit: `1119632bf948`

Apply it to a clean checkout:

```bash
git clone --branch rust-v0.153.1 https://github.com/openai/codex.git codex
git -C codex am ../patches/codex/0001-feat-thread-store-add-remote-PostgreSQL-adapter.patch
```

Build Codex using the upstream instructions. The resulting CLI and App Server understand the
`[experimental_thread_store]` configuration documented in the repository root README.

Mira's release workflow performs this application and uses Codex's canonical package builder for
Linux amd64 and Windows amd64. The resulting `mira-codex-package` includes the entrypoint,
`codex-code-mode-host`, platform sandbox resources, `rg` and `codex-package.json`; it is published
as an independent Codex runtime, downloaded by the Node on demand. The Node probes the remote ThreadStore configuration before advertising a build
as compatible. Updating `CODEX_VERSION` therefore requires rebasing this patch and passing both
release matrix builds, not just changing the version file.

The patch is intentionally kept separate from the Mira control plane. When updating Codex, rebase
or regenerate it against the new upstream tag, run the `codex-thread-store` tests, and verify the
App Server, CLI resume and subagent E2E scenarios before changing the supported baseline.

`bearer_token` is optional in the remote store table. Mira Node starts App Server with the current
device credential in `MIRA_NODE_TOKEN`, so the credential stays out of argv and central desired
state. A manually launched patched Codex must receive that environment value from a wrapper that
reads the protected Mira identity file, or use an explicit token only in isolated development.

Runtime revision `0.153.1-mira.8` adds chunked history uploads and fork progress.
Deploy a Server with schema 27 before using the new runtime for large writes.
The adapter sends 4 MiB byte chunks, seals staged JSON records, then publishes
history with the ordinary atomic commit receipt. App Server waits for the fork's
rollout flush before reporting success and emits ephemeral, request-correlated
`mira/thread/fork/progress` notifications. Existing small commits remain compatible.

Regression coverage includes `codex-thread-store` unit tests, Mira's PostgreSQL
history upload integration tests, `tests/large_fork_e2e.py` (a >64 MiB fork followed
by cold-runtime resume), and `tests/fork_progress_browser.py` (Camoufox progress,
request isolation and cancellation). Runtime packages still use the full canonical
package build; the local development executable used by tests is not a release.

Runtime revision `0.153.1-mira.9` adds account/runtime request identity and
administrator-confirmed encrypted-input recovery and remote AgentGraphStore,
requiring Server schema 29. The adapter keeps canonical history and persistence diffs intact;
only resumed model context applies the binding-scoped, frozen-prefix policy.
Reasoning remains unchanged unless the Server supplies an explicit consent record.
Legacy CLI/App Server resume and restored subagents use the same input projection.
The client never retries a failed model turn as part of this recovery.
Spawn edges and open/closed status share the remote history writer's cancellation,
retry and permanent-failure fencing. Server events rebuild the graph independently
of account-local SQLite, with thread generations checked on reads and writes.

Validation includes 258 thread-store tests, 367 core session tests, PostgreSQL
v1/v2 account writer/receipt tests and `tests/codex_accounts_runtime_e2e.mjs` with
real Node/App Server processes and a synthetic Responses provider. The latter
covers separate keys, live handoff, compatible input, invalid_encrypted_content,
consent replay, unchanged canonical history, forks and cold subagent account handoff.

Runtime revision `0.153.1-mira.10` projects the selected managed account's provider
when reading a thread for restore. Existing subagents keep their saved model and
canonical metadata/history, while direct followup uses the new account's provider
and credentials. Account protocol 2 requires Mira Server 1.0.20 or later; the
Server still reads protocol 1. The baseline and database schema remain unchanged.

Validation adds provider-projection tests (260 thread-store tests total), atomic
current-generation family handoff in PostgreSQL, and direct cold-child followup
across different provider IDs, URLs and credentials in the actual runtime E2E.

Runtime revision `0.153.1-mira.11` adds experimental `mira/thread/unload` for an
idle conversation tree. Mira holds a Node-side execution gate through the route
commit. The App Server validates the complete tree, waits for each loaded
session's shutdown, listener drainage and ThreadStore flush, and confirms the
exact scope without terminating the account process or changing canonical
history/graph state. History compatibility confirmation uses the same path.
Local GNU/Linux canonical packages can be assembled with
`MIRA_CODEX_LOCAL_NATIVE=1 node scripts/build-codex-release.mjs ...` for explicit
offline deployment; their canonical target remains GNU/Linux. Published CI
packages continue to use musl.

Runtime revision `0.153.1-mira.12` separates metadata read scope from history
loading. Restoring subagent metadata requests the selected thread's state instead
of repeatedly downloading the entire store; canonical metadata, generation and
history remain unchanged. Regression coverage restores 220 children and asserts
one scoped metadata request per child with no history downloads.

Runtime revision `0.153.1-mira.13` retains the same persistence patch and requires
release-profile packaging. Local runtime builds must compile **all** canonical
companions with `cargo build --release`; symbol stripping does not turn a debug
build into a release build. The packager rejects the pinned upstream CLI's
debug-only diagnostic, including stripped binaries and foreign-platform inputs.
This matters for interrupted custom tool calls: upstream release builds project
an `aborted` result for sampling, while debug builds deliberately panic. Never
repair this condition by deleting or inventing canonical tool history.

Runtime revision `0.153.1-mira.14` adds persistent, cancellable retry for model
HTTP/wrapped-WebSocket 429 responses in sampling and remote compaction v2.
The runtime respects `Retry-After`, uses capped exponential backoff with jitter,
and reports rate-limit waits through existing retry notifications. Explicit
usage/quota/billing failures remain terminal. This targeted model-request fix
does not replay completed tools or alter ThreadStore persistence. No provider
configuration changes are required; the new runtime must be installed and the
account App Server must load it before existing conversations benefit.

Runtime revision `0.153.1-mira.15` confines a locally generated `ThreadNotFound`
write failure to the affected thread. A deleted thread missing from a successful
scoped head response previously latched a global error and disabled unrelated
reads, appends and thread creation until restart. The deleted thread stays fenced;
other conversations and subagents keep working. Regression coverage reproduces
this response path and verifies existing writes, new roots and child creation.
It also replaces the account-wide operation queue with bounded independent
thread scopes, retaining parent/child and graph ordering and cancellation-safe
durability barriers. Scoped memory caches reuse histories across unrelated writes
and metadata changes, and extend acknowledged local appends. Generation changes
and deletion invalidate cached histories. Regression tests hold a history read or
parent write open while unrelated creation/flush succeeds, and verify cache
reuse and generation isolation. See `docs/runtime-reliability.md` for cache bounds.

Runtime revision `0.153.1-mira.16` also invalidates a thread's cache on session
shutdown. Confirmed account input-recovery policies can change without a
canonical version change; a subsequent resume must reload that policy while
other active threads keep their cached histories. Regression coverage includes
267 thread-store tests and the real account runtime recovery/handoff scenario.

Runtime revision `0.153.1-mira.17` raises scoped in-memory history retention to
2 GiB of serialized payload, allocated on demand. Canonical validation and the
eight-scope bound remain unchanged.

Runtime revision `0.153.1-mira.18` adds an opt-in compatibility adapter for
Responses providers that cannot replay encrypted inter-agent message payloads:

```toml
[model_providers.example]
plaintext_agent_messages = true
```

The three message-bearing v2 tools (`spawn_agent`, `send_message`, `followup_task`)
use the custom `mira_collaboration` namespace without encrypted parameters.
Providers without namespace tools receive `mira_`-prefixed function names.
The existing handlers retain spawning, delivery and scheduling semantics;
direct calls and Code Mode both deliver readable message content even when the
provider omits `encrypted_function_args`. Other tools retain their names.
The adapter preserves the actual tool names and invocation provenance in history.
Default provider behavior is unchanged. This does not recover ciphertext already
stored by an incompatible provider: use a new conversation with verified handoff
information and retain the original history for reference.

Core integration coverage exercises plaintext delivery, queueing, follow-up,
history replay and Code Mode alongside upstream encrypted-message regressions.
Run `MIRA_TEST_PLAINTEXT_AGENT_MESSAGES=1 node tests/codex_accounts_runtime_e2e.mjs`
against a disposable local Server and the new runtime to cover canonical
PostgreSQL history and cold subagent resume during account handoff.

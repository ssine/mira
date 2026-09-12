# Codex patch

Mira currently needs a small patch on top of the official Codex source tree so that CLI and
App Server processes can use the remote PostgreSQL-backed ThreadStore adapter.

- Upstream: <https://github.com/openai/codex>
- Base tag: `rust-v0.153.1` (pinned in repository-root `CODEX_VERSION`)
- Base commit: `9856412`
- Patch source commit: `3571f5c`

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

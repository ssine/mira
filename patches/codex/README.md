# Codex patch

Mira currently needs a small patch on top of the official Codex source tree so that CLI and
App Server processes can use the remote PostgreSQL-backed ThreadStore adapter.

- Upstream: <https://github.com/openai/codex>
- Base tag: `rust-v0.153.1` (pinned in repository-root `CODEX_VERSION`)
- Base commit: `9856412`
- Patch source commit: `a84054b`

Apply it to a clean checkout:

```bash
git clone --branch rust-v0.153.1 https://github.com/openai/codex.git codex
git -C codex am ../patches/codex/0001-feat-thread-store-add-remote-PostgreSQL-adapter.patch
```

Build Codex using the upstream instructions. The resulting CLI and App Server understand the
`[experimental_thread_store]` configuration documented in the repository root README.

Mira's release workflow performs this application and uses Codex's canonical package builder for
Linux amd64 and Windows amd64. The resulting `mira-codex-package` includes the entrypoint,
`codex-code-mode-host`, platform sandbox resources, `rg` and `codex-package.json`; it is packaged
beside `mira-node`. The Node probes the remote ThreadStore configuration before advertising a build
as compatible. Updating `CODEX_VERSION` therefore requires rebasing this patch and passing both
release matrix builds, not just changing the version file.

The patch is intentionally kept separate from the Mira control plane. When updating Codex, rebase
or regenerate it against the new upstream tag, run the `codex-thread-store` tests, and verify the
App Server, CLI resume and subagent E2E scenarios before changing the supported baseline.

`bearer_token` is optional in the remote store table. Mira Node starts App Server with the current
device credential in `MIRA_NODE_TOKEN`, so the credential stays out of argv and central desired
state. A manually launched patched Codex must receive that environment value from a wrapper that
reads the protected Mira identity file, or use an explicit token only in isolated development.

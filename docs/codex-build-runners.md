# Codex runtime build runners

`.github/workflows/codex-release.yml` compiles the Mira-compatible Codex runtime on two
self-hosted GitHub Actions runners on the owner's Desktop instead of GitHub-hosted
machines. The packaging/publication job and promotion remain on GitHub-hosted runners.

| Runner | Label | Host | OS identity |
| --- | --- | --- | --- |
| `desktop-wsl-codex` | `mira-codex-linux` | Desktop WSL (Ubuntu) | dedicated `mira-ci` user, systemd service |
| `desktop-windows-codex` | `mira-codex-windows` | Desktop Windows | `NT AUTHORITY\NETWORK SERVICE` service |

## Who can trigger a build

The build job runs only when `github.repository == 'ssine/mira'` and both `github.actor`
and `github.triggering_actor` are `ssine`. Triggers are pushes to `main` that touch the
runtime lock/patch/tests, `codex-v*` tags and manual dispatch. There is no `pull_request`
trigger. The repository also requires approval before workflows from any fork pull
request run, because a pull request can propose its own workflow file. Never add
`pull_request`/`pull_request_target` triggers or a self-hosted label to a workflow that
runs untrusted code.

Isolation is defense in depth, not a sandbox: `mira-ci` cannot read `/home/sine`, and
may run only `sudo apt-get` (needed by the upstream MUSL tool script; this is still
root-equivalent). WSL interop and `/mnt/<drive>` remain reachable from any WSL user, so
a job could start Windows programs as the logged-in Windows user. Only trusted commits
must reach these runners.

## Caches

Each runner keeps, under its tool cache (`_work/_tool/mira-codex/`), a persistent
upstream checkout reset to `rust-v<CODEX_VERSION>` and re-patched every run, and one
Cargo target directory per target and Rust toolchain. Cargo fingerprints invalidate by
toolchain, source and flags; final binaries are deleted before every build so each
package is freshly linked and then verified. A target directory above 80 GiB is
discarded. Verified rusty_v8 archives are cached by version and re-checked against
upstream digests every run. Deleting these directories is always safe.

## Host prerequisites

- WSL: `python-is-python3` (Python 3.11+), `musl-tools`, `libcap-dev`, `pkg-config`,
  `clang`, `lld`, `libc++-dev`, `libc++abi-dev`, `g++`, `make`, `perl`, `xz-utils`, Git.
  Runner at `/opt/mira-ci/runner`; service `actions.runner.ssine-mira.desktop-wsl-codex`.
- Windows: Visual Studio 2022 Build Tools (C++), a Windows SDK, Git for Windows, Node.js,
  Python 3.13 at `C:\Program Files\Python313` (not on the global PATH), long paths
  enabled. Runner installation at `D:\actions-runner\mira-codex`; its work folder is the
  plain NTFS folder `F:\mira-ci\work` (service start type Automatic, delayed). Defender
  excludes `F:\mira-ci` and `D:\actions-runner`. The
  first Windows step adds Git Bash and Python through `GITHUB_PATH` (the runner resolves
  `bash` there before `System32`, whose `bash.exe` would start WSL) and puts
  `CARGO_HOME`, `RUSTUP_HOME`, `TEMP` and `TMP` under the work folder; the runner's
  `.path`/`.env` files are not used.
- Rust 1.95.0, Node.js 22 and Zig are installed by the workflow actions.
- Cargo fetches git dependencies (Codex's rusty_v8 fork has deep Chromium submodules)
  through the Git CLI with `core.longpaths` scoped to the job by `GIT_CONFIG_*`
  variables, as upstream CI does; the runner account's Git configuration is unchanged.

The Windows job lowers its worker to below-normal CPU priority and low memory
priority; cargo, rustc and link inherit the CPU priority class, so Windows trims build
memory before interactive programs. The system-wide Segment Heap
(`HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Segment Heap`, `Enabled=1`)
is enabled on the Desktop, as in upstream Codex CI: with the default NT heap the final
LTO step of `codex.exe` ran at about one core for roughly 40 minutes; with the Segment
Heap it used five to eight cores and finished in about 16 minutes. Deleting the value
restores the default heap for newly started processes.

Builds queue while the Desktop is off or asleep. Both runners share one physical CPU,
so the Linux and Windows builds run concurrently but each is slower than alone.

## Re-registering a runner

Create a token with `gh api -X POST repos/ssine/mira/actions/runners/registration-token`
and run `config.sh`/`config.cmd --unattended --replace` with the same name and label.
Do not print or store the token.

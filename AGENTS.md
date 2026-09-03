# Mira repository guide

This file records the project goals and architecture decisions that should remain stable while Mira
evolves. It is both design documentation and working guidance for coding agents in this repository.

## Product goal

Mira turns a user's Windows desktop, WSL distributions, Linux home servers, Linux NAS devices and
Android phones into one trusted personal execution fabric for Codex.

The desired user experience is:

1. Mira Node runs natively on every device and makes an outbound connection to Mira Server.
2. A user can submit work from a local CLI, Codex Desktop, a future Web/mobile UI or an external
   integration such as Feishu.
3. The user or scheduler chooses the node on which a Codex Agent and its App Server run.
4. That Codex Agent can use centrally injected `dynamicTools` to inspect or operate any authorized
   online node.
5. The same thread can be opened from different clients and resumed on a different execution node.
6. PostgreSQL on the user's Home Server remains the only durable source of truth for conversations.

Android participates as a device node for files, processes, screen capture and input. It does not
need to run Codex itself.

## Terminology

- **Mira Server**: the central control plane, App Server broker and persistence API.
- **Mira Node**: infrastructure software running on a physical or logical device.
- **node**: one registered Mira Node instance, such as Windows, WSL, Linux, NAS or Android.
- **Codex Agent**: the AI execution associated with a Codex thread.
- **Codex subagent**: a child Codex Agent/thread created by the multi-agent system.

Do not call Mira Node an "agent" in user-facing text or new identifiers. This avoids confusing the
infrastructure process with Codex Agent. Existing official Codex names such as `multi_agent_v2`,
`subagent` and upstream crate names must not be rewritten.

The source component directory is singular `node/` because it contains one software implementation.
Collections remain plural: `/v1/nodes`, database resources and variables holding node lists.

## Architectural boundaries

```text
Clients
  -> Mira Server
       -> PostgreSQL authoritative thread store
       -> node registry and desired/reported state
       -> App Server WebSocket proxy and dynamicTools broker
       -> outbound-connected Mira Nodes
            -> local file/process/PTY/screen capabilities
            -> optional local Codex App Server
```

Mira Server does not mount devices or initiate network connections into them. Mira Nodes maintain
outbound control connections. SSH v1 is an additional end-to-end SSH byte transport over dedicated
outbound WSS streams, not a replacement for the JSON control/App Server protocols. The Server
coordinates connections and relays encrypted bytes; it does not log in through system sshd.
See `protocol/ssh-v1.md` for identity, wire protocol, bounds and current feature limits.

SSH architecture decisions: built-in Mira clients; one independently supervised worker per SSH
connection, launched from the same Node binary; maintained SSH/SFTP libraries; no public port 22
or external sshd requirement. Reuse the approved Node identity and derive purpose-separated local
host/client keys; the database contains public keys only. Verify both host and caller keys. Revocation
must close active connections and reap children. Worker process isolation is not a privilege sandbox.
Bulk binary transfers must bypass JSON text buffers and apply bounded backpressure. Shell executes
as the Node OS user, not an arbitrary SSH username. Do not claim full OpenSSH extension compatibility,
automatic session recovery or Android PTY acceptance based only on cross-compilation.

Codex remains a native process on the selected execution node. Mira does not reimplement Codex or
turn model execution into a central monolith. The central service coordinates nodes and persists
threads while execution stays close to the selected workspace.

## Persistence decisions

PostgreSQL is the sole durable source of truth for Codex thread state and history.

- A node-local `CODEX_HOME`, JSONL file or UI cache must not become an authoritative history store.
- Store canonical raw rollout/thread items without discarding unknown fields. This preserves forward
  compatibility with newer Codex versions.
- Authoritative events and immutable history items are append-oriented. Derived views, indexes and
  snapshots are projections and must be rebuildable from authoritative data.
- A whole-thread JSON representation can be exposed as a compatibility snapshot, but routine writes
  use fine-grained state and history operations to avoid full-document contention and lost updates.
- Commits use expected store versions, operation UUIDs and idempotency. Non-conflicting commits may
  rebase; conflicting changes must return a conflict rather than silently overwrite data.
- Replacing or recreating a thread advances its generation. Reads must not combine items from
  different generations.
- Parent thread ID, source kind and subagent identity are durable metadata, not UI-only annotations.

The v1 snapshot API is a compatibility adapter. The v2 event/delta API is the preferred persistence
contract. See `protocol/thread-store-v2.md`.

## Protocol and upgrade policy

Mira should follow official Codex ThreadStore and App Server semantics instead of inventing a
parallel conversation model.

- Preserve raw upstream payloads and record event format and Codex version metadata.
- Keep adapters at the boundary. Internal projections may evolve independently and be rebuilt.
- Old authoritative events must remain replayable after upgrades.
- Add explicit protocol versions and backward-compatible readers before changing emitted formats.
- Database migrations are ordered, transactional and checksum-verified.
- Never edit SQL text in an already released migration; append a new migration instead.
- Test a supported Codex baseline through CLI, App Server resume and subagent scenarios before
  changing the baseline in `patches/codex/`.
- The patch should stay narrowly focused on replacing the ThreadStore persistence boundary. Avoid
  broad forks of unrelated Codex behavior.

## Subagent model

A Codex subagent is not an in-memory detail of its parent. Persist it as its own thread with a parent
thread ID and source kind. Parent and children share the same PostgreSQL store and can receive the
same dynamic tools, but their histories and generations remain independent.

Do not flatten a subagent tree into one JSON document. Cross-node resume must reconstruct the same
tree from durable metadata even if parent and child ran on different nodes.

## Mira Node design

`node/` is one Go module and produces `mira-node` plus the `mira` control CLI for Windows, Linux and
WSL. The Android APK embeds `mira-node` but does not need the control CLI.
WSL uses the Linux build. Platform-specific behavior belongs behind build-tagged adapters under
`node/internal/node/`; registration, heartbeat, reverse WebSocket, file/process operations, output
cursors and lifecycle rules stay shared.

The Android APK under `node/android/app/` is a platform shell around the same Go binary. Java owns
Android Framework responsibilities: Activity, foreground service, Accessibility, MediaProjection,
permissions, boot recovery and child-process lifecycle. Go owns the Mira protocol and common data
plane. Do not introduce a separate Termux or ADB-specific Mira Node implementation.

Desktop/server nodes may discover and run Codex App Server. Android reports App Server as
unsupported. Desired state comes from Mira Server; reported state describes what is actually
running. Secrets such as server tokens stay local and must not be included in desired state.
Nodes also discover rollout JSONL files under their configured, environment and default Codex homes.
Discovery is read-only. Import is an explicit administrator action: preserve every original record in
append-only provenance storage, then adapt it into the versioned ThreadStore without silently replacing
divergent PostgreSQL history.

amd64 Linux and Windows release packages include `mira-codex` plus `codex-code-mode-host`, built from
the official tag pinned by `CODEX_VERSION` and the narrow patch under `patches/codex/`. Keep ordinary
official Codex installations usable, but never select one for remote ThreadStore execution unless the
Node's runtime probe confirms support. `mira codex` intentionally injects the current Server endpoint,
store ID and Node credential so CLI and web/App Server sessions share PostgreSQL.

Node capabilities must retain these safety properties:

- file operations are confined to configured absolute roots; by default these are `/` on Unix-like systems and all visible drive roots on Windows;
- realpath/symlink resolution cannot escape those roots;
- allowed roots themselves cannot be moved or removed;
- reads and writes are bounded (currently 4 MiB per operation);
- process and PTY output buffers are bounded and cursor-based;
- machine telemetry is bounded and excludes environment values and full system process command lines;
- managed session counts are bounded;
- App Server listeners are loopback-only;
- shutdown attempts graceful process termination before force killing.

Use `MIRA_NODE_*` for new configuration names. Legacy `NODE_AGENT_*` and
`ANDROID_NATIVE_*` aliases are accepted only to make existing PoC deployments upgradeable.

## Security model

Mira v1 has exactly two security identities: one administrator and one credential per approved Node.

- Do not commit tokens, device identifiers, private addresses, production dumps or Codex auth data.
- Default listeners and development databases stay on loopback.
- A single human administrator authenticates with a password and a database-backed cookie session.
  Browser mutations require CSRF proof. Nodes and Codex clients never receive that password.
- Create/reset the administrator only with the local admin command. The running Server must not
  accept a default or environment-provided administrator password.
- A new Node generates a credential UUID and 256-bit secret before requesting enrollment. The
  Server stores only the secret hash; there is no activation secret or replacement token.
- `mira-node`, `mira`, local Codex and local App Server share one protected identity file and Node
  credential. Do not introduce CLI login or a global ThreadStore token.
- All approved Nodes are mutually trusted in v1. The Mira Node process' OS identity and resource
  limits remain the effective boundary. File roots cover the filesystems visible to that identity by
  default and may be explicitly narrowed per Node.
- Node credentials can register, heartbeat and open the reverse channel only as their bound Node.
  They cannot call administrator routes.
- Durable WebSocket credentials use an `auth.*` subprotocol and are never accepted in a URL query.
- Enrollment, approval, login, capability and revocation events are append-only audit records that
  exclude request content, command output, screenshots, environment values and credentials.
- `compose.homeserver.yaml` publishes Mira Server on host loopback only; Caddy is the sole TLS/WSS
  ingress. Proxy headers may be trusted only while the backend port cannot be reached directly.
- Choose the desktop/server Mira Node OS identity deliberately because its visible filesystems are
  exposed by default. `MIRA_NODE_ALLOWED_ROOTS` remains an optional defense-in-depth restriction.
  Never mount the Docker socket into a node container.
- Android root access always comes from explicit user authorization through an installed provider
  such as KernelSU, Magisk or APatch. The APK cannot grant itself root.
- Non-root Android must honor platform permission flows and cannot silently bypass private app data
  or MediaProjection confirmation.

## Repository layout

- `server/`: Node.js control plane, PostgreSQL migrations, persistence adapters and node broker.
- `server/public/`: same-origin administrator device console served by Mira Server.
- `node/cmd/mira-node/`: Go command entry point.
- `node/cmd/mira/`: Go control CLI using the current machine's Node identity.
- `node/internal/node/`: shared node runtime and platform adapters.
- `node/android/app/`: Android application shell.
- `protocol/`: versioned wire-protocol documentation.
- `tests/`: JavaScript/Python integration and end-to-end scenarios.
- `patches/codex/`: exportable patch against an explicit official Codex baseline.
- `compose.yaml`: loopback local PostgreSQL development service.
- `compose.homeserver.yaml`: trusted-LAN Home Server deployment.

## Development rules

- Preserve user changes and avoid unrelated rewrites.
- Prefer compatibility adapters and migrations over destructive format changes.
- Keep server APIs explicit about desired versus reported state.
- A new node capability requires protocol validation, a bounded implementation, dynamic tool schema
  updates and an end-to-end test.
- A new persistent field requires an append-only migration and rebuild-path consideration.
- A change affecting thread storage must test v1 compatibility, v2 commits, App Server resume and
  subagent persistence.
- A change affecting common node code must pass native Go tests and cross-compile for Windows amd64
  and Android arm64.
- Android changes should also build the debug APK when an Android SDK is available.
- Deployable Android APKs must use `node/build-android.sh` with the pinned NDK and cgo/system DNS.
  The pure-Go Android cross-compile below is compile-only; it does not validate Android domain DNS.
  Verify enrollment and WSS against a domain on a real device before releasing Android changes.
- `VERSION` is the unified Mira release SemVer. Keep checked mirrors consistent and derive Android
  versionName/versionCode and release build metadata from it. Codex version and Node wire protocol
  version are separate concepts. Never regenerate the production Android signing identity.
- Install/update code must preserve Node identity and configuration, retain old binaries, verify
  release checksums, refuse unrelated service replacement and avoid silently interrupting sessions.
- Windows PTY uses real ConPTY behind a build-tagged adapter. Test native Windows, not just cross
  compilation. Keep UTF-8 decoding state across output chunks and bound all retained data.
- For release changes, run `scripts/build-release.sh dist` and `node tests/installers_e2e.mjs`.

Run the baseline checks from the repository root:

```bash
npm run check --prefix server
(cd node && go test ./...)
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go -C node build ./cmd/mira-node
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go -C node build ./cmd/mira
GOOS=android GOARCH=arm64 CGO_ENABLED=0 go -C node build ./cmd/mira-node
for file in tests/*.mjs; do node --check "$file"; done
python3 -m compileall -q tests
POSTGRES_PASSWORD=ci-only-password docker compose -f compose.yaml config --quiet
POSTGRES_PASSWORD=ci-only-password docker compose -f compose.homeserver.yaml config --quiet
```

## Known gaps

The intended architecture still needs durable thread-to-node assignment, a task queue and scheduler,
writer leases with fencing for network partitions, a multi-version Codex compatibility matrix,
server-orchestrated fleet upgrades, scheduled credential rotation and hardware-backed key storage,
distributed login limits for multi-Server deployments, a dedicated mobile client, backup/restore tooling,
metrics and alerting. The current Agent console is an administrator-facing single-user UI, not a
multi-user collaboration product. Do not describe these gaps as already implemented.

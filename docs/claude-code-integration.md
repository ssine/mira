# Managed Claude Code

Status: implemented, with local integration validation on 2026-09-27. Not deployed
by this change. The model endpoint used in automated tests is a local fixture.

## User experience

The administrator console has a **Claude** entry, also accessible from the Codex
conversation menu. Choose an execution Node and an absolute workspace directory.
The first send prepares the pinned SDK on that Node. Subsequent turns reuse its
installed package and resume the native session from Mira's acknowledged history.

Implemented:

- Conversation creation with replay-safe request IDs, streaming Markdown, native
  reasoning/tool details, interruption, reconnect/readback, rename and archive.
- Node selection, workspace selection, native model discovery, model/effort input,
  browser-local drafts and chunked file uploads with progress and cancellation.
- Images become native typed image inputs; other attachments remain files on the
  selected Node. Uploads do not buffer whole files in the browser. Individual
  inline images are bounded to 32 MiB; ordinary files use bounded chunks.
- Native `AskUserQuestion` prompts appear in the conversation and answers return
  to the waiting SDK invocation. Ordinary tools use the same unrestricted OS-user
  execution policy as Mira's default Codex configuration.
- Mira device tools use the shared MCP adapter. The existing bounded Developer
  instructions file reader and absolute Mira SSH/SCP/SFTP instructions are shared
  with Codex. Native user/project/local Claude settings and `CLAUDE.md` still apply.
- Native subagent transcripts are mirrored independently and selectable from the
  conversation. Root membership is durable; unknown nested parent relationships
  remain unknown rather than being inferred from a filename. Original native
  metadata is retained in raw records.
- Completed conversations can resume on another approved Claude-capable Node.
  The user chooses a compatible workspace there. An active turn prevents ownership
  transfer; old Node/revision writers are rejected after a new turn is reserved.

The two engines currently have separate conversation lists in the same console.
The Claude interface does not yet implement active-turn steering, conversation
forking, a `mira claude` CLI wrapper, Codex account switching, or aggregate account/
subagent cost accounting. It shows the SDK's reported usage and cost for a run.
Workspace files and attachment paths are not automatically copied between Nodes.

## Installation and authentication

The Go Server and normal Node install remain independent of Node.js and Claude.
An execution Node needs **Node.js 22+ and npm** when Claude is selected. Mira runs
`npm ci --ignore-scripts` against an embedded lockfile only on demand. SDK
**0.3.280**, bundled Claude Code **2.1.280**, and MCP SDK **1.30.0** are pinned.
The cache lives under `<identity-directory>/runtimes/claude/<asset-hash>/`; a
completed installation is reused offline. Partial installs are staged and removed.
A Node runs at most eight active Claude turn processes. Preparation is asynchronous
and does not stop heartbeats. Shutdown interrupts workers, then kills their process
trees after a grace period. Windows child processes do not create console windows.

Authentication belongs to the Node's OS user: its Claude login/configuration or
provider environment is used. There is no model login or credential in Server
state. A Windows service does not automatically inherit an interactive user's
Claude login. These optional, local environment settings are supported:

| Variable | Purpose |
| --- | --- |
| `MIRA_NODE_CLAUDE_NODE` | Select an installed Node.js executable. npm must also be available. |
| `MIRA_NODE_CLAUDE_CONFIG_DIR` | Select the local Claude configuration/auth directory. |
| `MIRA_NODE_CLAUDE_BINARY` | Explicit native Claude executable override for the pinned SDK. |

Mira's existing Node credential is passed privately to the adapter over stdin,
not through process arguments. Runtime storage and tool calls use Mira Server;
production workers never receive PostgreSQL credentials. Local Claude transcripts
and settings follow Claude's own lifecycle. Mira does not remove user auth data.

## Persistence and failure behavior

The user explicitly accepted Claude's weaker persistence mechanism on 2026-09-23:
Claude writes local transcripts and then mirrors batches through `SessionStore`.
This **does not provide Codex's pre-sampling durability barrier**. A failed batch
can emit `system/mirror_error` while model/tool execution continues.

Schema 35 adds independent Claude sessions, turns, native transcript collections,
raw entries, operation receipts and SDK events. It does not rewrite Codex tables
or change released migrations. Raw entries/events use PostgreSQL **JSON**, preserving
unknown fields and escaped NUL. The event-type projection is separate: PostgreSQL
JSON field extraction itself can reject escaped NUL anywhere in a raw object.

- Mirror retries reuse an exact operation UUID and body. A receipt digest includes
  the transcript subpath. UUID-bearing entries deduplicate; UUID-less metadata
  remains append-only and is deduplicated only when replaying the same operation.
- Writes check the approved Node identity, session revision and owning turn.
  A storage retry never retries a tool or model request.
- Turn completion and mirror status are independent. Degradation is sticky. The
  UI requires an explicit acknowledged-history choice before resuming a degraded
  conversation; subsequent successful batches do not silently erase missing history.
- Managed resume throws when its main transcript is absent remotely, instead of
  letting the SDK fall back to arbitrary local history. It restores subagent keys
  through `listSubkeys` and keeps the native session ID across compatible Nodes.
- Ambiguous start/control failures keep the turn reserved. **Check runtime status**
  asks the owning Node whether that worker still exists. Only an explicit negative
  observation from the same runtime instance closes an unacknowledged turn as
  failed/incomplete. An offline Node or a restarted manager with an empty process
  map cannot be presumed to have stopped an old turn. Such an unresolved turn
  remains reserved; operator investigation is needed if its worker never reports
  completion. The worker closes its SDK query when the owning Node closes stdin.
- The SDK materializes an entire transcript array on resume. Server reads/writes
  stream native JSONL, but the SDK's resume memory requirement remains. A mirror
  record is bounded to 64 MiB; there is no total transcript size ceiling.
- UI history pages omit token deltas and are projections of recorded SDK events.
  Live events are cursor-based; older history loads on request. Raw native entries
  remain the resume source, independent of the visible event projection.

Wire endpoints and ownership rules are in
[Claude sessions v1](../protocol/claude-sessions-v1.md).

## Validation

The original integration was validated on main `b2f255f` (Mira 1.0.43).
The release integration uses schema 35; released migrations 1–34 remain unchanged.
Regression validation passed the full Go suite, 109 JavaScript unit tests,
Windows/Android compile checks, PostgreSQL migration/import/channel tests,
v1/v2 storage, Web/Node capabilities, and native Codex CLI/App Server resume,
active-turn steering, account handoff and persisted subagent scenarios.
Both native engine test suites use loopback simulated model endpoints.

`TestClaudeManagedSDK` runs the actual official SDK/native Claude subprocess
against a real Mira HTTP Server, PostgreSQL, two authenticated outbound Node
channels, and a simulated Anthropic Messages endpoint. It verifies device MCP
execution, exact-operation retry after a committed response is lost, restored
context after deleting local history, native questions/answers, native subagent
transcripts, cross-Node ownership/resume, interruption, and permanent mirror
failure reporting. Model discovery also uses the native SDK. An unknown turn from a previous runtime
instance remains reserved instead of being mistaken for a stopped process.

Browser acceptance covers creating/sending a conversation through the Web UI,
restoring drafts after reload, native history/images/child selection, and the
narrow-screen drawer/composer layout. The existing Web/Node regression suite
also passes against the native test Server.

```sh
MIRA_HISTORY_UPLOAD_TEST_DATABASE_URL="$DISPOSABLE_POSTGRES_URL" \
MIRA_CLAUDE_SDK_TEST=1 \
  go -C node test ./internal/miraserver -run '^TestClaude' -count=1 -v
```

`TestClaudeNativeStorage` covers raw unknown/NUL data, UUID-less retry behavior,
subpath separation/traversal rejection, stale writers, mirror degradation and CSRF.
The earlier isolated [SDK probe](../experiments/claude-code/README.md) remains
available; its direct PostgreSQL adapter is test scaffolding only.

Local acceptance covers Linux amd64. Windows amd64 and Android arm64 are compile
checks; Android advertises no Claude runtime. Native Windows execution, real
provider authentication/billing, platform-to-platform filesystem portability and
long-running background Claude tasks have not been accepted by these tests.

Official references: [programmatic usage](https://code.claude.com/docs/en/headless),
[custom tools](https://code.claude.com/docs/en/agent-sdk/custom-tools),
[SessionStore](https://code.claude.com/docs/en/agent-sdk/session-storage).

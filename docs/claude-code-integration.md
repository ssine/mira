# Managed Claude Code

Included in Mira 1.0.57. Automated tests use a local model fixture; separate
Linux acceptance also verified real Messages and Bedrock gateway APIs on 2026-09-30.

## User experience

Mira 1.0.59 uses one **Conversations** page for Codex and Claude: the same project
sidebar, transcript renderer, composer, model controls, drafts, attachments and account
popover. New conversations choose their engine through the selected account. Existing
conversations offer accounts from the same engine only; native histories are not converted.
The sidebar distinguishes same-name accounts by engine. Old Claude links resolve into
this shared page. Choose an execution Node and an absolute workspace directory.
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
- Mira device tools use the shared MCP adapter. The Node's Developer instructions
  file is shared with Codex: the same bounded reader loads it on every Claude turn and
  appends it to the system prompt together with the absolute Mira SSH/SCP/SFTP
  instructions. Native user/project/local Claude settings and `CLAUDE.md` still apply.
- Claude accounts may set a default reasoning effort for their default model. The
  model picker maps the account's full model ID to the SDK alias that resolves to it,
  so its effort levels remain selectable.
- The transcript follows Codex's rhythm: tool calls and thinking collapse into one
  line per batch, and the text Claude writes between them stays visible as progress.
  Built-in tools are summarized as actions with durations, like Codex shell actions:
  commands, reads, edits with line counts, searches and subagents. Images a tool
  returns stay in its card. Mira's system prompt asks Claude for a short update
  every several tool calls; the Developer instructions file comes later and can
  override it. While a turn runs, polls patch changed or new cards in place.
- Native subagent transcripts are mirrored independently and selectable from the
  conversation. The parent timeline shows only the subagent's tool row. Root membership is durable; unknown nested parent relationships
  remain unknown rather than being inferred from a filename. Original native
  metadata is retained in raw records.
- Messages sent while Claude runs join the running turn, as with Codex. Claude reads
  one at the next tool boundary, or answers it after the final answer. The message
  appears where Claude read it; until then it shows as waiting, and one Claude never
  read (a stop, a failed turn) shows as not added. If the turn ends before the
  message is accepted, the composer sends it as the next turn instead. Adding input
  needs a Node advertising `claudeSteerV1`; older Nodes ask the user to wait.
- Completed conversations can resume on another approved Claude-capable Node.
  The user chooses a compatible workspace there. An active turn prevents ownership
  transfer; old Node/revision writers are rejected after a new turn is reserved.

The Claude adapter does not yet implement conversation forking,
permanent deletion, automatic title generation or a `mira claude` CLI wrapper.
Unsupported actions are omitted from its menu. Native child records are grouped beneath
the owning session for browsing, without asserting unknown nested parentage. Continue
child work through its root session. Workspace files and attachment paths are not
automatically copied between Nodes.

## Usage and cost projection

Schema 36 adds immutable per-turn account attribution and a rebuildable, indexed
usage projection. It leaves Codex tables and migrations 1–35 unchanged. Historical
account attribution uses explicit immutable turn requests only; ambiguous historical
accounts stay unassigned. Account names are captured when a turn starts, so switching
or renaming an account cannot move previously attributed spend.

Each native result replaces that turn's cumulative snapshot. Session totals and per-turn
costs difference consecutive snapshots before filtering by date/account. They never add
assistant block usage or child totals to the already inclusive result. Counter resets,
missing results and unprojectable raw JSON remain partial/unavailable; real reported
zero stays zero. The append transaction updates the projection once, so an acknowledged
retry cannot charge twice. SDK events containing escaped NUL remain valid authoritative
records even if PostgreSQL cannot extract their projection.

The same conversation details, turn footers and account cost charts show Claude's SDK
estimate in USD, including cache usage. This is not a gateway billing API. Whole-session
tokens use `modelUsage`, including subagent requests; native `usage` is main-loop-only.
Message footers retain native record timestamps. Account curves preserve per-turn
timestamps, grouping adjacent turns only on dense days (at most 128 points/day),
without discarding costs or incomplete status.
No unsupported per-child cost split is inferred. See the
[official cost semantics](https://code.claude.com/docs/en/agent-sdk/cost-tracking).

A running turn has no result yet. Until its result lands, conversation summaries price it
from the latest `usage` of each `assistant` response (deduplicated by message ID,
subagents included) with the public Claude list prices in `claude_views.go`. Cache
writes use their 5-minute/1-hour multipliers. The estimate is marked `running`; unknown
models or invalid usage make it partial. The Web shows it beside the running indicator
and in the conversation total, then fetches the turn footer from the SDK result once the
turn settles. Schema 38 only adds the per-turn `assistant` event index for this lookup.

Rebuild the projection by reloading attribution from `mira_claude_turns`, then applying
`mira_claude_project_result(turn_id, seq, payload)` to the latest root `result` event per
turn. Reads scan the small usage projection, never the raw conversation history.

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

The existing **Accounts** page manages both Codex and Claude accounts. Select
Claude Code when adding an account, then configure a Messages or Bedrock API,
default model and API key. Credentials are sent over the approved Node channel
and saved with private permissions under `<identity-directory>/accounts/<id>/claude/`.
Only the account name, provider metadata, enabled state and revision are stored
on Server. Claude conversations persist their selected Node account and can
switch accounts between turns; active turns prevent configuration changes.
Model discovery and native child processes use that account's isolated environment.
The shared page supports renaming, updating credentials, and enabling/disabling
Claude accounts. Claude API quotas are not presented as Codex subscription quotas.

Without a managed account, the Node's existing native Claude login/configuration
or provider environment remains available as **Node default configuration**. A Windows service does not automatically inherit an interactive user's
Claude login. These optional, local environment settings are supported:

| Variable | Purpose |
| --- | --- |
| `MIRA_NODE_CLAUDE_NODE` | Select an installed Node.js executable. npm must also be available. |
| `MIRA_NODE_CLAUDE_CONFIG_DIR` | Select the local Claude configuration/auth directory. |
| `MIRA_NODE_CLAUDE_BINARY` | Explicit native Claude executable override for the pinned SDK. |

An executable wrapper used with `MIRA_NODE_CLAUDE_BINARY` must preserve any
existing `CLAUDE_CONFIG_DIR`: the SDK sets it to a temporary directory when
restoring a remote session. Overriding that directory breaks native resume.
Provider credentials may be supplied by the Node environment or such a wrapper;
Messages-compatible gateways use native Anthropic settings, while AWS gateways
use native Bedrock settings. No Chat Completions translation is required.

Mira's existing Node credential is passed privately to the adapter over stdin,
not through process arguments. Runtime storage and tool calls use Mira Server;
production workers never receive PostgreSQL credentials. Local Claude transcripts
and settings follow Claude's own lifecycle. Mira does not remove user auth data.

## Persistence and failure behavior

The user explicitly accepted Claude's weaker persistence mechanism on 2026-09-23:
Claude writes local transcripts and then mirrors batches through `SessionStore`.
This **does not provide Codex's pre-sampling durability barrier**. A failed batch
can emit `system/mirror_error` while model/tool execution continues.

Schema 35 adds Node-local Claude account metadata and independent sessions, turns, native transcript collections,
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
- A foreground `result` is not the end of a Mira turn while native background
  tasks remain active. Keep streaming input open using the SDK's replace-set
  `background_tasks_changed` signal; task bookends are not ordered against that
  signal. Wait for the parent's subsequent result after background work settles.
  Ambient watchers do not keep a turn running. An unexpected SDK exit without
  final completion is reported as a failure, never successful completion.
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

Native `AskUserQuestion` requests appear as visible question forms outside folded
tool activity. Single-choice, multiple-choice and free-text answers use the existing
turn-scoped answer endpoint. No option is preselected; submitting requires an answer
to every question. Polls preserve drafts, focus and in-flight submission state;
recorded answers become read-only summaries and ended turns close unanswered forms.
The activity line distinguishes waiting for an answer from model execution.

For incomplete history, the composer explains that continuing uses the saved records
and does not recover missing content. An accepted continuation acknowledges the
known gap across reloads and later turns. `historyAcknowledgementRequired` is a read
projection of accepted turn requests and subsequent mirror failures or reconciled
worker exits, ordered by session revision. The Server checks it under the session
lock before accepting a new turn. A new gap requires a new acknowledgement; an
ordinary model error does not. Historical `persistence=incomplete` stays intact and
remains visible in conversation details, while an acknowledged gap stops occupying
the composer. Older Servers without this projection retain the conservative prompt.

Transient event/transcript write failures retry the identical serialized operation
with capped backoff within one 45-second deadline, below the SDK's 60-second append
timeout. Authentication, ownership and other permanent rejections fail immediately
at the adapter boundary. Interrupts stop native work without waiting for storage;
already-started writes drain before the completion event, subject to the Node's
existing shutdown deadline. This improves brief Server-restart recovery; it does
not add Codex's pre-sampling durability barrier or recover previously missing data.

Wire endpoints and ownership rules are in
[Claude sessions v1](../protocol/claude-sessions-v1.md).

## Node-local session cache

The device workbench's overview includes **Claude Code / 本地会话缓存** on
capable Nodes. The default budget is 4 GiB; the administrator can choose another
nonnegative size or set 0 to disable it. Settings persist in
`<identity-directory>/claude-cache.json`, independently of SDK versions and
Mira updates. Cache files live under `cache/claude-sessions` in that directory.
Only execution Nodes populate this cache; configuration does not prepare the SDK.

Every resume validates a cached cursor/prefix against Server ownership and its
immutable canonical records, then downloads the missing suffix. Cache keys separate
Server origins, session UUIDs and native child subpaths. Local byte counts and full
SHA-256 digests detect truncated or changed files; corrupt/missing caches are rebuilt
from Server. Legacy Servers still return full history. No offline fallback is used.
This reduces network transfer, not model prompt tokens or the SDK's full-history
memory requirement. Provider prompt caching is independent of this disk budget.

One writer lock bounds total data/metadata across SDK workers. Writes append missing
records, sync data, and replace commit metadata. Reads accept only the committed byte
range. The Node Manager repairs dead-worker locks before new turns or settings reads;
busy cache writers are skipped without delaying native history loading. Least recently
used transcripts are evicted to fit the budget; oversized transcripts remain usable
without caching. Shrinking or disabling prunes immediately when no writer is active;
otherwise the UI reports pending cleanup, retried on the next use or refresh. Cache
I/O failures never substitute local data for Server or replay a model/tool request.

## Validation

The original integration was validated on main `b2f255f` (Mira 1.0.43).
The unified conversation release uses schema 36; released migrations 1–35 remain unchanged.
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
failure reporting. It adds input to running turns both at a tool boundary and during
the final answer, and verifies the model reads it once and a retry replays the
verdict. Model discovery also uses the native SDK. An unknown turn from a previous runtime
instance remains reserved instead of being mistaken for a stopped process.
The delayed-background regression holds a native child until the parent has
emitted an interim result, then verifies that its completion wakes the parent
and produces a final synthesis before Mira releases turn ownership.

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
checks; Android advertises no Claude runtime. Real-provider Linux acceptance verified local Claude tool execution and resume,
managed MCP tools, acknowledged persistence and restoration after Node restart
and deletion of local history through both Messages and Bedrock gateways.
Native Windows execution, platform-to-platform filesystem portability and
hours-long background workload stress testing remain outside this acceptance.

Official references: [programmatic usage](https://code.claude.com/docs/en/headless),
[custom tools](https://code.claude.com/docs/en/agent-sdk/custom-tools),
[SessionStore](https://code.claude.com/docs/en/agent-sdk/session-storage).

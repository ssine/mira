# Claude sessions v1

The `/v1/claude/` API controls an optional Node-local Claude SDK. It is independent
of Codex App Server and ThreadStore. Native session IDs, transcript lines and SDK
events are retained; shared UI objects are projections only.

## Administrator operations

Use Mira's existing administrator session; mutations require `X-Mira-CSRF`.

- `POST runtimes/{nodeId}/prepare`: asynchronously prepare the pinned SDK.
- `POST runtimes/{nodeId}/status`: bounded preparation/status observation.
- `POST runtimes/{nodeId}/describe`: read native models and local account metadata.
- `GET sessions?archived=0&offset=0`: paged conversation list (`data`, `nextOffset`).
- `POST sessions`: `{requestId,nodeId,cwd,title?,model?}`. UUID request IDs replay
  the same creation; changed creation bodies conflict.
- `GET sessions/{id}`: metadata, active turn, revision, and mirror state.
- `PATCH sessions/{id}`: `{title?,archived?}`.
- `POST sessions/{id}/turns`: `{requestId,text,nodeId?,cwd?,model?,effort?,attachments?,
  continueAcknowledgedHistory?}`. Attachment entries have native `path`, `name`,
  and `mime`. Request ID is the durable turn ID. Exact replay returns that ID,
  including after worker exit; it never dispatches again. Active turns conflict.
- `POST sessions/{id}/steer`: `{requestId,expectedTurnId,text,attachments?}` adds
  input to the running turn (see [Adding input to a running turn](#adding-input-to-a-running-turn)).
- `POST sessions/{id}/interrupt`: request native interruption and allow mirror flush.
  Added input the model has not read yet is discarded.
- `POST sessions/{id}/answer`: `{questionId,answers}` for a question emitted by
  the currently active turn. The Node forwards it to the waiting native tool.
- `POST sessions/{id}/reconcile`: inspect the owning Node. An absent worker closes
  an otherwise active turn as failed with incomplete persistence. Offline or a
  different runtime instance is not proof of absence.
- `GET sessions/{id}/events?after=N`: up to 200 raw event envelopes and a cursor.
- `GET sessions/{id}/events?view=transcript&before=N`: latest/older events without
  stream deltas, returned chronologically with `cursor` and `earliest`. `before=0`
  starts at the newest page. Pages stop near 4 MiB, allowing one larger native
  record to remain atomic. There is no whole-history count limit.
- `GET sessions/{id}/history?subpath=...`: equivalent paging for raw transcript
  entries (also accepts `view=transcript&before=N`).
- `GET sessions/{id}/children`: independent transcript IDs/subpaths, source kind,
  root ID and nullable known parent ID. No guessed nested ancestry.

New ownership is reserved under a session row lock, with an increasing revision,
only when no active turn exists. The worker's native session ID remains unchanged.
An accepted command whose reply is lost stays reserved until a completion or an
explicit negative observation from its owning Node. History status `incomplete`
requires `continueAcknowledgedHistory:true` for subsequent turns and remains visible.

## Node storage operations

These require the existing approved Node Bearer credential. Every request carries
`X-Mira-Claude-Turn` and `X-Mira-Claude-Revision`. Server verifies the session's
current Node/revision and that turn's immutable ownership. No database credential
or additional security identity is introduced.

- `POST sessions/{id}/entries?subpath=&operationId=UUID`: native JSONL, one object
  per line, transactionally streamed into PostgreSQL. Each record is bounded to
  64 MiB; total stream length is not capped. Retain all fields and escaped NUL.
- `GET sessions/{id}/entries?subpath=`: ordered native JSONL stream.
- `GET sessions/{id}/subkeys`: `{subkeys:[...]}` for native child transcripts.
- `POST sessions/{id}/events`: `{eventId:UUID,payload:object}`. SDK messages are
  unchanged. Adapter lifecycle records use `mira_user`, `mira_steer`,
  `mira_steer_rejected`, `mira_question`, `mira_answer`, `mira_interrupt_requested`,
  `mira_error`, and `mira_completed`. Question event IDs equal question IDs and
  `mira_steer` event IDs equal steer request IDs. Other IDs are independent UUIDs.

Root transcript subpath is empty. Child paths are opaque relative paths up to
1024 characters; absolute, backslash, traversal and NUL paths are rejected.
Native SDK project keys may change with the chosen workspace; lookup is by the
managed session UUID and subpath. The adapter rejects unexpected native session IDs.

An append receipt hashes the exact body and subpath. Replaying an operation with
another body/key/session conflicts. Entries carrying `uuid` deduplicate within
one transcript; UUID-less metadata is retained on distinct operations. Event
receipts likewise reject reuse with changed payload, session or turn. Commit
receipt lookup and inserts share a transaction and session serialization lock.

`mira_completed` acknowledges that the SDK iterator ended; it carries failure,
interruption and mirror degradation separately. `system/mirror_error` marks the
session incomplete, even if a later result reports successful model execution.
Neither a failed batch nor a missing display event authorizes replaying tool effects.

Transport is the existing outbound Node channel for control and HTTPS to Mira
Server for storage. The private `claude` control capability is not advertised as
an Agent device tool and is not accepted through the generic invoke API.

## Managed accounts

The existing administrator account UI includes Claude bindings, exposed as
`claudeAccounts` on Node views. `POST /v1/claude/accounts` creates a Node binding
from `nodeId` and `name`; `PATCH /v1/claude/accounts/:id` changes `name` or `enabled`.
`POST /v1/claude/accounts/:id/configure` sends a typed `provider` (`id`: `anthropic`
or `bedrock`, `baseUrl`, `model`, Bedrock `region`, and optional default `effort`:
`low`, `medium`, `high`, `xhigh` or `max`) and an optional `apiKey`
through the private Node channel. Omit `apiKey` to retain it; an empty key clears it.
Keys never enter PostgreSQL, account responses, desired state, or audit metadata.

Session creation and turn submission accept `nodeAccountId`. Turns inherit the
session's binding when omitted; an explicit empty string selects native Node
configuration. The Node/account pair is checked before turn reservation. Unknown,
disabled, unconfigured and foreign-Node bindings fail without falling back to
ambient credentials. Account row locks serialize reservation with credential
changes; the Node also refuses changes while one of that account's workers runs.
The account binding is durable metadata, independent of native transcript entries.
Runtime `describe` accepts the same binding for native model/account discovery.
When a turn resolves to the account's `model` and neither the request nor the session
supplies an effort, the account's default `effort` is used; otherwise the SDK default
applies. The account `model` is usually a full model ID while the native catalog lists
aliases, so clients match it to catalog entries through `resolvedModel`.

Each turn appends the Node's `desiredAppServer.developerInstructionsFile`, shared with
Codex, to the Claude Code system prompt. It uses the same bounded file-capability
reader (UTF-8, at most 256 KiB), is read on every turn, and blocks the turn when it
cannot be read or validated.

## Shared conversation read projections (Mira 1.0.59)

`GET /v1/claude/conversations` exposes bounded keyset pages (`view=roots|children`,
`archived`, `projectKey`, `parentThreadId`, `cursor`) and the complete root project
directory. Cursors are scoped to filters. `GET /v1/claude/conversations/:id` resolves
root and native child identities to shared Web summaries. These are read projections,
not Codex ThreadStore records. Native child collection membership does not assert
unknown direct parentage.

`GET /v1/claude/sessions/:id/costs?turnId=...` returns per-turn estimates.
`GET /v1/claude/accounts/cost-history?name=...&range=24h|7d|30d&timezone=...`
returns daily/per-turn SDK estimates (at most 128 adjacent turn groups per day) for a Claude account name. These routes require
administrator authentication. Engine namespaces keep same-name Codex/Claude totals
separate. See `docs/claude-code-integration.md` for cumulative-result and rebuild rules.

## Disposable transcript cache (Mira 1.0.61)

Desktop Nodes advertise `claudeSessionCacheV1` alongside `claudeRuntimeV1`.
Administrator+CSRF `POST runtimes/{nodeId}/cache-status` returns
`{maxBytes,usedBytes,entries,cleanupPending}`. `cache-configure` accepts only
`{maxBytes}` (a nonnegative safe integer; 0 disables). Both are private Claude
runtime controls, not dynamic tools. Settings are Node-local; Server never supplies
a filesystem path. Older Nodes reject these actions explicitly. No database
migration or change to Codex storage is involved.

An optional `GET sessions/{id}/entries?subpath=&cache=1&after=N&prefix=SHA256`
validates the cached final raw payload against the immutable sequence in PostgreSQL
under the existing ownership/session lock. A stale cursor or mismatched prefix resets
to a full read. Responses carry `X-Mira-Claude-Cache-Version: 1`,
`X-Mira-Claude-Cache-Start` (exclusive), `X-Mira-Claude-Cache-End` (inclusive), and
`X-Mira-Claude-Cache-Prefix` (SHA-256 of the last raw JSON payload, excluding the
NDJSON newline; empty for no entries). An unchanged prefix returns an empty body
with equal Start/End. Clients verify the received record count. Absent cache headers
mean a complete legacy stream and must never be appended to a cached prefix.
Ordinary GET is unchanged. Authentication/revision/ownership checks always apply,
including cache hits, and storage failure never authorizes an offline resume.

## Adding input to a running turn

Desktop Nodes that can add input to a running turn advertise `claudeSteerV1`.
`POST sessions/{id}/steer` takes `{requestId,expectedTurnId,text,attachments?}`;
the request ID is a UUID naming this input, and attachments use the turn format.
The Server forwards it to the worker that owns `expectedTurnId` and returns
`{accepted:true,turnId}` only after the worker has recorded and queued it:

- The worker records `mira_steer` `{steerId,message,text,attachments}` under the
  request ID before queueing the native user message with the same UUID. If the
  turn finishes in between, it records `mira_steer_rejected` `{steerId}` instead.
- The SDK reports the queued message with `command_lifecycle`
  `{command_uuid,state}` events: `queued`, then `started` when the model reads it
  (at the next tool boundary, or as another response when it arrives during the
  final answer), then `completed`. A stop reports `cancelled`. Results list read
  messages in `user_message_uuids`. A successful result ends the turn only once
  no accepted message is outstanding.
- `409 turn_not_steerable`: the turn is no longer `expectedTurnId`, or is
  finishing. Nothing was queued; send the input as a new turn once it ends.
- `409 steer_unsupported`: the owning Node lacks `claudeSteerV1`.
- `400`: invalid request, or input the worker could not read (such as an image).
- `503`: the worker could not record the input, so it will not join this turn.
  The worker keeps this verdict for the request ID; after the turn ends, a retry
  returns `turn_not_steerable`.
- A retried request ID replays its verdict. After the turn ends, a recorded
  `mira_steer` without a later rejection returns `{accepted:true,turnId,replayed:true}`;
  while the turn runs, the worker answers again from its own record. A worker that
  does not answer in time returns an error without a verdict: the input may still
  join the turn, and only a retry with the same request ID is safe.

Transcript projections place an added message at its `started` event. Until
then it is shown as waiting; one the turn never read is shown as not added.

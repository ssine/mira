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
- `POST sessions/{id}/interrupt`: request native interruption and allow mirror flush.
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
  unchanged. Adapter lifecycle records use `mira_user`, `mira_question`,
  `mira_answer`, `mira_interrupt_requested`, `mira_error`, and `mira_completed`.
  Question event IDs equal question IDs. Other IDs are independent UUIDs.

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

# File preview data plane v1

`filePreviewV1: true` negotiates read-only ZIP metadata, paged directories and
outbound binary file streams. The ordinary `file` capability retains existing
operations and execution-context policy. New fields are `pageSize` (1–1000),
`cursor` (nonnegative enumeration ordinal), `archive` (boolean), `archiveEntry`
(safe relative ZIP path) and optional `entryId` (central-directory ordinal), and `archiveVersion` from stat/list
for rejecting a changed archive before reusing an ordinal.
Archives accept only stat/list/read. A page returns entries, hasMore and
nextCursor; directory order follows OS enumeration and changes require refresh.
ZIP directories can be implicit; duplicate file names retain individual IDs.

Administrator APIs:

- GET/HEAD `/v1/files/meta?nodeId=...&path=...` with stat or action=list/cursor.
- GET/HEAD `/v1/files/content?...` streams current bytes, supports Range and
  conditional headers; `download=true` sets attachment disposition.
- POST `/v1/file-previews` accepts nodeId, root, relative entry and resource
  (native path or ZIP container). Requires administrator CSRF. Returns a unique
  origin, one-use bootstrap grant in its fragment, ID and expiry.
- GET/POST/DELETE `/v1/file-previews/<id>` reads owner controls, renews or stops.

Creation and renewal set preview expiry to 90 days (three months) from the
request. The claimed preview cookie lasts 90 days. Previews still require a live
administrator session and the original Node control connection, and Server
restarts discard their transient state.

For content, Server allocates a bounded transient stream UUID bound to the exact
Node control connection. It sends `file.open` with sessionId and params, including
method, selected Range/conditional headers and resource/execution context.
The Node dials `/v1/file-streams/<UUID>` using `mira-file-v1` and its `auth.*`
subprotocol. Server accepts one socket only from the allocated approved Node
and matching control epoch. There is no credential in a query string.

The first bounded text frame is `{status,headers}` for the HTTP response. Binary
frames follow, each at most 64 KiB. A final `{"done":true}` text frame marks EOF.
Only content type/length/range, Accept-Ranges, ETag and Last-Modified are relayed;
Node cannot set administrator cookies or CSP. Backpressure follows synchronous
writes over TCP/WSS and HTTP. HTTP cancellation, inactivity, `file.close`, Node
revocation/control disconnect and shutdown close sockets and cancel preparation.
There is no unbounded queue or whole-file Base64 conversion.

Node opens and authorizes files under the requested Windows execution identity.
A website's resolved native file must also remain inside its resolved site root,
including symlinks. ZIP resources retain their container/root and validate entry
names and IDs. HTTP content uses local seekable descriptors/sections or a bounded
selected-entry cache; it does not emulate seeking over sequential decompression.
A file change during transfer aborts the response. Readers use ETag/If-Range
and require refresh on a version mismatch rather than mixing versions.

The dedicated preview suffix is dispatched before **all** console assets,
administrator APIs and Node WebSockets, including unknown/nested preview hosts.
The one-use fragment grant is removed from history and exchanged by a same-origin
POST for a host-only HttpOnly cookie. HTTPS cookies use the `__Host-` prefix.
Every request checks cookie scope, expiry, live admin ownership and Node presence.
The routing ID is never authorization. CSP blocks Service Workers/native bridge
exposure; site data never receives administrator cookies or Node credentials.
Preview links can open a new tab. The destination uses its own sandbox policy,
and the preview's same-origin opener policy isolates cross-origin destinations.
Bootstrap and management paths are reserved under `/__mira_preview/`.

Source contexts are projections of canonical turn cwd and execution events, not
new authoritative history. Unknown/multiple origins require explicit Node
selection. No quote-back action or preview runtime state is stored in ThreadStore.

Multi-ingress deployments configure console-origin to preview-origin mappings on
Server. The same POST API returns the complete URL for the current configured
console origin, including the preview port; no Web or Node protocol changes are
needed. All configured preview suffixes are reserved, and each transient session
is bound to the single complete origin selected at creation. A grant or cookie
presented at another hostname or port cannot claim or read that session.

## File references: `fileProbeV1` and `desktopRevealV1`

Two `file` actions serve the administrator console's file references. They are
negotiated by Node capability flags and are accepted only from the
administrator: an Agent tool call (a Node identity) is rejected before any Node
is contacted, and neither action appears in the Agent tool schema.

`probe` (`fileProbeV1`) resolves a batch of written references and reports
which exist. Parameters: `paths` (1–64 strings, each at most 4096 UTF-16
units), optional absolute `cwd`, and the usual Windows `executionContext` /
`userSessionId`. The Node, not the Server or the browser, resolves each one the
way the Agent that wrote it would: `~` and `~/…` against the home directory of
the execution identity (the interactive user's profile under Windows `user`
context), anything else relative to `cwd`, and a rooted path without a drive
against the drive of `cwd` on Windows. `~name` is an ordinary relative name.
The result is `{home, entries}`; each entry repeats `input` and carries
`exists`, the cleaned absolute `path` when one could be formed, and for an
existing path `type` (`file`, `directory`, `other`; links are followed), `size`
and `modifiedAt`. A reference that cannot be answered sets `reason`: `invalid`,
`relative` (no `cwd`), `home_unavailable`, `forbidden` (outside the configured
roots, including through a link) or `unreadable`. A forbidden entry carries no
metadata, so a probe never discloses whether something exists outside the
roots. One unanswerable reference does not fail the batch. Nothing is read
beyond `stat`, and nothing is changed.

`reveal` (`desktopRevealV1`) shows one existing, authorized `path` in the
desktop file manager of the machine the Node runs on: a file is selected in its
folder, a directory is opened. The Node starts one fixed system program whose
only argument is that path; the target is never opened or executed. The result
adds `revealed` and `desktopPath`, the path as the file manager received it.

- WSL advertises the flag while Windows Explorer is reachable through interop.
  The path is translated with `wslpath -w`, so a distribution file becomes
  `\\wsl.localhost\<distribution>\…` and a Windows mount becomes a drive path.
- Windows always advertises it. A Node run by a user starts Explorer directly.
  A LocalSystem service starts it with the selected interactive session's own
  token on that session's desktop; `executionContext: system` is rejected
  because it has no desktop, and no logged-in user is an error rather than a
  fallback. This is the only place a Node starts a program on the interactive
  desktop. A Node that itself runs inside a job object cannot cross sessions
  and reports access denied; the installed service is in no job.
- Other Linux Nodes and Android do not advertise it.

Administrator APIs, both `POST` with CSRF proof:

- `/v1/files/probe` with `{nodeId, paths, cwd?, executionContext?, userSessionId?}`.
- `/v1/files/reveal` with `{nodeId, path, executionContext?, userSessionId?}`.

A Node without the flag answers `409 capability_unavailable`. Both calls are
audited as capability invocations like any other `file` action; the audit
record holds the action, not the paths.

The console links a reference only after its Node confirms it. Candidates are
Markdown link targets, inline code and bare paths in prose, never fenced code.
One request carries a message's distinct references (at most 64); answers are
kept in memory for ten minutes (two for a missing path) and are never
persisted. Without a recorded Node, or with a Node that cannot answer, the
console falls back to the written form: a Markdown link and code containing a
separator resolve against the recorded working directory, `~` and bare prose
stay text. A relative Markdown target that cannot be resolved is made inert
rather than left to navigate the console origin.

Creating a preview maps a root or entry that the Node rejects (missing,
unreadable, outside its roots) to `400 invalid_root` / `invalid_entry`, so the
launcher can offer the pages that do exist instead of a generic failure.

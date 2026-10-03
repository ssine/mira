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
Bootstrap and management paths are reserved under `/__mira_preview/`.

Source contexts are projections of canonical turn cwd and execution events, not
new authoritative history. Unknown/multiple origins require explicit Node
selection. No quote-back action or preview runtime state is stored in ThreadStore.

# Persistent port sites v1

A port site is a PostgreSQL route from a stable `p-<name>` hostname label to one
approved Node's `127.0.0.1:<port>`. It uses the existing preview ingress mappings,
wildcard DNS and TLS. The prefix cannot collide with released static-preview IDs.
Static previews retain their transient lifetime, grant and administrator-session
checks. Public port sites do not use those grants or Mira visitor authentication.

## Management

`GET /v1/sites?limit=50&after=<name>` returns `data`, `hasMore`, `nextCursor`.
`POST /v1/sites` accepts `name`, `nodeId` (UUID), `port`, optional `scheme`
(`http` default, `https`) and `enabled` (true default).
`GET /v1/sites/{name-or-UUID}` returns the site.
`PATCH /v1/sites/{name-or-UUID}` accepts target fields, enabled, and required
`expectedRevision`. `DELETE` requires `expectedRevision` in its JSON body.
Management uses approved Node or administrator identities; browser mutations
require the existing CSRF proof. The CLI and shared `site` tool resolve selectors.
Create is idempotent for the same name and settings; conflicts return 409. Writes
compare revisions in SQL. Delete creates a tombstone, reserving the old browser
origin permanently. Schema 40 adds only a table/index and preserves schema 39's
SQL contract for Supervisor rollback. Audit metadata excludes application data.

Records include siteId, name, nodeId, port, scheme, enabled, revision, timestamps,
url and phase. `available` means an approved, supporting Node control channel is
connected; Mira never probes or infers application health. Offline Nodes and
Server restarts retain the route. Site URLs have no login grant or token.

## Data plane

The Server selects sites by Host before its console/API routes. Every path,
including `/v1/admin/session`, belongs to the upstream service on a site Host.
Its HTTP reverse proxy dials a dedicated outbound Node transport coordinated by
`site.open {sessionId, params:{port}}` over the current Node control epoch.
The Node attaches to `/v1/site-streams/{UUID}` using `mira-site-v1` and its existing
Node credential. The exact Node, current epoch, pending session and single attach
are checked. The Node only dials IPv4 loopback, never a caller-selected address.
Server sends `site.close` on stream cleanup; disconnect/revocation closes streams.

The first frame is JSON `{ready:true}` after a successful local TCP dial, or
`{ready:false,error:"port_unavailable"}`. Subsequent binary records are:

- DATA: byte 1 followed by 1–65535 opaque bytes.
- FIN: byte 2 alone, closing only the sender's write direction.

Frames have a 64 KiB maximum, bounded queues and backpressure. Invalid framing
closes the connection. The byte adapter preserves both TCP half-closes; cancel,
stop, delete, revocation and shutdown close sockets and unblock stalled writers.
SetWriteDeadline interrupts blocked writes by closing that transport; a failed
write is never replayed. No application body-size, total-duration or idle-time
limit is imposed. Transport handshakes/local dials and idle connection pools are
bounded. Defaults: 128 streams per Server, 32 per Node, 32 upstream connections
per site; Node also enforces 32 workers. Exhausted capacity fails new connections.
Server proxy cache: 64 sites, 4 idle connections per site, 30-second idle timeout.
The limits bound resources, not the length of a request or a stream.

Both WSS and the existing ephemeral sequenced HTTPS transport carry these binary
frames. Automatic fallback, identity validation and same-session frame retry are
unchanged. Lost sessions close the application connection; Mira never resumes or
replays an HTTP operation. There is no Tailscale API, interface, route or daemon
requirement anywhere in this feature.

The proxy supports HTTP request/response streaming, SSE and WebSocket upgrades.
It preserves methods, paths, queries, statuses, application headers/cookies and
original Host, with standard hop-by-hop removal and forwarded address/proto
headers. It does not parse service payloads or validate application credentials.
HTTPS upstreams use normal certificate verification for 127.0.0.1; self-signed
certificates are not silently accepted. Arbitrary TCP/UDP protocols require a
separate transport contract and are outside this HTTP site feature.

Disable/delete commit the route change, discard idle pools and close active
streams before returning. A lifecycle gate fences dials against these changes.
Changing an enabled target invalidates idle pools; already-running requests may
finish on their old connection. Unknown/disabled site returns 404, unavailable
Node 503, backend connection failure 502. Route configuration survives updates;
live application connections do not survive worker replacement.

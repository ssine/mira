# Mira HTTPS transport v1

This transport preserves Mira's existing text/binary frame protocols. It uses
ordinary completed HTTPS responses, without Upgrade, SSE or HTTP streaming.
Local Codex App Server sockets stay on loopback WebSocket. PostgreSQL remains
conversation authority; this transport has no durable queues or RPC replay.

## Session

POST an existing Node control, App Server, SSH side or file-stream URL with
`?transport=https` and `X-Mira-Protocol` naming its existing Mira subprotocol.
A Node supplies its existing credential in `Authorization: Bearer`; the Web
client uses its administrator cookie, matching Origin and `X-Mira-Csrf`.
Credentials never appear in a URL. The 201 JSON response has a random 128-bit
hex `id`, `protocol`, `maxFrameBytes` and `pollSeconds`.

Every subsequent request reauthenticates that same identity; an ID is only a
routing key. The administrator identity is scoped to its login session.

- GET `/v1/transports/<id>/receive?after=<last-consumed-sequence>` waits up to
  20 seconds, returning binary records with Content-Length, or 204 if idle.
- POST `/v1/transports/<id>/send` carries binary records; 204 acknowledges
  acceptance into the application input queue.
- POST `/v1/transports/<id>/close` terminates the session and releases buffers.

Each direction has an independent sequence starting at one. A record is a
one-byte kind (1=text, 2=binary, 8=close), uint64 big-endian sequence,
uint32 big-endian byte length, then raw payload. Binary is never Base64.

Only one receive request is active per session. A brief overlap returns 429,
which allows retry after a timed-out proxy request finishes cancellation.
Send requests are serialized; malformed or conflicting batches return 400/409.
Send gaps are checked before any new record reaches the application.

## Retry and closure

A lost POST acknowledgement retries the exact bytes and sequence. The Server
checks recent frame digests and skips an identical accepted record. A lost GET
body retries the same cursor; output remains retained until its sequence is
acknowledged in the next GET. Readers consume a complete response before
publishing any records. HTTP/network retries last at most 55 seconds, within a
60-second session lease. A permanent failure or 410 ends the connection.

Closing application output retains already-written frames followed by a close
frame, so SSH exit messages are not truncated. Client close, Server shutdown
or expiry releases retained data. Revocation denies all further requests and
closes the existing application connection and its SSH descendants.

Session loss does not reconstruct a socket, retry an RPC or rerun a tool. The
caller observes failure and reconciles application state through existing APIs.
Storage commit idempotency and Web thread-creation request IDs remain separate
application guarantees. An accepted operation can finish after its client loses
contact; an HTTP send acknowledgement is not an operation result.

## Bounds and selection

The Server admits 512 sessions globally and 128 per authenticated owner. Each
session retains at most 64 output records and approximately 1 MiB of output;
one larger frame (up to the existing 16 MiB control bound) may occupy the queue
alone. One additional empty close record may be queued. Aggregate retained
output is at most 64 MiB. Input has one queued record and each request is bounded
to 64 records and 16 MiB plus record headers. Slow writers wait with deadlines;
SSH and resource-specific limits remain in force. Browser pending sends are
bounded to 64 records and 16 MiB. These are transport concurrency/backpressure
limits, not whole-file size limits. Sessions expire after 60 seconds without an
authenticated request, with a 10-second cleanup interval.

`MIRA_NODE_TRANSPORT=auto|websocket|https` selects native external transports.
Node configuration also accepts `transport`; auto is the default. Auto tries a
WebSocket handshake for up to three seconds. Any handshake error triggers HTTPS
with the same credential, including timeouts, connection loss and all HTTP
rejections (such as 401, proxy-generated 403, 426 and 5xx). A cancelled caller
does not start a fallback handshake.
HTTPS independently enforces authentication and authorization.
After two consecutive established WebSocket connections each disconnect within
one minute, the Node uses HTTPS on
subsequent connections. Browser adapters use the same fallback and track
abnormal disconnects per origin. Explicit WebSocket mode disables fallback.
No handshake is replayed after an uncertain HTTPS handshake response.

All external transports honor HTTP(S)_PROXY and NO_PROXY. A custom native
Dialer's explicit proxy callback is preserved for both transport variants.
TLS validation is retained, redirects are refused, and HTTP is intended only
for explicit local development endpoints; production endpoints use HTTPS.

Engine.IO's GET/POST separation informed this design, but Mira does not use
Socket.IO event envelopes, Base64 binary polling or its recovery protocol.

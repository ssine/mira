# Mira authentication protocol v1

Mira v1 has exactly two security identities: one human administrator and one long-lived identity per
approved device. Every process inside an approved device boundary (`mira-node`, `mira`, local Codex,
and a local Codex App Server) reuses that device's Node credential. `clientType` is audit metadata,
not an authorization boundary.

## Administrator

Create or reset the sole administrator on the Server host:

```bash
cd server
npm run admin -- set-password admin
```

The command reads a password from a hidden TTY, stdin, or `MIRA_ADMIN_PASSWORD_FILE`, hashes it with
Argon2id, and revokes existing sessions on reset. The running Server has no default password and does
not bootstrap one from its environment.

`POST /v1/admin/login` creates a random database-backed cookie session. Production cookies use the
`__Host-` prefix with `Secure`, `HttpOnly`, `SameSite=Strict`, and `Path=/`. Browser mutations require
the per-session `X-Mira-CSRF` value returned by login/session refresh. Login failures are throttled.

Only the administrator may list/decide enrollments, revoke/restore Nodes, and read security audit
events. It may also perform the operations available to trusted Nodes.

## Node credential and enrollment

Before the first request, a Node atomically writes a protected local identity file containing:

- a UUID credential ID;
- a random 256-bit secret in `mira_node_<credential-id>_<base64url-secret>` format;
- a stable display `nodeKey`;
- the canonical Server URL.

The Node sends `POST /v1/node-enrollments` with its descriptor, credential ID, and SHA-256 of the
decoded secret. The plaintext secret never reaches Server storage, logs, URLs, heartbeat data, or
desired state. The response contains an expiring enrollment ID and six-digit verification code.

The Node polls `GET /v1/node-enrollments/{id}` with its original token. Before approval that token is
recognized only as an unapproved identity and cannot register, heartbeat, or connect. An
administrator visually checks the code and calls one of:

```text
POST /v1/admin/enrollments/{id}/approve
POST /v1/admin/enrollments/{id}/reject
```

Approval creates (or re-enrolls) the stable Node record and binds the submitted credential hash. The
poll response then includes the Server-assigned `nodeId`; no activation request or Server-generated
secret is required.

Enrollment states are `pending`, `approved`, `rejected`, and `expired`. Approval and online state are
separate: an approved Node is online only while its reverse channel and fresh heartbeat are present.

## Trusted-device permissions

Every approved Node may list and inspect approved Nodes, invoke their advertised capabilities, use
the PostgreSQL ThreadStore, manage desired App Server state, and connect to an App Server proxy. It
may register, heartbeat, or establish a reverse WebSocket only for the `nodeId` bound to its own
credential. A Node cannot call administrator routes.

There is intentionally no v1 Node ACL, CLI sub-credential, global ThreadStore token, task ticket,
OAuth, mTLS, or multi-user policy. Target-side OS permissions, optional allowed-root narrowing, symlink resolution, output/file limits,
Android permissions, and OS privileges remain mandatory and can only narrow access.

Revocation marks the Node and every credential revoked, closes its reverse channel, closes proxies
where it is caller or target, preserves threads/audit history, and returns HTTP 403 for subsequent
requests. Re-enrollment generates a new credential. `POST /v1/admin/nodes/{id}/restore` only accepts
a fresh pending enrollment ID, so restoration also rotates the credential.

## WebSocket authentication

Non-browser clients authenticate in WebSocket subprotocols:

```text
mira-node-v1    or mira-client-v1
auth.<base64url-encoded-full-node-token>
```

The durable token is never accepted in a query parameter. The administrator website uses its cookie
session. The reverse route enforces that the URL Node ID equals the credential's Node ID.

When Mira brokers a Codex App Server, dynamic tool audit identity is the Node running that App
Server, while `targetNodeId` is the tool argument. Parent/subagent thread metadata may be attached to
the same audit event.

## Identity file

Defaults are `~/.config/mira/identity.json` on Linux/WSL,
`%LOCALAPPDATA%\\Mira\\identity.json` on Windows, APK-private no-backup storage on Android, and a
deployment-selected path such as `/var/lib/mira/identity.json` for system services.
`MIRA_IDENTITY_FILE` overrides the path. User files are atomically replaced with mode `0600`.

## Status codes

| Status | Meaning |
| ---: | --- |
| 401 | Missing, malformed, or secret-mismatched credential |
| 403 | Unapproved/revoked Node, wrong actor type, route identity mismatch, or CSRF failure |
| 404 | Node, enrollment, or resource does not exist |
| 409 | Enrollment/state conflict or unavailable advertised capability |
| 503 | Target Node reverse channel is offline |
| 504 | Capability or client operation timed out |

Security audit rows are append-only. Metadata records action, actor/client type, target, request,
thread, outcome, and bounded non-secret context; it excludes tokens, file contents, environment
values, screenshots, and command output.

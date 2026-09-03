# Node build metadata and PTY resize (Mira 0.9.0)

Node wire protocol remains **1**, independently of the Mira SemVer. Enrollment and registration
retain `nodeVersion` and optionally add:

```json
{
  "nodeVersion": "0.9.0",
  "nodeBuild": {
    "version": "0.9.0",
    "commit": "0123456789ab",
    "buildTime": "2026-09-04T00:00:00Z",
    "protocolVersion": 1,
    "goVersion": "go1.23.0",
    "platform": "windows",
    "architecture": "amd64"
  }
}
```

The Server persists this in migration 8's `node_build` JSONB columns; it remains Node-reported
metadata, not an attestation. If provided, `nodeBuild.version` must match `nodeVersion`.
Old clients without build metadata remain accepted. This is additive compatibility, not runtime
negotiation or a guarantee that arbitrary future protocol versions work.

PTY open/list/poll responses include `backend` and `resizeSupported`. Windows reports
`windows-conpty` and `true`. Linux's current `util-linux-script` backend reports `false`.
Clients should check the reported flag before sending:

```json
{"action":"resize","sessionId":"existing-session","cols":132,"rows":37}
```

Rows are 1–500 and columns 1–1000. Resize requires a live session and an implementing backend;
it does not create a new session or restart a shell. Web xterm resizes are debounced and forwarded
over the same authenticated capability route and `home_nodes.pty` dynamic tool.

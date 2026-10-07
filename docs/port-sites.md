# Persistent port sites

Upgrade Mira Server and the serving Node to 1.0.82 or newer. Reuse the existing
preview DNS/TLS and `MIRA_NODE_PREVIEW_DOMAIN` or `MIRA_NODE_PREVIEW_INGRESSES`.
No additional domain level, certificate or Node networking dependency is needed.
If preview ingress is already configured, there is no deployment configuration
change. For a new ingress follow [preview deployment](file-preview-deployment.md).

Register a local HTTP service:

```sh
mira site create --name my-service --node <Node-selector> --port 8000 --json
mira site get my-service --json
```

The result supplies a complete URL such as
`https://p-my-service.preview.mira.example.test`. Send requests directly to it;
paths and queries reach the Node's loopback port. The Web homepage's “端口站点”
panel and the shared `site` tool use the same durable routes. Mira's management
identity authorizes registration; visitors use the application's own access
policy. Configure application authentication on the service when needed.

For OpenAI-compatible services append `/v1` to the returned URL. Model name and
API key are upstream configuration; Mira does not understand or change them.
SSE and WebSocket traffic stream through the same route.

Upstream HTTP connections reuse the Node's existing data stream for subsequent
requests, avoiding repeated TLS/WebSocket handshakes. Concurrent requests open
separate bounded connections. From Mira 1.0.84, idle connections expire after
30 minutes; active SSE/WebSocket streams have no such idle limit. An interrupted request fails
without automatic replay. A later independent request can open a fresh stream.

Connection admission uses a shared Server resource budget rather than separate
32-connection quotas per Node or site. When full, Mira first closes idle pools
from the least recently used sites, stopping once space is available. Active
HTTP/SSE/WebSocket connections remain open. New connections fail if all budgeted
streams are active. `MIRA_NODE_SITE_STREAM_BUDGET` configures the budget (default
128, range 1–65536): on Server it covers all sites and Nodes, and on Node it bounds
local stream workers. Node also accepts local `siteStreamBudget` configuration.
Raise the Node worker budget alongside the Server budget when needed. Pool
retirement prevents removed proxies from retaining newly idle streams.
Upstream services and ingress proxies may close keep-alive connections earlier.

```sh
mira site update my-service --expected-revision 1 --enabled=false --json
mira site update my-service --expected-revision 2 --enabled=true --json
mira site delete my-service --expected-revision 3 --json
mira site list --limit 50 --json
```

Use the current revision from `get` when editing. A conflict means refresh before
retrying; it never silently overwrites another edit. Delete reserves the name so
an old browser origin cannot later refer to a different application. Stop/delete
closes live streams. Restart, logout and Node reconnect do not remove a route;
a worker update briefly interrupts active traffic. Availability means the Node
channel is online, not that Mira has inspected the service.

Only loopback HTTP/HTTPS ports are supported. This does not expose every Node
port automatically. The Node uses its usual outbound WSS/HTTPS Mira connection,
with no Tailscale requirement in the program. Existing ingress infrastructure is
independent and need not change. Implementation and resource bounds are specified
in [port-sites-v1](../protocol/port-sites-v1.md).

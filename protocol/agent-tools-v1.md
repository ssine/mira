# Agent device tools v1

The engine-independent tool catalog and invocation API reuse Mira's existing
CapabilityService. These are HTTP JSON endpoints, not an MCP transport. A runtime
adapter exposes the catalog through its own tool protocol, such as Claude's MCP.

Authentication is the existing approved Node Bearer credential or administrator
cookie. Every browser POST requires `X-Mira-CSRF`. No new identity or credential
is introduced. Revocation, allowed roots, operation bounds, execution-context
selection, output cursors and capability audit behavior remain unchanged.

## Catalog

`GET /v1/agent-tools` returns:

```json
{
  "namespace": "home_nodes",
  "description": "Inspect and operate trusted computers connected to the home control server.",
  "tools": [
    {
      "name": "status",
      "description": "List connected nodes or refresh detailed status for one node.",
      "inputSchema": {
        "type": "object",
        "properties": {
          "action": {"type": "string", "enum": ["list", "get"]},
          "nodeId": {"type": "string"}
        },
        "required": ["action"],
        "additionalProperties": false
      }
    }
  ]
}
```

The example abbreviates the full catalog and property descriptions. The shared
catalog in `node/internal/agenttools/tools.json` contains `status`, `file`,
`process`, `pty` and `screen`. Runtime adapters pass through complete input
schemas, including bounds and Windows execution-context options. SSH/SCP/SFTP
remain ordinary CLI commands and are not additional tools.

## Invocation

`POST /v1/agent-tools/call` accepts the same arguments as the Codex compatibility
endpoint. Optional `timeoutMs`, `X-Request-Id` and `X-Mira-Thread-Id` use the
existing capability timeout and audit semantics.

```json
{"tool":"status","arguments":{"action":"list"}}
```

A successful response uses MCP-compatible text/image content:

```json
{"content":[{"type":"text","text":"{\"nodes\":[]}"}],"isError":false}
```

Screenshot responses contain metadata text plus a separate
`{"type":"image","data":"<base64>","mimeType":"image/png"}` block. Other
results are serialized as text without interpreting unknown fields. Screenshots
must remain typed images when forwarded to a model.

Errors use Mira's normal non-2xx `{error, code}` envelope. Runtime adapters may
translate them into tool errors. **Do not automatically retry calls:** losing a
response does not prove that its side effects did not occur.

## Codex compatibility

`GET /v1/dynamic-tools` retains its `dynamicTools` namespace/function wrapper.
`POST /v1/dynamic-tools/call` retains its raw `{result: ...}` response. The App
Server broker still emits Codex `inputText` and data-URL `inputImage` items.
All these paths share the same definitions and capability dispatch; Codex's
WebSocket, thread store, account routes and session lifecycle are unchanged.

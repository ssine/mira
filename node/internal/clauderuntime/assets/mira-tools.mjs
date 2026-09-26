import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";

class MiraRequestError extends Error {}

// An in-process MCP adapter for the Agent SDK. The caller supplies its existing
// Node credential; this experiment never discovers or reads a Mira identity.
export function createMiraTools({ endpoint, credential }) {
  const base = new URL(endpoint);
  if (
    !["http:", "https:"].includes(base.protocol) ||
    base.username ||
    base.password ||
    base.search ||
    base.hash
  ) {
    throw new Error(
      "Expected an HTTP(S) Mira endpoint without embedded credentials or query parameters",
    );
  }
  if (
    typeof credential !== "string" ||
    !credential ||
    /[\r\n]/.test(credential)
  )
    throw new Error("A Node credential is required");
  const instance = new McpServer(
    { name: "home_nodes", version: "1" },
    { capabilities: { tools: {} } },
  );
  async function request(path, body, signal) {
    try {
      const response = await fetch(new URL(path, base), {
        method: body === undefined ? "GET" : "POST",
        headers: {
          authorization: `Bearer ${credential}`,
          "content-type": "application/json",
        },
        body: body === undefined ? undefined : JSON.stringify(body),
        redirect: "error",
        signal: AbortSignal.any([
          AbortSignal.timeout(125_000),
          ...(signal ? [signal] : []),
        ]),
      });
      // Do not put response bodies or credentials into SDK errors/transcripts.
      if (!response.ok)
        throw new MiraRequestError(
          `Mira tool request failed (HTTP ${response.status})`,
        );
      return await response.json();
    } catch (error) {
      if (error instanceof MiraRequestError) throw error;
      throw new MiraRequestError(
        "Mira tool request failed without a valid response; execution outcome may be unknown",
      );
    }
  }
  instance.server.setRequestHandler(
    ListToolsRequestSchema,
    async (_request, extra) => {
      const catalog = await request("/v1/agent-tools", undefined, extra.signal);
      return { tools: catalog.tools };
    },
  );
  instance.server.setRequestHandler(
    CallToolRequestSchema,
    async ({ params }, extra) => {
      // A failed response must never cause a tool's side effects to be retried.
      try {
        return await request(
          "/v1/agent-tools/call",
          { tool: params.name, arguments: params.arguments ?? {} },
          extra.signal,
        );
      } catch (error) {
        return {
          isError: true,
          content: [{ type: "text", text: error.message }],
        };
      }
    },
  );
  return { type: "sdk", name: "home_nodes", instance };
}

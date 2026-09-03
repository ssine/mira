export const dynamicToolNamespace = "home_nodes";

const nodeIdProperty = {
  type: "string",
  description: "Target node UUID from home_nodes.status(action=list).",
};

export function dynamicToolSpecs() {
  return [
    {
      type: "namespace",
      name: dynamicToolNamespace,
      description: "Inspect and operate trusted computers connected to the home control server.",
      tools: [
        {
          type: "function",
          name: "status",
          description: "List connected nodes or refresh detailed status for one node.",
          inputSchema: {
            type: "object",
            properties: {
              action: { type: "string", enum: ["list", "get"] },
              nodeId: nodeIdProperty,
            },
            required: ["action"],
            additionalProperties: false,
          },
        },
        {
          type: "function",
          name: "file",
          description:
            "List, inspect, read, write, create, move, or remove files inside a node's configured roots.",
          inputSchema: {
            type: "object",
            properties: {
              nodeId: nodeIdProperty,
              action: {
                type: "string",
                enum: ["roots", "stat", "list", "read", "write", "mkdir", "move", "remove"],
              },
              path: { type: "string" },
              destination: { type: "string" },
              content: { type: "string" },
              encoding: { type: "string", enum: ["utf8", "base64"] },
              offset: { type: "integer", minimum: 0 },
              length: { type: "integer", minimum: 1, maximum: 4194304 },
              recursive: { type: "boolean" },
              overwrite: { type: "boolean" },
            },
            required: ["nodeId", "action"],
            additionalProperties: false,
          },
        },
        {
          type: "function",
          name: "process",
          description:
            "Count or list system and managed processes, start a process, poll bounded output, or signal it.",
          inputSchema: {
            type: "object",
            properties: {
              nodeId: nodeIdProperty,
              action: { type: "string", enum: ["count", "list", "start", "poll", "signal"] },
              processId: { type: "string" },
              command: { type: "string" },
              args: { type: "array", items: { type: "string" }, maxItems: 128 },
              cwd: { type: "string" },
              env: { type: "object", additionalProperties: { type: "string" } },
              cursor: { type: "integer", minimum: 0 },
              signal: { type: "string", enum: ["SIGINT", "SIGTERM", "SIGKILL"] },
              system: { type: "boolean" },
            },
            required: ["nodeId", "action"],
            additionalProperties: false,
          },
        },
        {
          type: "function",
          name: "pty",
          description:
            "Open an interactive terminal session, write input, poll output, or close the session.",
          inputSchema: {
            type: "object",
            properties: {
              nodeId: nodeIdProperty,
              action: { type: "string", enum: ["open", "write", "poll", "close", "list"] },
              sessionId: { type: "string" },
              command: { type: "string" },
              args: { type: "array", items: { type: "string" }, maxItems: 128 },
              cwd: { type: "string" },
              input: { type: "string" },
              cursor: { type: "integer", minimum: 0 },
              rows: { type: "integer", minimum: 1, maximum: 500 },
              cols: { type: "integer", minimum: 1, maximum: 1000 },
            },
            required: ["nodeId", "action"],
            additionalProperties: false,
          },
        },
        {
          type: "function",
          name: "screen",
          description:
            "Inspect and control an Android display through a trusted Mira Node. " +
            "Screenshots are returned to the model as images.",
          inputSchema: {
            type: "object",
            properties: {
              nodeId: nodeIdProperty,
              action: {
                type: "string",
                enum: ["display", "screenshot", "hierarchy", "tap", "swipe", "key", "text"],
              },
              x: { type: "integer", minimum: 0 },
              y: { type: "integer", minimum: 0 },
              startX: { type: "integer", minimum: 0 },
              startY: { type: "integer", minimum: 0 },
              endX: { type: "integer", minimum: 0 },
              endY: { type: "integer", minimum: 0 },
              durationMs: { type: "integer", minimum: 1, maximum: 60000 },
              keyCode: {
                oneOf: [
                  { type: "integer", minimum: 0, maximum: 999 },
                  { type: "string", pattern: "^KEYCODE_[A-Z0-9_]+$" },
                ],
              },
              text: { type: "string", minLength: 1, maxLength: 4096 },
            },
            required: ["nodeId", "action"],
            additionalProperties: false,
          },
        },
      ],
    },
  ];
}

export function dynamicToolContentItems(tool, result) {
  if (
    tool === "screen" &&
    result?.action === "screenshot" &&
    result.mimeType === "image/png" &&
    result.encoding === "base64" &&
    typeof result.content === "string"
  ) {
    const { content, ...metadata } = result;
    return [
      { type: "inputText", text: JSON.stringify(metadata) },
      { type: "inputImage", imageUrl: `data:image/png;base64,${content}` },
    ];
  }
  return [{ type: "inputText", text: JSON.stringify(result) }];
}

export async function dispatchDynamicTool(capabilityService, actor, tool, args, context = {}) {
  if (tool === "status" && args?.action === "list") {
    return { nodes: await capabilityService.list(actor) };
  }
  if (!args || typeof args.nodeId !== "string") {
    throw new Error("nodeId is required");
  }
  if (tool === "status") {
    return capabilityService.invoke(actor, args.nodeId, "status", {}, context);
  }
  if (!["file", "process", "pty", "screen"].includes(tool)) {
    throw new Error(`unknown ${dynamicToolNamespace} tool: ${tool}`);
  }
  const { nodeId: _nodeId, ...params } = args;
  return capabilityService.invoke(actor, args.nodeId, tool, params, context);
}

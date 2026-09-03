import http from "node:http";
import path from "node:path";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import process from "node:process";

const projectDirectory = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const workspace = `${projectDirectory}/runtime/workspace`;
const controlUrl = "http://127.0.0.1:8787";
const token = "local-poc-token";
const nodeKey = `dynamic-tools-model-e2e-${process.pid}`;
const listenUrl = "ws://127.0.0.1:4512";
const storeId = `dynamic-tools-model-e2e-${process.pid}`;
const codexBinary =
  process.env.CODEX_TEST_BINARY ?? `${projectDirectory}/codex/codex-rs/target/nix/debug/codex`;
let targetNodeId = null;
const responseBodies = [];

function eventStream(events) {
  return `${events.map((event) => `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`).join("")}data: [DONE]\n\n`;
}

const mockServer = http.createServer(async (request, response) => {
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  const encodedBody = Buffer.concat(chunks).toString("utf8");
  if (request.method !== "POST" || !request.url?.endsWith("/responses") || !encodedBody) {
    response.writeHead(404, { "content-type": "application/json" });
    response.end(JSON.stringify({ error: "mock route not found", method: request.method, url: request.url }));
    return;
  }
  const body = JSON.parse(encodedBody);
  responseBodies.push(body);
  const sequence = responseBodies.length;
  const events =
    sequence === 1
      ? [
          { type: "response.created", response: { id: "resp-tool" } },
          {
            type: "response.output_item.done",
            item: {
              type: "function_call",
              call_id: "home-status-call",
              namespace: "home_nodes",
              name: "status",
              arguments: JSON.stringify({ action: "get", nodeId: targetNodeId }),
            },
          },
          {
            type: "response.completed",
            response: {
              id: "resp-tool",
              usage: { input_tokens: 0, output_tokens: 0, total_tokens: 0 },
            },
          },
        ]
      : [
          { type: "response.created", response: { id: "resp-final" } },
          {
            type: "response.output_item.done",
            item: {
              type: "message",
              role: "assistant",
              id: "msg-final",
              content: [{ type: "output_text", text: "DYNAMIC_TOOL_OK" }],
            },
          },
          {
            type: "response.completed",
            response: {
              id: "resp-final",
              usage: { input_tokens: 0, output_tokens: 0, total_tokens: 0 },
            },
          },
        ];
  const payload = Buffer.from(eventStream(events));
  response.writeHead(200, {
    "content-type": "text/event-stream",
    "content-length": payload.length,
    connection: "close",
  });
  response.end(payload);
});
await new Promise((resolve) => mockServer.listen(0, "127.0.0.1", resolve));
const mockPort = mockServer.address().port;

async function request(pathname) {
  const response = await fetch(`${controlUrl}${pathname}`, {
    headers: { authorization: `Bearer ${token}` },
  });
  const payload = await response.json();
  if (!response.ok) throw new Error(`${pathname}: ${response.status} ${JSON.stringify(payload)}`);
  return payload;
}

async function waitFor(predicate, description, timeoutMs = 60_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (agent.exitCode !== null) throw new Error(`node agent exited while waiting for ${description}`);
    const result = await predicate();
    if (result) return result;
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  throw new Error(`timed out waiting for ${description}`);
}

function rpc(socket) {
  let nextId = 1;
  const pending = new Map();
  const notifications = [];
  socket.addEventListener("message", (event) => {
    const message = JSON.parse(event.data);
    if (message.id !== undefined && pending.has(message.id)) {
      const { resolve, reject } = pending.get(message.id);
      pending.delete(message.id);
      if (message.error) reject(new Error(JSON.stringify(message.error)));
      else resolve(message.result);
    } else notifications.push(message);
  });
  return {
    call(method, params) {
      const id = nextId++;
      socket.send(JSON.stringify({ method, id, params }));
      return new Promise((resolve, reject) => pending.set(id, { resolve, reject }));
    },
    notify(method, params = {}) {
      socket.send(JSON.stringify({ method, params }));
    },
    async wait(method, timeoutMs = 60_000) {
      return waitFor(() => notifications.find((message) => message.method === method), method, timeoutMs);
    },
  };
}

const configOverrides = [
  'model="mock-model"',
  'model_provider="mock_provider"',
  `model_providers.mock_provider={ name="Mock", base_url="http://127.0.0.1:${mockPort}/v1", wire_api="responses", request_max_retries=0, stream_max_retries=0 }`,
  'experimental_thread_store.type="remote_http"',
  `experimental_thread_store.endpoint="${controlUrl}"`,
  `experimental_thread_store.store_id="${storeId}"`,
  `experimental_thread_store.bearer_token="${token}"`,
];
const agent = spawn("node", ["node-agent/agent.mjs"], {
  cwd: projectDirectory,
  env: {
    ...process.env,
    NODE_AGENT_KEY: nodeKey,
    CODEX_BINARY: codexBinary,
    APP_SERVER_CODEX_HOME: `${projectDirectory}/runtime/client-a`,
    APP_SERVER_LISTEN_URL: listenUrl,
    APP_SERVER_CONFIG_OVERRIDES: JSON.stringify(configOverrides),
    NODE_AGENT_ALLOWED_ROOTS: JSON.stringify([workspace]),
    NODE_AGENT_HEARTBEAT_SECONDS: "1",
  },
  stdio: ["ignore", "pipe", "pipe"],
});
const agentOutput = [];
for (const stream of [agent.stdout, agent.stderr]) {
  stream.setEncoding("utf8");
  stream.on("data", (chunk) => agentOutput.push(chunk));
}

let socket;
try {
  const node = await waitFor(async () => {
    const nodes = await request("/v1/nodes");
    return nodes.data.find(
      (candidate) =>
        candidate.nodeKey === nodeKey &&
        candidate.reportedAppServer?.status === "running" &&
        candidate.channelStatus?.connected,
    );
  }, "node and app-server");
  targetNodeId = node.nodeId;
  socket = new WebSocket(
    `ws://127.0.0.1:8787/v1/nodes/${targetNodeId}/app-server?access_token=${token}`,
  );
  await new Promise((resolve, reject) => {
    socket.addEventListener("open", resolve, { once: true });
    socket.addEventListener("error", reject, { once: true });
  });
  const client = rpc(socket);
  await client.call("initialize", {
    clientInfo: { name: "dynamic_tools_e2e", title: "Dynamic Tools E2E", version: "0.1.0" },
  });
  client.notify("initialized");
  const started = await client.call("thread/start", {
    cwd: workspace,
    approvalPolicy: "never",
    sandbox: "read-only",
  });
  await client.call("turn/start", {
    threadId: started.thread.id,
    input: [{ type: "text", text: "Report the node status." }],
    approvalPolicy: "never",
  });
  const completed = await client.wait("turn/completed", 90_000);
  const secondRequest = responseBodies[1];
  const outputItem = secondRequest?.input?.find(
    (item) => item.type === "function_call_output" && item.call_id === "home-status-call",
  );
  const outputText = JSON.stringify(outputItem?.output ?? "");
  if (completed.params.turn.status !== "completed" || !outputText.includes(node.hostname)) {
    throw new Error("dynamic tool result was not returned to the model");
  }
  console.log(
    JSON.stringify({
      ok: true,
      nodeId: targetNodeId,
      threadId: started.thread.id,
      injectedByProxy: true,
      itemToolCallIntercepted: true,
      resultReturnedToModel: true,
      responseRequests: responseBodies.length,
    }),
  );
} catch (error) {
  console.error(
    JSON.stringify({ ok: false, error: error.message, agentOutput: agentOutput.join("").slice(-4_000) }),
  );
  process.exitCode = 1;
} finally {
  socket?.close();
  if (agent.exitCode === null) {
    const exited = new Promise((resolve) => agent.once("exit", resolve));
    agent.kill("SIGTERM");
    await exited;
  }
  await new Promise((resolve) => mockServer.close(resolve));
}

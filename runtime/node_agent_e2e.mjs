import path from "node:path";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import process from "node:process";

const projectDirectory = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const controlUrl = "http://127.0.0.1:8787";
const token = "local-poc-token";
const nodeKey = `local-e2e-${process.pid}`;
const listenUrl = "ws://127.0.0.1:4511";
const storeId = `node-agent-e2e-${process.pid}`;
const workspace = `${projectDirectory}/runtime/workspace`;
const codexBinary =
  process.env.CODEX_TEST_BINARY ??
  `${projectDirectory}/codex/codex-rs/target/debug/codex`;

async function jsonRequest(pathname, options = {}) {
  const response = await fetch(`${controlUrl}${pathname}`, {
    ...options,
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
      ...(options.headers ?? {}),
    },
  });
  const payload = await response.json();
  if (!response.ok) throw new Error(`${pathname} failed: ${response.status} ${JSON.stringify(payload)}`);
  return payload;
}

async function waitFor(predicate, description, timeout = 60_000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (child.exitCode !== null) {
      throw new Error(`node agent exited with ${child.exitCode} while waiting for ${description}`);
    }
    const value = await predicate();
    if (value) return value;
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`timed out waiting for ${description}`);
}

async function findNode() {
  const nodes = await jsonRequest("/v1/nodes");
  return nodes.data.find((node) => node.nodeKey === nodeKey) ?? null;
}

async function initializeAppServer(nodeId) {
  const proxyUrl = `ws://127.0.0.1:8787/v1/nodes/${nodeId}/app-server?access_token=${token}`;
  const socket = new WebSocket(proxyUrl);
  await new Promise((resolve, reject) => {
    socket.addEventListener("open", resolve, { once: true });
    socket.addEventListener("error", reject, { once: true });
  });
  socket.send(
    JSON.stringify({
      method: "initialize",
      id: 1,
      params: {
        clientInfo: { name: "node_agent_e2e", title: "Node Agent E2E", version: "0.1.0" },
        capabilities: { experimentalApi: true },
      },
    }),
  );
  const response = await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error("initialize timed out")), 10_000);
    socket.addEventListener("message", (event) => {
      const message = JSON.parse(event.data);
      if (message.id === 1) {
        clearTimeout(timeout);
        resolve(message);
      }
    });
  });
  socket.send(JSON.stringify({ method: "initialized", params: {} }));
  if (response.error) throw new Error(`initialize failed: ${JSON.stringify(response.error)}`);
  socket.send(
    JSON.stringify({
      method: "thread/start",
      id: 2,
      params: { cwd: workspace, approvalPolicy: "never", sandbox: "read-only" },
    }),
  );
  const started = await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error("thread/start timed out")), 20_000);
    socket.addEventListener("message", (event) => {
      const message = JSON.parse(event.data);
      if (message.id === 2) {
        clearTimeout(timeout);
        resolve(message);
      }
    });
  });
  socket.close();
  if (started.error) throw new Error(`thread/start failed: ${JSON.stringify(started.error)}`);
  return { initialized: response.result, threadId: started.result.thread.id };
}

async function dynamicCall(tool, arguments_) {
  return (
    await jsonRequest("/v1/dynamic-tools/call", {
      method: "POST",
      body: JSON.stringify({ tool, arguments: arguments_ }),
    })
  ).result;
}

async function waitManaged(tool, idField, id, marker) {
  return waitFor(async () => {
    const result = await dynamicCall(tool, {
      nodeId: runningNode.nodeId,
      action: "poll",
      [idField]: id,
      cursor: 0,
    });
    const output = result.output.chunks.map((chunk) => chunk.text).join("");
    return result.running || !output.includes(marker) ? null : { result, output };
  }, `${tool} output`);
}

const child = spawn("node", ["node-agent/agent.mjs"], {
  cwd: projectDirectory,
  env: {
    ...process.env,
    NODE_AGENT_KEY: nodeKey,
    CODEX_BINARY: codexBinary,
    APP_SERVER_CODEX_HOME: `${projectDirectory}/runtime/client-a`,
    APP_SERVER_LISTEN_URL: listenUrl,
    NODE_AGENT_HEARTBEAT_SECONDS: "1",
    NODE_AGENT_ALLOWED_ROOTS: JSON.stringify([workspace]),
    APP_SERVER_CONFIG_OVERRIDES: JSON.stringify([
      'experimental_thread_store.type="remote_http"',
      'experimental_thread_store.endpoint="http://127.0.0.1:8787"',
      `experimental_thread_store.store_id="${storeId}"`,
      `experimental_thread_store.bearer_token="${token}"`,
    ]),
  },
  stdio: ["ignore", "pipe", "pipe"],
});
const output = [];
for (const stream of [child.stdout, child.stderr]) {
  stream.setEncoding("utf8");
  stream.on("data", (chunk) => output.push(chunk));
}

let runningNode;
try {
  const running = await waitFor(async () => {
    const node = await findNode();
    return node?.reportedAppServer?.status === "running" && node?.channelStatus?.connected
      ? node
      : null;
  }, "node registration and app-server startup");
  runningNode = running;
  const appServerResult = await initializeAppServer(running.nodeId);
  const stored = await jsonRequest(`/v1/stores/${storeId}`);
  const dynamicTools = stored.snapshot.created_threads[appServerResult.threadId].dynamic_tools;
  if (!dynamicTools.some((tool) => tool.name === "home_nodes")) {
    throw new Error("central proxy did not persist the injected home_nodes tools");
  }

  const status = await dynamicCall("status", { action: "get", nodeId: running.nodeId });
  const testPath = `${workspace}/node-agent-${process.pid}.txt`;
  await dynamicCall("file", {
    nodeId: running.nodeId,
    action: "write",
    path: testPath,
    content: "file-ok",
  });
  const file = await dynamicCall("file", { nodeId: running.nodeId, action: "read", path: testPath });
  await dynamicCall("file", { nodeId: running.nodeId, action: "remove", path: testPath });
  const startedProcess = await dynamicCall("process", {
    nodeId: running.nodeId,
    action: "start",
    command: "/bin/sh",
    args: ["-c", "printf process-ok"],
    cwd: workspace,
  });
  const processResult = await waitManaged(
    "process",
    "processId",
    startedProcess.processId,
    "process-ok",
  );
  const startedPty = await dynamicCall("pty", {
    nodeId: running.nodeId,
    action: "open",
    command: "/bin/sh",
    args: ["-c", "printf pty-ok"],
    cwd: workspace,
  });
  const ptyResult = await waitManaged("pty", "sessionId", startedPty.sessionId, "pty-ok");

  await jsonRequest(`/v1/nodes/${running.nodeId}/desired-app-server`, {
    method: "PUT",
    body: JSON.stringify({ running: false }),
  });
  await waitFor(async () => {
    const node = await findNode();
    return node?.reportedAppServer?.status === "stopped" ? node : null;
  }, "app-server stop");

  await jsonRequest(`/v1/nodes/${running.nodeId}/desired-app-server`, {
    method: "PUT",
    body: JSON.stringify({
      running: true,
      listenUrl,
      codexPath: codexBinary,
      codexHome: `${projectDirectory}/runtime/client-a`,
    }),
  });
  const restarted = await waitFor(async () => {
    const node = await findNode();
    return node?.reportedAppServer?.status === "running" ? node : null;
  }, "app-server restart");

  console.log(
    JSON.stringify({
      ok: true,
      nodeId: restarted.nodeId,
      nodeMode: restarted.nodeMode,
      codexVersion: restarted.reportedAppServer.codexVersion,
      appServerInitialized: Boolean(appServerResult.initialized?.userAgent),
      appServerProxy: true,
      dynamicToolsInjected: true,
      machineStatus: status.hostname === running.hostname,
      fileRoundTrip: file.content === "file-ok",
      processRoundTrip: processResult.output.includes("process-ok"),
      ptyRoundTrip: ptyResult.output.includes("pty-ok"),
      stopReconciled: true,
      restartReconciled: true,
    }),
  );
} catch (error) {
  console.error(JSON.stringify({ ok: false, error: error.message, output: output.join("").slice(-4_000) }));
  process.exitCode = 1;
} finally {
  if (child.exitCode === null) {
    const exited = new Promise((resolve) => child.once("exit", resolve));
    child.kill("SIGTERM");
    await exited;
  }
}

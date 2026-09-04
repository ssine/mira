import path from "node:path";
import fs from "node:fs/promises";
import os from "node:os";
import { execFileSync, spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import process from "node:process";

import { adminRequest, appServerWebSocket, approvePendingNode, loginAdmin } from "./auth_helpers.mjs";

const projectDirectory = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const controlUrl = "http://127.0.0.1:8787";
let token = "";
const nodeKey = `local-e2e-${process.pid}`;
const listenUrl = "ws://127.0.0.1:4511";
const storeId = `node-e2e-${process.pid}`;
const workspace = `${projectDirectory}/tests/workspace`;
const codexBinary =
  process.env.CODEX_TEST_BINARY ??
  `${projectDirectory}/codex/codex-rs/target/debug/codex`;
const nodeBinary = process.env.MIRA_NODE_TEST_BINARY ?? `${projectDirectory}/tests/bin/mira-node`;
const nodeStateDirectory = await fs.mkdtemp(path.join(os.tmpdir(), "mira-node-e2e-"));
const identityFile = path.join(nodeStateDirectory, "identity.json");
const adminSession = await loginAdmin(controlUrl);

await fs.mkdir(workspace, { recursive: true });
await fs.mkdir(`${projectDirectory}/tests/client-a`, { recursive: true });
await fs.mkdir(`${projectDirectory}/tests/bin`, { recursive: true });
if (!process.env.MIRA_NODE_TEST_BINARY) {
  execFileSync("go", ["build", "-o", nodeBinary, "./cmd/mira-node"], {
    cwd: `${projectDirectory}/node`,
  });
}

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
      throw new Error(`Mira Node exited with ${child.exitCode} while waiting for ${description}`);
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
  const socket = appServerWebSocket(controlUrl, token, nodeId, storeId);
  await new Promise((resolve, reject) => {
    socket.addEventListener("open", resolve, { once: true });
    socket.addEventListener("error", reject, { once: true });
  });
  socket.send(
    JSON.stringify({
      method: "initialize",
      id: 1,
      params: {
        clientInfo: { name: "mira_node_e2e", title: "Mira Node E2E", version: "0.1.0" },
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
      params: { approvalPolicy: "never", sandbox: "read-only" },
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
  return { initialized: response.result, threadId: started.result.thread.id, cwd: started.result.cwd };
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

const child = spawn(nodeBinary, [], {
  cwd: projectDirectory,
  env: {
    ...process.env,
    CONTROL_SERVER_TOKEN: "",
    MIRA_NODE_TOKEN: "",
    MIRA_NODE_KEY: nodeKey,
    MIRA_IDENTITY_FILE: identityFile,
    CODEX_BINARY: codexBinary,
    APP_SERVER_CODEX_HOME: `${projectDirectory}/tests/client-a`,
    APP_SERVER_LISTEN_URL: listenUrl,
    MIRA_NODE_HEARTBEAT_SECONDS: "1",
    MIRA_NODE_ALLOWED_ROOTS: JSON.stringify([workspace]),
    APP_SERVER_CONFIG_OVERRIDES: JSON.stringify([
      'experimental_thread_store.type="remote_http"',
      'experimental_thread_store.endpoint="http://127.0.0.1:8787"',
      `experimental_thread_store.store_id="${storeId}"`,
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
  await approvePendingNode(controlUrl, adminSession, nodeKey);
  const approvedIdentity = await waitFor(async () => {
    try {
      const value = JSON.parse(await fs.readFile(identityFile, "utf8"));
      return value.nodeId ? value : null;
    } catch { return null; }
  }, "Node identity approval");
  token = approvedIdentity.token;
  const running = await waitFor(async () => {
    const node = await findNode();
    return node?.reportedAppServer?.status === "running" && node?.channelStatus?.connected
      ? node
      : null;
  }, "node registration and app-server startup");
  runningNode = running;
  const expectedMiraCLIPath = path.join(path.dirname(nodeBinary), "mira");
  if (running.machineStatus?.miraCliPath !== expectedMiraCLIPath ||
      running.reportedAppServer?.miraCliPath !== expectedMiraCLIPath) {
    throw new Error("Node did not report the installed Mira CLI absolute path");
  }
  const configured = await jsonRequest(`/v1/nodes/${running.nodeId}/desired-app-server`, {
    method: "PUT",
    body: JSON.stringify({ running: true, defaultCwd: workspace }),
  });
  if (configured.desiredAppServer?.defaultCwd !== workspace) {
    throw new Error("Node default working directory was not persisted");
  }
  const appServerResult = await initializeAppServer(running.nodeId);
  if (appServerResult.cwd !== workspace) {
    throw new Error(`App Server did not apply the Node default cwd: ${appServerResult.cwd}`);
  }
  const runtimeBinding = await waitFor(async () => {
    const threads = await adminRequest(
      controlUrl, adminSession, `/v1/codex/threads?storeId=${encodeURIComponent(storeId)}`,
    );
    return threads.data?.find((thread) =>
      thread.threadId === appServerResult.threadId && thread.runtimeNodeId === running.nodeId) ?? null;
  }, "durable thread runtime binding");
  if (runtimeBinding.cwd !== workspace) {
    throw new Error(`PostgreSQL thread projection did not preserve the Node default cwd: ${runtimeBinding.cwd}`);
  }
  const stored = await jsonRequest(`/v1/stores/${storeId}`);
  const dynamicTools = stored.snapshot.created_threads[appServerResult.threadId].dynamic_tools;
  if (!dynamicTools.some((tool) => tool.name === "home_nodes")) {
    throw new Error("central proxy did not persist the injected home_nodes tools");
  }

  const status = await dynamicCall("status", { action: "get", nodeId: running.nodeId });
  const testPath = `${workspace}/mira-node-${process.pid}.txt`;
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
      codexHome: `${projectDirectory}/tests/client-a`,
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
      runtimeNodeId: runtimeBinding.runtimeNodeId,
      dynamicToolsInjected: true,
      miraCliPathReported: true,
      defaultCwdApplied: runtimeBinding.cwd === workspace,
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
  await fs.rm(nodeStateDirectory, { recursive: true, force: true });
}

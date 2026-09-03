import path from "node:path";
import fs from "node:fs/promises";
import os from "node:os";
import { execFileSync, spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import process from "node:process";

import { approvePendingNode, loginAdmin } from "./auth_helpers.mjs";

const projectDirectory = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const controlUrl = (process.env.MIRA_SERVER_URL ?? process.env.CONTROL_SERVER_URL ?? "http://127.0.0.1:18787").replace(/\/$/, "");
let token = process.env.MIRA_NODE_TOKEN ?? "";
if (!token && process.env.MIRA_IDENTITY_FILE) {
  token = JSON.parse(await fs.readFile(process.env.MIRA_IDENTITY_FILE, "utf8")).token;
}
if (!token) throw new Error("set MIRA_NODE_TOKEN or MIRA_IDENTITY_FILE to an approved Node identity");
const codexBinary =
  process.env.CODEX_TEST_BINARY ??
  `${projectDirectory}/codex/codex-rs/target/nix/debug/codex`;
const wslNodeKey = `wsl-homeserver-e2e-${process.pid}`;
const wslListenUrl = "ws://127.0.0.1:4520";
const storeId = `homeserver-multinode-${process.pid}`;
const configOverrides = [
  'experimental_thread_store.type="remote_http"',
  `experimental_thread_store.endpoint="${controlUrl}"`,
  `experimental_thread_store.store_id="${storeId}"`,
];
const nodeBinary = process.env.MIRA_NODE_TEST_BINARY ?? `${projectDirectory}/tests/bin/mira-node`;
const nodeStateDirectory = await fs.mkdtemp(path.join(os.tmpdir(), "mira-homeserver-e2e-"));
const identityFile = path.join(nodeStateDirectory, "identity.json");
const adminSession = await loginAdmin(controlUrl);

await fs.mkdir(`${projectDirectory}/tests/workspace`, { recursive: true });
await fs.mkdir(`${projectDirectory}/tests/client-a`, { recursive: true });
await fs.mkdir(`${projectDirectory}/tests/bin`, { recursive: true });
if (!process.env.MIRA_NODE_TEST_BINARY) {
  execFileSync("go", ["build", "-o", nodeBinary, "./cmd/mira-node"], {
    cwd: `${projectDirectory}/node`,
  });
}

async function request(pathname, options = {}) {
  const response = await fetch(`${controlUrl}${pathname}`, {
    ...options,
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
      ...(options.headers ?? {}),
    },
  });
  const payload = await response.json();
  if (!response.ok) {
    throw new Error(`${options.method ?? "GET"} ${pathname}: ${response.status} ${JSON.stringify(payload)}`);
  }
  return payload;
}

async function nodes() {
  return (await request("/v1/nodes")).data;
}

async function waitFor(predicate, description, timeout = 60_000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const value = await predicate();
    if (value) return value;
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`timed out waiting for ${description}`);
}

async function setDesired(nodeId, desired) {
  return request(`/v1/nodes/${nodeId}/desired-app-server`, {
    method: "PUT",
    body: JSON.stringify(desired),
  });
}

async function dynamicCall(tool, arguments_) {
  return (
    await request("/v1/dynamic-tools/call", {
      method: "POST",
      body: JSON.stringify({ tool, arguments: arguments_ }),
    })
  ).result;
}

function outputText(view) {
  return (view.output?.chunks ?? []).map((chunk) => chunk.text).join("");
}

async function openAppServer() {
  const socket = new WebSocket(wslListenUrl);
  await new Promise((resolve, reject) => {
    socket.addEventListener("open", resolve, { once: true });
    socket.addEventListener("error", reject, { once: true });
  });
  let requestId = 0;
  async function call(method, params, timeoutMs = 30_000) {
    requestId += 1;
    const id = requestId;
    socket.send(JSON.stringify({ method, id, params }));
    return new Promise((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error(`${method} timed out`)), timeoutMs);
      function onMessage(event) {
        const message = JSON.parse(event.data);
        if (message.id !== id) return;
        clearTimeout(timeout);
        socket.removeEventListener("message", onMessage);
        if (message.error) reject(new Error(`${method}: ${JSON.stringify(message.error)}`));
        else resolve(message.result);
      }
      socket.addEventListener("message", onMessage);
    });
  }
  await call("initialize", {
    clientInfo: { name: "homeserver_multinode_e2e", title: "Home Multi-node E2E", version: "0.1.0" },
    capabilities: { experimentalApi: true },
  });
  socket.send(JSON.stringify({ method: "initialized" }));
  return { socket, call };
}

let wslNode = null;
let wslNodeId = null;
let homeNode = null;
const nodeOutput = [];

try {
  homeNode = (await nodes()).find((node) => node.nodeKey === "mira-homeserver-docker");
  if (!homeNode) throw new Error("mira-homeserver-docker node is not registered");

  await setDesired(homeNode.nodeId, { running: false });
  await waitFor(async () => {
    const node = (await nodes()).find((item) => item.nodeId === homeNode.nodeId);
    return node?.status === "online" && node.reportedAppServer?.status === "stopped" ? node : null;
  }, "Home Server App Server stop");
  await setDesired(homeNode.nodeId, {
    running: true,
    listenUrl: "ws://127.0.0.1:4510",
    codexPath: "/usr/local/bin/codex",
    codexHome: "/home/node/.codex",
    configOverrides: [],
  });
  homeNode = await waitFor(async () => {
    const node = (await nodes()).find((item) => item.nodeId === homeNode.nodeId);
    return node?.status === "online" && node.reportedAppServer?.status === "running" ? node : null;
  }, "Home Server App Server restart");

  const machineStatus = await dynamicCall("status", {
    action: "get",
    nodeId: homeNode.nodeId,
  });
  const roots = await dynamicCall("file", {
    nodeId: homeNode.nodeId,
    action: "roots",
  });
  const writableRoot = (roots.roots.find((root) => root.configured === "/home/node") ?? roots.roots[0]).configured;
  const remoteFile = `${writableRoot}/mira-node-e2e-${process.pid}.txt`;
  await dynamicCall("file", {
    nodeId: homeNode.nodeId,
    action: "write",
    path: remoteFile,
    content: "HOME_NODE_FILE_OK",
  });
  const remoteRead = await dynamicCall("file", {
    nodeId: homeNode.nodeId,
    action: "read",
    path: remoteFile,
  });
  await dynamicCall("file", {
    nodeId: homeNode.nodeId,
    action: "remove",
    path: remoteFile,
  });
  if (remoteRead.content !== "HOME_NODE_FILE_OK") {
    throw new Error("Home Server node file round-trip failed");
  }

  const remoteProcess = await dynamicCall("process", {
    nodeId: homeNode.nodeId,
    action: "start",
    command: "/bin/sh",
    args: ["-c", "printf HOME_NODE_PROCESS_OK"],
    cwd: writableRoot,
  });
  await waitFor(async () => {
    const view = await dynamicCall("process", {
      nodeId: homeNode.nodeId,
      action: "poll",
      processId: remoteProcess.processId,
    });
    return outputText(view).includes("HOME_NODE_PROCESS_OK") ? view : null;
  }, "Home Server managed process output");

  const remotePty = await dynamicCall("pty", {
    nodeId: homeNode.nodeId,
    action: "open",
    command: "/bin/sh",
    args: ["-c", "printf HOME_NODE_PTY_OK"],
    cwd: writableRoot,
  });
  await waitFor(async () => {
    const view = await dynamicCall("pty", {
      nodeId: homeNode.nodeId,
      action: "poll",
      sessionId: remotePty.sessionId,
    });
    return outputText(view).includes("HOME_NODE_PTY_OK") ? view : null;
  }, "Home Server PTY output");

  wslNode = spawn(nodeBinary, [], {
    cwd: projectDirectory,
    env: {
      ...process.env,
      CONTROL_SERVER_URL: controlUrl,
      CONTROL_SERVER_TOKEN: "",
      MIRA_NODE_TOKEN: "",
      MIRA_NODE_KEY: wslNodeKey,
      MIRA_IDENTITY_FILE: identityFile,
      MIRA_NODE_HEARTBEAT_SECONDS: "1",
      CODEX_BINARY: codexBinary,
      APP_SERVER_CODEX_HOME: `${projectDirectory}/tests/client-a`,
      APP_SERVER_LISTEN_URL: wslListenUrl,
      APP_SERVER_CONFIG_OVERRIDES: JSON.stringify(configOverrides),
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  for (const stream of [wslNode.stdout, wslNode.stderr]) {
    stream.setEncoding("utf8");
    stream.on("data", (chunk) => nodeOutput.push(chunk));
  }

  await approvePendingNode(controlUrl, adminSession, wslNodeKey);

  const wslNodeRecord = await waitFor(async () => {
    if (wslNode.exitCode !== null) {
      throw new Error(`WSL Mira Node exited with ${wslNode.exitCode}: ${nodeOutput.join("").slice(-2_000)}`);
    }
    const node = (await nodes()).find((item) => item.nodeKey === wslNodeKey);
    return node?.status === "online" && node.reportedAppServer?.status === "running" ? node : null;
  }, "WSL node registration and App Server startup");
  wslNodeId = wslNodeRecord.nodeId;

  const client = await openAppServer();
  let threadId;
  try {
    const started = await client.call("thread/start", {
      cwd: `${projectDirectory}/tests/workspace`,
      approvalPolicy: "never",
      sandbox: "read-only",
    });
    threadId = started.thread.id;
    const listed = await client.call("thread/list", { limit: 20 });
    if (!listed.data.some((thread) => thread.id === threadId)) {
      throw new Error("WSL App Server could not list the thread it created");
    }
  } finally {
    client.socket.close();
  }

  const stored = await waitFor(async () => {
    const snapshot = await request(`/v1/stores/${storeId}`);
    return snapshot.snapshot?.created_threads?.[threadId] ? snapshot : null;
  }, "WSL-created thread in Home Server PostgreSQL");
  const events = await request(`/v1/stores/${storeId}/events?limit=100`);
  if (!events.data.some((event) => event.codexVersion === "0.151.0")) {
    throw new Error("Home Server event log did not record the patched Codex version");
  }

  const onlineNodes = await nodes();
  const selected = onlineNodes.filter(
    (node) => [homeNode.nodeId, wslNodeId].includes(node.nodeId) && node.status === "online",
  );
  if (selected.length !== 2) throw new Error("both execution nodes were not online together");

  console.log(
    JSON.stringify({
      ok: true,
      controlPlane: "homeserver",
      nodes: selected.map((node) => ({
        nodeId: node.nodeId,
        nodeKey: node.nodeKey,
        nodeMode: node.nodeMode,
        codexVersion: node.reportedAppServer.codexVersion,
      })),
      homeStopStartReconciled: true,
      homeMachineStatus: machineStatus.hostname,
      homeFileRoundTrip: true,
      homeProcessRoundTrip: true,
      homePtyRoundTrip: true,
      wslAppServerInitialized: true,
      postgresStoreId: storeId,
      postgresVersion: stored.version,
      postgresThreadId: threadId,
      codexVersionRecorded: true,
    }),
  );
} catch (error) {
  console.error(
    JSON.stringify({ ok: false, error: error.message, nodeOutput: nodeOutput.join("").slice(-4_000) }),
  );
  process.exitCode = 1;
} finally {
  if (wslNodeId) {
    await setDesired(wslNodeId, { running: false }).catch(() => {});
    await waitFor(async () => {
      const node = (await nodes()).find((item) => item.nodeId === wslNodeId);
      return node?.reportedAppServer?.status === "stopped" ? node : null;
    }, "WSL App Server cleanup", 15_000).catch(() => {});
  }
  if (wslNode?.exitCode === null) {
    const exited = new Promise((resolve) => wslNode.once("exit", resolve));
    wslNode.kill("SIGTERM");
    await exited;
  }
  if (homeNode) {
    await setDesired(homeNode.nodeId, {
      running: true,
      listenUrl: "ws://127.0.0.1:4510",
      codexPath: "/usr/local/bin/codex",
      codexHome: "/home/node/.codex",
      configOverrides: [],
    }).catch(() => {});
  }
  await fs.rm(nodeStateDirectory, { recursive: true, force: true });
}

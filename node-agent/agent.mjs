import crypto from "node:crypto";
import fsSync from "node:fs";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { spawn } from "node:child_process";
import { execFile } from "node:child_process";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

import { CapabilityRuntime } from "./capabilities.mjs";

const execFileAsync = promisify(execFile);
const agentVersion = "0.2.0";
const moduleDirectory = path.dirname(fileURLToPath(import.meta.url));
const projectDirectory = path.dirname(moduleDirectory);
const controlUrl = (process.env.CONTROL_SERVER_URL ?? "http://127.0.0.1:8787").replace(
  /\/$/,
  "",
);
const controlToken = process.env.CONTROL_SERVER_TOKEN ?? "local-poc-token";
const configuredListenUrl = process.env.APP_SERVER_LISTEN_URL ?? "ws://127.0.0.1:4510";
const configuredCodexHome = process.env.APP_SERVER_CODEX_HOME ?? null;
const configuredConfigOverrides = (() => {
  const encoded = process.env.APP_SERVER_CONFIG_OVERRIDES;
  if (!encoded) return [];
  const parsed = JSON.parse(encoded);
  if (
    !Array.isArray(parsed) ||
    parsed.length > 20 ||
    parsed.some((value) => typeof value !== "string" || value.length === 0 || value.length > 2_048)
  ) {
    throw new Error("APP_SERVER_CONFIG_OVERRIDES must be a JSON array of at most 20 strings");
  }
  return parsed;
})();
const heartbeatOverride = Number.parseInt(process.env.NODE_AGENT_HEARTBEAT_SECONDS ?? "0", 10);

let stopping = false;
let nodeId = null;
let desiredAppServer = null;
let appServer = null;
let codexInstallations = [];
let lastError = null;
let controlSocket = null;
const appServerTunnels = new Map();
const capabilityRuntime = new CapabilityRuntime();

function log(message, fields = {}) {
  process.stdout.write(`${JSON.stringify({ time: new Date().toISOString(), message, ...fields })}\n`);
}

async function postJson(pathname, method, body) {
  const response = await fetch(`${controlUrl}${pathname}`, {
    method,
    headers: {
      authorization: `Bearer ${controlToken}`,
      "content-type": "application/json",
    },
    body: JSON.stringify(body),
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new Error(`${method} ${pathname} returned ${response.status}: ${JSON.stringify(payload)}`);
  }
  return payload;
}

async function candidatePaths() {
  if (process.env.CODEX_BINARY) {
    return [path.resolve(process.env.CODEX_BINARY)];
  }
  const candidates = [
    path.join(projectDirectory, "codex", "codex-rs", "target", "debug", "codex"),
  ];
  try {
    const { stdout } = await execFileAsync("sh", ["-lc", "command -v codex"], {
      timeout: 5_000,
    });
    if (stdout.trim()) candidates.push(stdout.trim());
  } catch {
    // The bundled/test binary may still be available.
  }
  return [...new Set(candidates.map((candidate) => path.resolve(candidate)))];
}

async function fileHash(candidate) {
  try {
    const hash = crypto.createHash("sha256");
    await new Promise((resolve, reject) => {
      const stream = fsSync.createReadStream(candidate);
      stream.on("data", (chunk) => hash.update(chunk));
      stream.on("error", reject);
      stream.on("end", resolve);
    });
    return hash.digest("hex");
  } catch {
    return null;
  }
}

async function inspectCodex(candidate) {
  try {
    const [{ stdout: versionOutput }, { stdout: helpOutput, stderr: helpError }, sha256] =
      await Promise.all([
        execFileAsync(candidate, ["--version"], { timeout: 10_000 }),
        execFileAsync(candidate, ["app-server", "--help"], { timeout: 10_000 }),
        fileHash(candidate),
      ]);
    const help = `${helpOutput}\n${helpError}`;
    return {
      path: candidate,
      version: versionOutput.trim(),
      sha256,
      appServerSupported: help.includes("--listen") && help.includes("generate-json-schema"),
      validatedAt: new Date().toISOString(),
    };
  } catch (error) {
    return {
      path: candidate,
      version: null,
      sha256: null,
      appServerSupported: false,
      validationError: error.message,
      validatedAt: new Date().toISOString(),
    };
  }
}

async function discoverCodex() {
  codexInstallations = [];
  const candidates = await candidatePaths();
  log("discovering Codex installations", { candidates });
  for (const candidate of candidates) {
    const installation = await inspectCodex(candidate);
    codexInstallations.push(installation);
    log("inspected Codex installation", {
      path: installation.path,
      version: installation.version,
      appServerSupported: installation.appServerSupported,
    });
  }
  return codexInstallations;
}

async function detectNodeMode() {
  if (process.platform !== "linux") return process.platform;
  try {
    const release = await fs.readFile("/proc/sys/kernel/osrelease", "utf8");
    if (/microsoft|wsl/i.test(release) || process.env.WSL_DISTRO_NAME) return "wsl";
  } catch {
    // Fall back to plain Linux.
  }
  if (process.env.ANDROID_ROOT || process.env.TERMUX_VERSION) return "android";
  return "linux";
}

function capabilities(nodeMode) {
  return {
    appServer: true,
    shell: true,
    files: true,
    processes: true,
    pty: true,
    reverseChannel: true,
    nativePaths: true,
    rootAvailable: typeof process.getuid === "function" && process.getuid() === 0,
    nodeMode,
  };
}

function controlWebSocketUrl() {
  const url = new URL(controlUrl);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  url.pathname = `/v1/nodes/${nodeId}/connect`;
  url.search = "";
  return url.toString();
}

function sendControl(message) {
  if (controlSocket?.readyState !== WebSocket.OPEN) throw new Error("control channel is offline");
  controlSocket.send(JSON.stringify(message));
}

function closeAppServerTunnels() {
  for (const socket of appServerTunnels.values()) socket.close();
  appServerTunnels.clear();
}

async function openAppServerTunnel(sessionId) {
  if (typeof sessionId !== "string" || appServerTunnels.has(sessionId)) return;
  if (!appServer?.ready) throw new Error("local app-server is not running");
  const socket = new WebSocket(appServer.listenUrl);
  appServerTunnels.set(sessionId, socket);
  socket.addEventListener("message", (event) => {
    try {
      sendControl({ type: "appserver.message", sessionId, payload: String(event.data) });
    } catch {
      socket.close();
    }
  });
  socket.addEventListener("close", () => {
    appServerTunnels.delete(sessionId);
    if (controlSocket?.readyState === WebSocket.OPEN) {
      sendControl({ type: "appserver.closed", sessionId });
    }
  });
  socket.addEventListener("error", () => {
    if (controlSocket?.readyState === WebSocket.OPEN) {
      sendControl({ type: "appserver.error", sessionId, error: "local app-server connection failed" });
    }
  });
  await new Promise((resolve, reject) => {
    socket.addEventListener("open", resolve, { once: true });
    socket.addEventListener("error", reject, { once: true });
  });
  sendControl({ type: "appserver.opened", sessionId });
}

async function handleControlMessage(event) {
  let message;
  try {
    message = JSON.parse(String(event.data));
  } catch {
    controlSocket?.close(1007, "invalid JSON");
    return;
  }
  if (message.type === "request") {
    try {
      const result = await capabilityRuntime.execute(message.capability, message.params);
      sendControl({ type: "response", requestId: message.requestId, ok: true, result });
    } catch (error) {
      sendControl({
        type: "response",
        requestId: message.requestId,
        ok: false,
        error: { message: error.message },
      });
    }
    return;
  }
  if (message.type === "appserver.open") {
    try {
      await openAppServerTunnel(message.sessionId);
    } catch (error) {
      sendControl({ type: "appserver.error", sessionId: message.sessionId, error: error.message });
    }
    return;
  }
  if (message.type === "appserver.message") {
    const socket = appServerTunnels.get(message.sessionId);
    if (socket?.readyState === WebSocket.CONNECTING) {
      await new Promise((resolve, reject) => {
        socket.addEventListener("open", resolve, { once: true });
        socket.addEventListener("error", reject, { once: true });
      });
    }
    if (socket?.readyState === WebSocket.OPEN) socket.send(message.payload);
    else {
      sendControl({
        type: "appserver.error",
        sessionId: message.sessionId,
        error: "tunnel is not open",
      });
    }
    return;
  }
  if (message.type === "appserver.close") {
    appServerTunnels.get(message.sessionId)?.close();
    appServerTunnels.delete(message.sessionId);
  }
}

async function ensureControlChannel() {
  if (controlSocket?.readyState === WebSocket.OPEN) return;
  if (controlSocket?.readyState === WebSocket.CONNECTING) return;
  const protocolToken = `auth.${Buffer.from(controlToken).toString("base64url")}`;
  const socket = new WebSocket(controlWebSocketUrl(), ["codex-node-v1", protocolToken]);
  controlSocket = socket;
  socket.addEventListener("message", (event) => void handleControlMessage(event));
  socket.addEventListener("close", () => {
    if (controlSocket === socket) controlSocket = null;
    closeAppServerTunnels();
  });
  await new Promise((resolve, reject) => {
    socket.addEventListener("open", resolve, { once: true });
    socket.addEventListener("error", reject, { once: true });
  });
  sendControl({ type: "hello", nodeId, agentVersion, protocolVersion: 1 });
  log("connected reverse capability channel", { nodeId });
}

function reportedAppServer() {
  if (appServer?.child && appServer.child.exitCode === null) {
    return {
      status: appServer.ready ? "running" : "starting",
      pid: appServer.child.pid,
      listenUrl: appServer.listenUrl,
      codexPath: appServer.codex.path,
      codexVersion: appServer.codex.version,
      codexHome: appServer.codexHome,
      configOverrideCount: appServer.configOverrides.length,
      startedAt: appServer.startedAt,
      lastError,
    };
  }
  return { status: "stopped", lastError };
}

async function register() {
  const nodeMode = await detectNodeMode();
  const nodeKey =
    process.env.NODE_AGENT_KEY ?? `${os.hostname()}:${process.platform}:${nodeMode}:${os.arch()}`;
  const response = await postJson("/v1/nodes/register", "POST", {
    nodeKey,
    hostname: os.hostname(),
    platform: process.platform,
    architecture: os.arch(),
    nodeMode,
    agentVersion,
    capabilities: capabilities(nodeMode),
    codexInstallations,
    defaultDesiredAppServer: {
      running: process.env.APP_SERVER_AUTO_START !== "false",
      listenUrl: configuredListenUrl,
      codexPath: process.env.CODEX_BINARY ?? null,
      codexHome: configuredCodexHome,
      configOverrides: configuredConfigOverrides,
      revision: 1,
    },
  });
  nodeId = response.nodeId;
  desiredAppServer = response.desiredAppServer;
  log("registered node", { nodeId, nodeKey, desiredAppServer, codexInstallations });
  return response.heartbeatIntervalSeconds;
}

function healthUrl(listenUrl) {
  const url = new URL(listenUrl);
  if (url.protocol === "ws:") url.protocol = "http:";
  if (url.protocol === "wss:") url.protocol = "https:";
  url.pathname = "/healthz";
  url.search = "";
  return url.toString();
}

async function waitForReady(instance) {
  const deadline = Date.now() + 20_000;
  const url = healthUrl(instance.listenUrl);
  while (Date.now() < deadline) {
    if (instance.child.exitCode !== null) {
      throw new Error(`app-server exited with code ${instance.child.exitCode}`);
    }
    try {
      const response = await fetch(url, { signal: AbortSignal.timeout(1_000) });
      if (response.ok) return;
    } catch {
      // Startup polling is expected to fail briefly.
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`app-server did not become ready at ${url}`);
}

function selectCodex(desired) {
  if (desired.codexPath) {
    return codexInstallations.find(
      (installation) => installation.path === path.resolve(desired.codexPath),
    );
  }
  return codexInstallations.find((installation) => installation.appServerSupported);
}

async function startAppServer(desired) {
  const codex = selectCodex(desired);
  if (!codex?.appServerSupported) throw new Error("no validated Codex app-server installation found");
  const listenUrl = desired.listenUrl ?? configuredListenUrl;
  if (!/^ws:\/\/127\.0\.0\.1:\d+$/.test(listenUrl)) {
    throw new Error("the current node agent only permits loopback ws://127.0.0.1:PORT listeners");
  }
  const codexHome = desired.codexHome ?? configuredCodexHome;
  const configOverrides = desired.configOverrides ?? configuredConfigOverrides;
  const environment = { ...process.env };
  if (codexHome) environment.CODEX_HOME = codexHome;
  const arguments_ = ["app-server", "--listen", listenUrl];
  for (const override of configOverrides) arguments_.push("-c", override);
  const child = spawn(codex.path, arguments_, {
    env: environment,
    stdio: ["ignore", "pipe", "pipe"],
  });
  const output = [];
  for (const stream of [child.stdout, child.stderr]) {
    stream.setEncoding("utf8");
    stream.on("data", (chunk) => {
      output.push(chunk);
      if (output.length > 30) output.shift();
    });
  }
  const instance = {
    child,
    codex,
    codexHome,
    configOverrides,
    listenUrl,
    startedAt: new Date().toISOString(),
    ready: false,
    output,
  };
  appServer = instance;
  child.on("exit", (code, signal) => {
    if (appServer === instance) {
      appServer = null;
      if (!stopping && desiredAppServer?.running) {
        lastError = `app-server exited unexpectedly: code=${code} signal=${signal}`;
      }
    }
  });
  try {
    await waitForReady(instance);
    instance.ready = true;
    lastError = null;
    log("started app-server", reportedAppServer());
  } catch (error) {
    lastError = `${error.message}: ${output.join("").slice(-2_000)}`;
    child.kill("SIGTERM");
    throw new Error(lastError);
  }
}

async function stopAppServer() {
  closeAppServerTunnels();
  const instance = appServer;
  if (!instance?.child || instance.child.exitCode !== null) {
    appServer = null;
    return;
  }
  instance.child.kill("SIGTERM");
  await Promise.race([
    new Promise((resolve) => instance.child.once("exit", resolve)),
    new Promise((resolve) => setTimeout(resolve, 5_000)),
  ]);
  if (instance.child.exitCode === null) instance.child.kill("SIGKILL");
  appServer = null;
  log("stopped app-server");
}

function desiredIdentity(desired) {
  return JSON.stringify({
    listenUrl: desired.listenUrl ?? configuredListenUrl,
    codexPath: desired.codexPath ?? null,
    codexHome: desired.codexHome ?? configuredCodexHome,
    configOverrides: desired.configOverrides ?? configuredConfigOverrides,
  });
}

async function reconcile() {
  if (!desiredAppServer?.running) {
    await stopAppServer();
    return;
  }
  if (appServer?.child?.exitCode === null) {
    const current = {
      listenUrl: appServer.listenUrl,
      codexPath: appServer.codex.path,
      codexHome: appServer.codexHome,
      configOverrides: appServer.configOverrides,
    };
    const desired = JSON.parse(desiredIdentity(desiredAppServer));
    const selected = selectCodex(desiredAppServer);
    desired.codexPath = selected?.path ?? desired.codexPath;
    if (JSON.stringify(current) === JSON.stringify(desired)) return;
    await stopAppServer();
  }
  await startAppServer(desiredAppServer);
}

async function heartbeat() {
  const machineStatus = await capabilityRuntime.machineStatus();
  const response = await postJson(`/v1/nodes/${nodeId}/heartbeat`, "POST", {
    reportedAppServer: reportedAppServer(),
    codexInstallations,
    capabilities: capabilities(await detectNodeMode()),
    machineStatus,
  });
  desiredAppServer = response.desiredAppServer;
}

async function shutdown(signal) {
  if (stopping) return;
  stopping = true;
  log("shutting down node agent", { signal });
  controlSocket?.close(1000, "agent shutting down");
  closeAppServerTunnels();
  await capabilityRuntime.close();
  await stopAppServer();
  process.exit(0);
}

process.on("SIGINT", () => void shutdown("SIGINT"));
process.on("SIGTERM", () => void shutdown("SIGTERM"));

async function main() {
  await discoverCodex();
  const serverInterval = await register();
  const heartbeatSeconds = heartbeatOverride > 0 ? heartbeatOverride : serverInterval;
  while (!stopping) {
    try {
      await ensureControlChannel();
    } catch (error) {
      log("control channel connection failed", { error: error.message });
    }
    try {
      await reconcile();
    } catch (error) {
      lastError = error.message;
      log("node reconciliation failed", { error: error.message });
    }
    try {
      await heartbeat();
    } catch (error) {
      log("node heartbeat failed", { error: error.message });
    }
    await new Promise((resolve) => setTimeout(resolve, heartbeatSeconds * 1_000));
  }
}

main().catch((error) => {
  log("node agent failed", { error: error.stack ?? error.message });
  process.exit(1);
});

import process from "node:process";

import { AndroidAdbCapabilityRuntime, discoverAdbDevices } from "./android-adb-capabilities.mjs";

const agentVersion = "0.3.0";
const controlUrl = (process.env.CONTROL_SERVER_URL ?? "http://127.0.0.1:8787").replace(/\/$/, "");
const controlToken = process.env.CONTROL_SERVER_TOKEN ?? "local-poc-token";
const adbBinary = process.env.ADB_BINARY ?? "adb";
const heartbeatOverride = Number.parseInt(process.env.NODE_AGENT_HEARTBEAT_SECONDS ?? "0", 10);

let stopping = false;
let nodeId = null;
let controlSocket = null;
let runtime = null;
let device = null;
let lastError = null;

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

async function selectDevice() {
  const devices = await discoverAdbDevices(adbBinary);
  const configured = process.env.ANDROID_ADB_SERIAL;
  if (configured) {
    const match = devices.find((candidate) => candidate.serial === configured);
    if (!match) throw new Error(`configured ADB device is not present: ${configured}`);
    if (match.state !== "device") throw new Error(`configured ADB device ${configured} is ${match.state}`);
    return match;
  }
  const ready = devices.filter((candidate) => candidate.state === "device");
  if (ready.length !== 1) {
    throw new Error(
      `ANDROID_ADB_SERIAL is required when ${ready.length} ready ADB devices are connected`,
    );
  }
  return ready[0];
}

function capabilities() {
  return {
    appServer: false,
    shell: false,
    files: true,
    processes: true,
    pty: false,
    screen: true,
    input: true,
    reverseChannel: true,
    nativePaths: true,
    rootAvailable: runtime?.useRoot === true,
    nodeMode: "android-adb",
    transport: "adb",
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
      const result = await runtime.execute(message.capability, message.params);
      sendControl({ type: "response", requestId: message.requestId, ok: true, result });
    } catch (error) {
      lastError = error.message;
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
    sendControl({
      type: "appserver.error",
      sessionId: message.sessionId,
      error: "this Android ADB node does not run Codex App Server",
    });
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
  });
  await new Promise((resolve, reject) => {
    socket.addEventListener("open", resolve, { once: true });
    socket.addEventListener("error", reject, { once: true });
  });
  sendControl({ type: "hello", nodeId, agentVersion, protocolVersion: 1 });
  log("connected Android ADB reverse capability channel", { nodeId, serial: device.serial });
}

async function register() {
  const info = await runtime.deviceInfo();
  const hostname = `${info.manufacturer ?? "android"}-${info.model ?? info.device ?? device.serial}`;
  const nodeKey = process.env.NODE_AGENT_KEY ?? `android-adb:${device.serial}`;
  const response = await postJson("/v1/nodes/register", "POST", {
    nodeKey,
    hostname,
    platform: "android",
    architecture: info.abi ?? "unknown",
    nodeMode: "android-adb",
    agentVersion,
    capabilities: capabilities(),
    codexInstallations: [],
    defaultDesiredAppServer: { running: false, revision: 1 },
  });
  nodeId = response.nodeId;
  if (response.desiredAppServer?.running === true) {
    await postJson(`/v1/nodes/${nodeId}/desired-app-server`, "PUT", { running: false });
  }
  log("registered Android ADB node", { nodeId, nodeKey, device: info });
  return response.heartbeatIntervalSeconds;
}

async function heartbeat() {
  let machineStatus;
  try {
    machineStatus = await runtime.machineStatus();
    lastError = null;
  } catch (error) {
    lastError = error.message;
    machineStatus = {
      sampledAt: new Date().toISOString(),
      platform: "android",
      adb: { serial: device.serial, transport: "adb", connected: false },
      error: error.message,
    };
  }
  await postJson(`/v1/nodes/${nodeId}/heartbeat`, "POST", {
    reportedAppServer: { status: "unsupported", lastError },
    codexInstallations: [],
    capabilities: capabilities(),
    machineStatus,
  });
}

async function shutdown(signal) {
  if (stopping) return;
  stopping = true;
  log("shutting down Android ADB node agent", { signal });
  controlSocket?.close(1000, "agent shutting down");
  await runtime?.close();
  process.exit(0);
}

process.on("SIGINT", () => void shutdown("SIGINT"));
process.on("SIGTERM", () => void shutdown("SIGTERM"));

async function main() {
  device = await selectDevice();
  runtime = await new AndroidAdbCapabilityRuntime({
    serial: device.serial,
    adbBinary,
    useRoot: process.env.ANDROID_ADB_USE_ROOT === "true",
  }).initialize();
  const serverInterval = await register();
  const heartbeatSeconds = heartbeatOverride > 0 ? heartbeatOverride : serverInterval;
  await heartbeat();
  while (!stopping) {
    try {
      await ensureControlChannel();
    } catch (error) {
      lastError = error.message;
      log("Android ADB control channel connection failed", { error: error.message });
    }
    try {
      await heartbeat();
    } catch (error) {
      lastError = error.message;
      log("Android ADB heartbeat failed", { error: error.message });
    }
    await new Promise((resolve) => setTimeout(resolve, heartbeatSeconds * 1_000));
  }
}

main().catch((error) => {
  log("Android ADB node agent failed", { error: error.stack ?? error.message });
  process.exit(1);
});

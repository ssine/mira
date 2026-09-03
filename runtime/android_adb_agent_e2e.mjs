import fs from "node:fs/promises";
import path from "node:path";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import process from "node:process";

import { dynamicToolContentItems } from "../server/dynamic-tools.mjs";

const projectDirectory = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const controlUrl = (process.env.CONTROL_SERVER_URL ?? "http://127.0.0.1:8787").replace(/\/$/, "");
const token = process.env.CONTROL_SERVER_TOKEN ?? "local-poc-token";
const serial = process.env.ANDROID_ADB_SERIAL;
const nodeKey = `android-adb-e2e-${process.pid}`;
const screenshotPath =
  process.env.ANDROID_ADB_SCREENSHOT_OUTPUT ??
  path.join(projectDirectory, "runtime", "workspace", `android-adb-${process.pid}.png`);

if (!serial) throw new Error("ANDROID_ADB_SERIAL is required for the Android ADB E2E test");

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
      throw new Error(`Android ADB agent exited with ${child.exitCode} while waiting for ${description}`);
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

async function dynamicCall(tool, arguments_) {
  return (
    await jsonRequest("/v1/dynamic-tools/call", {
      method: "POST",
      body: JSON.stringify({ tool, arguments: arguments_ }),
    })
  ).result;
}

const child = spawn("node", ["node-agent/android-adb-agent.mjs"], {
  cwd: projectDirectory,
  env: {
    ...process.env,
    ANDROID_ADB_SERIAL: serial,
    ANDROID_ADB_ALLOWED_ROOTS:
      process.env.ANDROID_ADB_ALLOWED_ROOTS ?? JSON.stringify(["/sdcard", "/data/local/tmp"]),
    CONTROL_SERVER_URL: controlUrl,
    CONTROL_SERVER_TOKEN: token,
    NODE_AGENT_KEY: nodeKey,
    NODE_AGENT_HEARTBEAT_SECONDS: "1",
  },
  stdio: ["ignore", "pipe", "pipe"],
});
const output = [];
for (const stream of [child.stdout, child.stderr]) {
  stream.setEncoding("utf8");
  stream.on("data", (chunk) => output.push(chunk));
}

let testRoot;
try {
  const node = await waitFor(async () => {
    const candidate = await findNode();
    return candidate?.channelStatus?.connected && candidate.status === "online" ? candidate : null;
  }, "node registration and reverse channel");
  if (node.capabilities.appServer !== false || node.capabilities.screen !== true) {
    throw new Error(`unexpected Android capabilities: ${JSON.stringify(node.capabilities)}`);
  }

  const status = await dynamicCall("status", { action: "get", nodeId: node.nodeId });
  const display = await dynamicCall("screen", { action: "display", nodeId: node.nodeId });
  let wake = null;
  if (process.env.ANDROID_ADB_E2E_WAKE === "true") {
    wake = await dynamicCall("screen", {
      action: "key",
      nodeId: node.nodeId,
      keyCode: "KEYCODE_WAKEUP",
    });
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  const screenshot = await dynamicCall("screen", { action: "screenshot", nodeId: node.nodeId });
  const png = Buffer.from(screenshot.content, "base64");
  if (png.subarray(0, 8).toString("hex") !== "89504e470d0a1a0a") {
    throw new Error("screen tool did not return a PNG");
  }
  const contentItems = dynamicToolContentItems("screen", screenshot);
  if (
    contentItems[1]?.type !== "inputImage" ||
    !contentItems[1].imageUrl.startsWith("data:image/png;base64,")
  ) {
    throw new Error("screenshot was not converted to a dynamic-tool inputImage");
  }
  await fs.mkdir(path.dirname(screenshotPath), { recursive: true });
  await fs.writeFile(screenshotPath, png);

  const hierarchy = await dynamicCall("screen", { action: "hierarchy", nodeId: node.nodeId });
  if (!hierarchy.content.includes("<hierarchy")) throw new Error("UI hierarchy XML is missing");

  testRoot = `/data/local/tmp/codex-adb-e2e-${process.pid}`;
  const testPath = `${testRoot}/source.txt`;
  const movedPath = `${testRoot}/moved.txt`;
  await dynamicCall("file", {
    nodeId: node.nodeId,
    action: "mkdir",
    path: testRoot,
  });
  await dynamicCall("file", {
    nodeId: node.nodeId,
    action: "write",
    path: testPath,
    content: "android-file-ok\n",
    overwrite: false,
  });
  const file = await dynamicCall("file", {
    nodeId: node.nodeId,
    action: "read",
    path: testPath,
  });
  const stat = await dynamicCall("file", { nodeId: node.nodeId, action: "stat", path: testPath });
  await dynamicCall("file", {
    nodeId: node.nodeId,
    action: "move",
    path: testPath,
    destination: movedPath,
  });
  const listing = await dynamicCall("file", { nodeId: node.nodeId, action: "list", path: testRoot });
  await dynamicCall("file", {
    nodeId: node.nodeId,
    action: "remove",
    path: testRoot,
    recursive: true,
  });
  testRoot = null;

  const systemProcesses = await dynamicCall("process", {
    nodeId: node.nodeId,
    action: "list",
    system: true,
  });
  const started = await dynamicCall("process", {
    nodeId: node.nodeId,
    action: "start",
    command: "/system/bin/sh",
    args: ["-c", "printf android-process-ok"],
    cwd: "/data/local/tmp",
  });
  const finished = await waitFor(async () => {
    const value = await dynamicCall("process", {
      nodeId: node.nodeId,
      action: "poll",
      processId: started.processId,
      cursor: 0,
    });
    return value.running ? null : value;
  }, "managed Android process");
  const processOutput = finished.output.chunks.map((chunk) => chunk.text).join("");
  const longRunning = await dynamicCall("process", {
    nodeId: node.nodeId,
    action: "start",
    command: "/system/bin/sleep",
    args: ["30"],
    cwd: "/data/local/tmp",
  });
  const signaled = await dynamicCall("process", {
    nodeId: node.nodeId,
    action: "signal",
    processId: longRunning.processId,
    signal: "SIGTERM",
  });
  const stopped = await waitFor(async () => {
    const value = await dynamicCall("process", {
      nodeId: node.nodeId,
      action: "poll",
      processId: longRunning.processId,
      cursor: 0,
    });
    return value.running ? null : value;
  }, "signaled Android process");

  let tap = null;
  if (process.env.ANDROID_ADB_E2E_TAP === "true") {
    tap = await dynamicCall("screen", { action: "tap", nodeId: node.nodeId, x: 1, y: 1 });
  }

  console.log(
    JSON.stringify({
      ok: true,
      nodeId: node.nodeId,
      serial,
      model: status.device.model,
      androidRelease: status.device.release,
      appServerUnsupported: node.reportedAppServer.status === "unsupported",
      display: { width: display.width, height: display.height },
      wakeTested: wake?.accepted === true,
      screenshot: { path: screenshotPath, bytes: png.length, inputImage: true },
      hierarchyBytes: Buffer.byteLength(hierarchy.content),
      fileRoundTrip:
        file.content === "android-file-ok\n" &&
        stat.type === "file" &&
        listing.entries.some((entry) => entry.path === movedPath),
      systemProcessList: systemProcesses.output.includes("PID"),
      managedProcess: processOutput.includes("android-process-ok"),
      processSignal: signaled.accepted === true && stopped.running === false,
      tapTested: tap?.accepted === true,
    }),
  );
} catch (error) {
  console.error(
    JSON.stringify({ ok: false, error: error.message, output: output.join("").slice(-4_000) }),
  );
  process.exitCode = 1;
} finally {
  if (testRoot) {
    const node = await findNode().catch(() => null);
    if (node) {
      await dynamicCall("file", {
        nodeId: node.nodeId,
        action: "remove",
        path: testRoot,
        recursive: true,
      }).catch(() => {});
    }
  }
  if (child.exitCode === null) {
    const exited = new Promise((resolve) => child.once("exit", resolve));
    child.kill("SIGTERM");
    await exited;
  }
}

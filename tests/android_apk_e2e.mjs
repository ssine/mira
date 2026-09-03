import process from "node:process";

const controlUrl = (process.env.CONTROL_SERVER_URL ?? "http://127.0.0.1:8787").replace(/\/$/, "");
const token = process.env.MIRA_NODE_TOKEN ?? process.env.CONTROL_SERVER_TOKEN;
const nodeKey = process.env.ANDROID_APK_NODE_KEY;

if (!token) throw new Error("MIRA_NODE_TOKEN is required");
if (!nodeKey) throw new Error("ANDROID_APK_NODE_KEY is required");

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

async function dynamicCall(tool, arguments_) {
  return (
    await request("/v1/dynamic-tools/call", {
      method: "POST",
      body: JSON.stringify({ tool, arguments: arguments_ }),
    })
  ).result;
}

async function waitFor(predicate, description, timeout = 30_000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const result = await predicate();
    if (result) return result;
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`timed out waiting for ${description}`);
}

let testPath;
let node;
try {
  node = await waitFor(async () => {
    const candidate = (await request("/v1/nodes")).data.find((value) => value.nodeKey === nodeKey);
    return candidate?.status === "online" && candidate.channelStatus?.connected ? candidate : null;
  }, "Android APK node");
  const status = await dynamicCall("status", { action: "get", nodeId: node.nodeId });
  const isRoot = status.rootEnabled === true;
  const writableRoot = status.allowedRoots[0];
  testPath = `${writableRoot}/mira-apk-e2e-${process.pid}-${Date.now()}.txt`;
  await dynamicCall("file", {
    action: "write", nodeId: node.nodeId, path: testPath,
    content: "android-apk-file-ok\n", overwrite: false,
  });
  const file = await dynamicCall("file", {
    action: "read", nodeId: node.nodeId, path: testPath,
  });
  const processSession = await dynamicCall("process", {
    action: "start", nodeId: node.nodeId, command: "/system/bin/id", cwd: writableRoot,
  });
  const finished = await waitFor(async () => {
    const value = await dynamicCall("process", {
      action: "poll", nodeId: node.nodeId, processId: processSession.processId, cursor: 0,
    });
    return value.running ? null : value;
  }, "Android APK managed process");
  const identity = finished.output.chunks.map((chunk) => chunk.text).join("").trim();
  if (isRoot) {
    await dynamicCall("screen", {
      action: "key", nodeId: node.nodeId, keyCode: "KEYCODE_WAKEUP",
    });
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  const display = await dynamicCall("screen", { action: "display", nodeId: node.nodeId });
  const screenshot = await dynamicCall("screen", { action: "screenshot", nodeId: node.nodeId });
  const png = Buffer.from(screenshot.content, "base64");
  const hierarchy = await dynamicCall("screen", { action: "hierarchy", nodeId: node.nodeId });
  await dynamicCall("file", { action: "remove", nodeId: node.nodeId, path: testPath });
  testPath = null;

  const result = {
    ok:
      file.content === "android-apk-file-ok\n" &&
      png.subarray(0, 8).toString("hex") === "89504e470d0a1a0a" &&
      /<hierarchy(?:\s|>)/.test(hierarchy.content) &&
      (isRoot ? identity.includes("uid=0(root)") : !identity.includes("uid=0")),
    nodeId: node.nodeId,
    nodeKey,
    effectiveMode: isRoot ? "root" : "app",
    nativeIdentity: status.native,
    androidPermissions: status.androidPermissions,
    allowedRoots: status.allowedRoots,
    display: { width: display.width, height: display.height },
    screenshotBytes: png.length,
    hierarchyFormat: hierarchy.format,
    processIdentity: identity,
  };
  if (!result.ok) throw new Error(`Android APK verification failed: ${JSON.stringify(result)}`);
  console.log(JSON.stringify(result));
} finally {
  if (testPath && node) {
    await dynamicCall("file", {
      action: "remove", nodeId: node.nodeId, path: testPath,
    }).catch(() => {});
  }
}

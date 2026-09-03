import process from "node:process";

const controlUrl = (process.env.CONTROL_SERVER_URL ?? "http://127.0.0.1:8787").replace(
  /\/$/,
  "",
);
const token = process.env.MIRA_NODE_TOKEN ?? process.env.CONTROL_SERVER_TOKEN;
const nodeKey =
  process.env.MIRA_ANDROID_ROOT_NODE_KEY ??
  process.env.ANDROID_NATIVE_NODE_KEY ??
  "mira-android-root";

if (!token) throw new Error("MIRA_NODE_TOKEN is required");

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
    const value = await predicate();
    if (value) return value;
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`timed out waiting for ${description}`);
}

let testRoot;
let node;
try {
  node = await waitFor(async () => {
    const nodes = (await request("/v1/nodes")).data;
    const candidate = nodes.find((value) => value.nodeKey === nodeKey);
    return candidate?.status === "online" && candidate.channelStatus?.connected ? candidate : null;
  }, "root Android node and reverse channel");

  const status = await dynamicCall("status", { action: "get", nodeId: node.nodeId });
  if (status.native?.uid !== 0 || status.rootEnabled !== true) {
    throw new Error(`Android Mira Node is not root: ${JSON.stringify(status.native)}`);
  }

  const display = await dynamicCall("screen", { action: "display", nodeId: node.nodeId });
  const screenshot = await dynamicCall("screen", { action: "screenshot", nodeId: node.nodeId });
  const png = Buffer.from(screenshot.content, "base64");
  if (png.subarray(0, 8).toString("hex") !== "89504e470d0a1a0a") {
    throw new Error("screen tool did not return a PNG");
  }

  testRoot = `/data/mira-native-root-e2e-${process.pid}-${Date.now()}`;
  const testPath = `${testRoot}/root.txt`;
  await dynamicCall("file", { nodeId: node.nodeId, action: "mkdir", path: testRoot });
  await dynamicCall("file", {
    nodeId: node.nodeId,
    action: "write",
    path: testPath,
    content: "android-native-root-file-ok\n",
    overwrite: false,
  });
  const file = await dynamicCall("file", {
    nodeId: node.nodeId,
    action: "read",
    path: testPath,
  });

  const started = await dynamicCall("process", {
    nodeId: node.nodeId,
    action: "start",
    command: "/system/bin/id",
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
  }, "root managed process");
  const processOutput = finished.output.chunks.map((chunk) => chunk.text).join("");

  await dynamicCall("file", {
    nodeId: node.nodeId,
    action: "remove",
    path: testRoot,
    recursive: true,
  });
  testRoot = null;

  const result = {
    ok:
      file.content === "android-native-root-file-ok\n" &&
      processOutput.includes("uid=0(root)") &&
      finished.exitCode === 0,
    controlUrl,
    nodeId: node.nodeId,
    nodeKey,
    directReverseChannel: node.channelStatus.connected,
    nativeIdentity: status.native,
    display: { width: display.width, height: display.height },
    screenshotBytes: png.length,
    rootDataFileRoundTrip: file.content === "android-native-root-file-ok\n",
    rootManagedProcess: processOutput.trim(),
  };
  if (!result.ok) throw new Error(`root capability verification failed: ${JSON.stringify(result)}`);
  console.log(JSON.stringify(result));
} finally {
  if (testRoot && node) {
    await dynamicCall("file", {
      nodeId: node.nodeId,
      action: "remove",
      path: testRoot,
      recursive: true,
    }).catch(() => {});
  }
}

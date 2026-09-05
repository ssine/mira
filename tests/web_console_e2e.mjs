import { execFileSync, spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const projectDirectory = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const serverUrl = (process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787").replace(/\/$/, "");
const adminPassword = process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password";
const nodeKey = `web-console-e2e-${process.pid}`;
const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-web-console-e2e-"));
const identityFile = path.join(temporary, "identity.json");
const nodeBinary = path.join(temporary, "mira-node");
const fixtureName = "console-fixture.txt";
const fixturePath = path.join(temporary, fixtureName);
const fixtureContent = `Mira console file browser ${process.pid}\n`;
const processMarker = `MIRA_PROCESS_${process.pid}`;
const terminalMarker = `MIRA_PTY_${process.pid}`;
const codexHome = path.join(temporary, "codex-home");
const importedThreadId = randomUUID();
const importStoreId = `web-console-e2e-${process.pid}`;

execFileSync("go", ["build", "-o", nodeBinary, "./cmd/mira-node"], {
  cwd: path.join(projectDirectory, "node"),
});
await fs.writeFile(fixturePath, fixtureContent);
const sessionDirectory = path.join(codexHome, "sessions", "2026", "09", "04");
await fs.mkdir(sessionDirectory, { recursive: true });
const sessionPath = path.join(sessionDirectory, `rollout-2026-09-04-${importedThreadId}.jsonl`);
await fs.writeFile(sessionPath, [
  JSON.stringify({
    timestamp: "2026-09-04T01:02:03.000Z",
    type: "session_meta",
    payload: {
      id: importedThreadId,
      cwd: temporary,
      source: "cli",
      cli_version: "0.151.0",
      base_instructions: "Mira import fixture",
      history_mode: "paginated",
    },
  }),
  JSON.stringify({
    timestamp: "2026-09-04T01:02:04.000Z",
    type: "event_msg",
    payload: { type: "user_message", message: "Imported Mira session" },
  }),
  "",
].join("\n"));

let desktopFixture = null;
if (process.env.MIRA_REAL_DESKTOP_SESSION) {
  const source = process.env.MIRA_REAL_DESKTOP_SESSION;
  const file = await fs.open(source, "r");
  let meta;
  try {
    const buffer = Buffer.alloc(512 * 1024);
    const { bytesRead } = await file.read(buffer, 0, buffer.length, 0);
    const end = buffer.subarray(0, bytesRead).indexOf(10);
    meta = JSON.parse(buffer.subarray(0, end).toString()).payload;
  } finally { await file.close(); }
  const directory = path.join(codexHome, "archived_sessions");
  await fs.mkdir(directory, { recursive: true });
  desktopFixture = { id: meta.id, path: path.join(directory, path.basename(source)) };
  await fs.copyFile(source, desktopFixture.path);
  for (const ancestor of (process.env.MIRA_REAL_DESKTOP_ANCESTORS ?? "").split(";").filter(Boolean)) {
    await fs.copyFile(ancestor, path.join(directory, path.basename(ancestor)));
  }
}

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

async function fetchBody(pathname, options = {}, bodyType = "json") {
  const response = await fetch(`${serverUrl}${pathname}`, options);
  let body;
  if (bodyType === "text") {
    body = await response.text();
  } else {
    try { body = await response.json(); } catch { body = {}; }
  }
  return { response, body };
}

async function waitFor(operation, description, timeoutMs = 30_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const result = await operation();
    if (result) return result;
    await new Promise((resolve) => setTimeout(resolve, 150));
  }
  throw new Error(`timed out waiting for ${description}`);
}

const assets = {};
for (const [pathname, contentType] of [
  ["/", "text/html"],
  ["/app.js", "text/javascript"],
  ["/trace-activity.js", "text/javascript"],
  ["/theme.js", "text/javascript"],
  ["/styles.css", "text/css"],
  ["/vendor/xterm.js", "text/javascript"],
  ["/vendor/xterm-addon-fit.js", "text/javascript"],
  ["/vendor/xterm.css", "text/css"],
  ["/vendor/marked.js", "text/javascript"],
  ["/vendor/dompurify.js", "text/javascript"],
]) {
  const result = await fetchBody(pathname, {}, "text");
  assert(result.response.ok, `${pathname} returned HTTP ${result.response.status}`);
  assert(
    result.response.headers.get("content-type")?.startsWith(contentType),
    `${pathname} has an unexpected content type`,
  );
  assert(result.body.length > 100, `${pathname} returned an unexpectedly small asset`);
  assets[pathname] = result.body;
}
assert(assets["/"].includes("/app.js") && assets["/"].includes("/styles.css"), "website shell does not load its assets");
assert(assets["/"].includes("/vendor/xterm.css"), "website shell does not load xterm styles");
assert(assets["/"].includes("/theme.js"), "website shell does not load its CSP-safe theme initializer");
const shellResponse = await fetch(`${serverUrl}/`);
const contentSecurityPolicy = shellResponse.headers.get("content-security-policy") ?? "";
assert(contentSecurityPolicy.includes("img-src 'self' data: blob:") &&
  contentSecurityPolicy.includes("media-src 'self' blob:") &&
  contentSecurityPolicy.includes("frame-src 'self' blob:"),
"website CSP does not permit locally constructed Node file previews");
assert(assets["/app.js"].includes('from "/vendor/xterm.js"'), "website does not load the xterm terminal emulator");
assert(assets["/app.js"].includes('from "/vendor/marked.js"'), "website does not load the Markdown renderer");
assert(assets["/app.js"].includes('from "/vendor/dompurify.js"'), "website does not sanitize rendered Markdown");
assert(assets["/"].includes("loginForm"), "website does not expose the administrator login view");
for (const control of ["globalNav", "globalNodes", "globalAgent", "globalRuntime", "themeToggle"]) {
  assert(assets["/"].includes(`id="${control}"`), `website omitted Nexus-style shell control ${control}`);
}
for (const wiring of ["themeStorageKey", "function terminalTheme()", "function toggleTheme()", "function navigateGlobal("]) {
  assert(assets["/app.js"].includes(wiring), `website omitted shell behavior: ${wiring}`);
}
for (const control of ["installLinux", "installWindows", "installServer", "installAndroid"]) {
  assert(assets["/"].includes(`id="${control}"`), `website omitted installer ${control}`);
}
assert(assets["/app.js"].includes("/v1/dynamic-tools"), "website does not load the dynamic tool catalog");
assert(assets["/app.js"].includes("/v1/dynamic-tools/call"), "website does not expose the dynamic tool debugger call");
for (const wiring of [
  "async function refreshAdminCsrf()",
  'body.code === "invalid_csrf"',
  "return api(path, options, false)",
]) {
  assert(assets["/app.js"].includes(wiring), `website omitted CSRF recovery wiring: ${wiring}`);
}
for (const control of ["debugPresetFields", "debugAdvanced", "debugArguments"]) {
  assert(assets["/"].includes(`id="${control}"`), `website omitted friendly debugger control ${control}`);
}
for (const wiring of [
  "const debugFieldSets =",
  "function debugArgumentsPreset(",
  "function renderDebugPresetFields(",
  "function debugArgumentsFromPresetFields(",
  '$("#debugPresetFields").addEventListener("input", syncDebugJsonFromPresetFields)',
  '$("#debugPresetFields").addEventListener("change", syncDebugJsonFromPresetFields)',
  '$("#debugAdvanced").open ? JSON.parse($("#debugArguments").value) : debugArgumentsFromPresetFields()',
]) {
  assert(assets["/app.js"].includes(wiring), `website omitted friendly debugger wiring: ${wiring}`);
}
for (const control of ["workspaceView", "fileRootSelect", "terminalOutput", "systemProcessCount", "memoryResource", "diskResources"]) {
  assert(assets["/"].includes(control), `website omitted workbench control ${control}`);
}
for (const control of ["agentView", "runtimeView", "agentThreadDrawer", "agentThreadDrawerToggle", "agentHome", "agentRuntimeNode", "agentRuntimeDefaultCwd", "agentRuntimeSaveCwd", "agentThreadList", "sessionSourceNode", "localSessionList", "conversationScroll", "conversationTrace", "conversationForm", "conversationAttach", "conversationFileInput", "conversationAttachments", "nodeFileDialog", "nodeFileDownload"]) {
  assert(assets["/"].includes(control), `website omitted Agent console control ${control}`);
}
for (const route of ["/v1/codex/threads", "/transcript?${query}", "/codex-sessions", "/codex-session-imports", "/v1/codex/runtimes/"]) {
  assert(assets["/app.js"].includes(route), `website omitted Agent console route ${route}`);
}
for (const wiring of ["transcriptPageSize = 60", "loadOlderAgentTranscript", "traceNearBottom", "scrollTraceToBottom", "preserveViewport", "data-load-older", "reconcilePendingUserTrace", "ensureToolGroup", "updateToolGroup", "emptyNarrative", "projectedThread?.cwd", "desiredAppServer?.defaultCwd", "saveAgentRuntimeDefaultCwd", "notificationIsForOpenThread", "activeTurns", "turnThreads", "resolveNodeFileReference", "decorateTraceFileReferences", "readNodeFile", "openNodeFile", "prepareTurnInput", "addComposerFiles", "dataset.nodeFilePath", "type: \"localImage\"", "file.slice(offset, offset + nodeFileChunkBytes)", "?storeId=personal", "sendPromise", "syncConversationSendUi", "miraRequestId", "newThreadRequestId"]) {
  assert(assets["/app.js"].includes(wiring), `website omitted paginated conversation wiring: ${wiring}`);
}
assert(!assets["/app.js"].includes('$("#conversationSend").disabled = false'),
  "App Server connect can re-enable Send while a submission is still in flight");
assert(assets["/app.js"].includes('sandbox: "danger-full-access"') &&
  !assets["/app.js"].includes('sandbox: "workspace-write"'),
"website does not start Codex in the default unrestricted sandbox mode");
assert(!assets["/app.js"].includes('"Turn", "Codex 正在处理…", "运行中"'),
  "website still renders normal Turn lifecycle notifications as transcript cards");
const compactCss = assets["/styles.css"].replace(/\s+/g, "");
const rule = (selector) => compactCss.split(`${selector}{`)[1]?.split("}")[0] ?? "";
assert(compactCss.includes(".trace-card{flex:00auto") &&
  compactCss.includes(".history-loader{flex:00auto") &&
  rule(".conversation-scroll").includes("min-height:0;") &&
  rule(".conversation-scroll").includes("overflow-y:auto;") &&
  !/overflow(?:-y)?:(?:auto|scroll)/.test(rule(".conversation-trace")) &&
  !/\.trace-body\{[^}]*max-height/.test(compactCss),
"website conversation cards may shrink, nest scrollbars, or omit the history loader");
assert(compactCss.includes(".tool-group{") && compactCss.includes(".tool-group-items{"),
  "website omitted collapsed tool-call groups");
assert(compactCss.includes('grid-template-areas:"conversation-head""conversation-notice""conversation-scroll""conversation-composer"'),
  "website conversation rows can shift when the optional notice is hidden");
assert(["height:100dvh;", "min-height:0;", "overflow:hidden;"].every((declaration) =>
  rule('body[data-view="agentView"]').includes(declaration)) &&
  ["position:relative;", "display:grid;", "width:100%;", "height:100%;", "min-height:0;", "overflow:hidden;"].every((declaration) =>
    rule(".chat-shell").includes(declaration)),
"dedicated conversation shell can escape the viewport or leave document-level blank space");
for (const operation of ['invoke("file"', 'invoke("process"', 'invoke("pty"']) {
  assert(assets["/app.js"].includes(operation), `website omitted ${operation} integration`);
}

const login = await fetchBody("/v1/admin/login", {
  method: "POST",
  headers: { "content-type": "application/json" },
  body: JSON.stringify({ username: "admin", password: adminPassword }),
});
assert(login.response.ok, `administrator login failed: ${login.response.status} ${JSON.stringify(login.body)}`);
const session = {
  cookie: login.response.headers.get("set-cookie")?.split(";", 1)[0],
  csrf: login.body.csrfToken,
};
assert(session.cookie && session.csrf, "administrator login returned an incomplete session");

async function admin(pathname, options = {}) {
  return fetchBody(pathname, {
    ...options,
    headers: {
      cookie: session.cookie,
      "x-mira-csrf": session.csrf,
      ...(options.body ? { "content-type": "application/json" } : {}),
      ...(options.headers ?? {}),
    },
  });
}

async function invoke(nodeId, capability, params, options = {}) {
  const result = await admin(`/v1/nodes/${nodeId}/invoke`, {
    method: "POST",
    body: JSON.stringify({ capability, params, timeoutMs: options.timeoutMs }),
  });
  if (!result.response.ok) {
    throw new Error(`${capability}.${params.action ?? "get"} failed: ${result.response.status} ${JSON.stringify(result.body)}`);
  }
  return result.body.result;
}

async function dynamicCall(tool, arguments_, options = {}) {
  const result = await admin("/v1/dynamic-tools/call", {
    method: "POST",
    body: JSON.stringify({ tool, arguments: arguments_, timeoutMs: options.timeoutMs }),
  });
  if (!result.response.ok) {
    throw new Error(`dynamic tool ${tool} failed: ${result.response.status} ${JSON.stringify(result.body)}`);
  }
  return result.body.result;
}

const toolCatalog = await admin("/v1/dynamic-tools");
assert(toolCatalog.response.ok, `dynamic tool catalog failed: ${toolCatalog.response.status}`);
const namespace = toolCatalog.body.dynamicTools?.find((item) => item.name === "home_nodes");
assert(namespace?.type === "namespace" && Array.isArray(namespace.tools), "home_nodes dynamic tool namespace is missing");
const tools = new Map(namespace.tools.map((tool) => [tool.name, tool]));
assert(
  JSON.stringify([...tools.keys()].sort()) === JSON.stringify(["file", "process", "pty", "screen", "status"]),
  `dynamic tool catalog differs from the final tool list: ${JSON.stringify([...tools.keys()])}`,
);
for (const [name, tool] of tools) {
  assert(tool.type === "function", `${name} is not a function tool`);
  assert(tool.inputSchema?.type === "object", `${name} omitted its object input schema`);
  assert(tool.inputSchema.additionalProperties === false, `${name} input schema allows unknown properties`);
  assert(tool.inputSchema.properties?.action, `${name} input schema omitted action`);
}
const expectedActions = {
  status: ["list", "get"],
  file: ["roots", "stat", "list", "read", "write", "mkdir", "move", "remove"],
  process: ["count", "list", "start", "poll", "signal"],
  pty: ["open", "write", "poll", "resize", "close", "list"],
  screen: ["display", "screenshot", "hierarchy", "tap", "swipe", "key", "text"],
};
const fieldSetSource = assets["/app.js"].slice(
  assets["/app.js"].indexOf("const debugFieldSets ="),
  assets["/app.js"].indexOf("const debugFieldDefinitions ="),
);
for (const [name, actions] of Object.entries(expectedActions)) {
  const schema = tools.get(name).inputSchema;
  assert(
    JSON.stringify(schema.properties.action.enum) === JSON.stringify(actions),
    `${name} action schema differs from the final contract`,
  );
  const expectedRequired = name === "status" ? ["action"] : ["nodeId", "action"];
  assert(
    expectedRequired.every((property) => schema.required?.includes(property)),
    `${name} input schema omitted required properties`,
  );
  assert(fieldSetSource.includes(`${name}: {`), `${name} does not expose preset form fields`);
  for (const action of actions) {
    assert(new RegExp(`\\b${action}:`).test(fieldSetSource), `${name}.${action} does not have an action preset`);
  }
}
assert(tools.get("file").inputSchema.properties.length.maximum === 4 * 1024 * 1024, "file read limit is missing from its schema");
assert(tools.get("process").inputSchema.properties.args.maxItems === 128, "process argument limit is missing from its schema");
assert(tools.get("pty").inputSchema.properties.rows.maximum === 500, "PTY row limit is missing from its schema");

const missingCsrf = await fetchBody("/v1/nodes/00000000-0000-4000-8000-000000000001/invoke", {
  method: "POST",
  headers: { cookie: session.cookie, "content-type": "application/json" },
  body: JSON.stringify({ capability: "status", params: {} }),
});
assert(missingCsrf.response.status === 403 && missingCsrf.body.code === "invalid_csrf", "administrator invoke did not enforce CSRF");
const dynamicMissingCsrf = await fetchBody("/v1/dynamic-tools/call", {
  method: "POST",
  headers: { cookie: session.cookie, "content-type": "application/json" },
  body: JSON.stringify({ tool: "status", arguments: { action: "list" } }),
});
assert(dynamicMissingCsrf.response.status === 403 && dynamicMissingCsrf.body.code === "invalid_csrf", "dynamic tool debugger did not enforce CSRF");

const child = spawn(nodeBinary, [], {
  cwd: temporary,
  env: {
    ...process.env,
    MIRA_SERVER_URL: serverUrl,
    MIRA_NODE_KEY: nodeKey,
    MIRA_IDENTITY_FILE: identityFile,
    MIRA_NODE_ALLOWED_ROOTS: JSON.stringify([temporary]),
    CODEX_HOME: codexHome,
    HOME: temporary,
    MIRA_NODE_HEARTBEAT_SECONDS: "1",
    APP_SERVER_AUTO_START: "false",
    MIRA_NODE_TOKEN: "",
    CONTROL_SERVER_TOKEN: "",
  },
  stdio: ["ignore", "pipe", "pipe"],
});
const logs = [];
for (const stream of [child.stdout, child.stderr]) {
  stream.setEncoding("utf8");
  stream.on("data", (chunk) => logs.push(chunk));
}

let nodeId = null;
let managedProcessId = null;
let terminalSessionId = null;
try {
  const pending = await waitFor(async () => {
    if (child.exitCode !== null) throw new Error(`mira-node exited: ${logs.join("").slice(-2000)}`);
    const result = await admin("/v1/admin/enrollments?status=pending");
    assert(result.response.ok, `enrollment list failed: ${result.response.status}`);
    return result.body.data?.find((item) => item.nodeKey === nodeKey);
  }, "Node enrollment");

  const approvalWithoutCsrf = await fetchBody(`/v1/admin/enrollments/${pending.enrollmentId}/approve`, {
    method: "POST",
    headers: { cookie: session.cookie, "content-type": "application/json" },
    body: "{}",
  });
  assert(approvalWithoutCsrf.response.status === 403, "enrollment approval did not enforce CSRF");
  const approval = await admin(`/v1/admin/enrollments/${pending.enrollmentId}/approve`, {
    method: "POST",
    body: "{}",
  });
  assert(approval.response.ok && approval.body.status === "approved", `approval failed: ${JSON.stringify(approval.body)}`);

  const online = await waitFor(async () => {
    if (child.exitCode !== null) throw new Error(`mira-node exited: ${logs.join("").slice(-2000)}`);
    const result = await admin("/v1/nodes");
    assert(result.response.ok, `Node list failed: ${result.response.status}`);
    return result.body.data?.find((node) => node.nodeKey === nodeKey && node.channelStatus?.connected === true);
  }, "approved Node reverse channel");
  nodeId = online.nodeId;

  const configuredDefaultCwd = await admin(`/v1/nodes/${nodeId}/desired-app-server`, {
    method: "PUT",
    body: JSON.stringify({ running: false, defaultCwd: temporary }),
  });
  assert(configuredDefaultCwd.response.ok && configuredDefaultCwd.body.desiredAppServer?.defaultCwd === temporary,
    `default working directory was not persisted: ${JSON.stringify(configuredDefaultCwd.body)}`);
  const relativeDefaultCwd = await admin(`/v1/nodes/${nodeId}/desired-app-server`, {
    method: "PUT",
    body: JSON.stringify({ running: false, defaultCwd: "relative/path" }),
  });
  assert(relativeDefaultCwd.response.status === 400 && relativeDefaultCwd.body.code === "invalid_request",
    "relative default working directory was accepted");
  const configuredNode = await admin(`/v1/nodes/${nodeId}`);
  assert(configuredNode.body.desiredAppServer?.defaultCwd === temporary,
    "default working directory did not survive the rejected update");

  const scanned = await admin(`/v1/nodes/${nodeId}/codex-sessions`);
  assert(scanned.response.ok, `Codex session scan failed: ${scanned.response.status} ${JSON.stringify(scanned.body)}`);
  const discovered = scanned.body.sessions?.find((item) => item.threadId === importedThreadId);
  assert(discovered?.path === sessionPath, "Codex session discovery omitted the default CODEX_HOME fixture");
  assert(discovered.title === "Imported Mira session", "Codex session discovery returned an incorrect title");
  const imported = await admin(`/v1/nodes/${nodeId}/codex-session-imports`, {
    method: "POST",
    body: JSON.stringify({ path: sessionPath, storeId: importStoreId }),
  });
  assert(imported.response.ok && imported.body.threadId === importedThreadId,
    `Codex session import failed: ${JSON.stringify(imported.body)}`);
  assert(imported.body.itemCount === 2, "Codex session import did not preserve every rollout record");
  const streamed = await fetchBody(`/v1/nodes/${nodeId}/codex-session-imports`, {
    method: "POST", headers: { accept: "application/x-ndjson", "content-type": "application/json", cookie: session.cookie, "x-mira-csrf": session.csrf },
    body: JSON.stringify({ path: sessionPath, storeId: importStoreId }),
  }, "text");
  const importEvents = streamed.body.trim().split("\n").map((line) => JSON.parse(line));
  assert(importEvents.some((event) => event.type === "progress" && event.phase === "reading") &&
    importEvents.at(-1).type === "complete", "streamed import omitted progress/completion");
  const threads = await admin(`/v1/codex/threads?storeId=${encodeURIComponent(importStoreId)}`);
  const importedProjection = threads.body.data?.find((item) => item.threadId === importedThreadId);
  assert(importedProjection?.title === "Imported Mira session" && importedProjection.itemCount === 2,
    "unified Codex thread projection omitted the imported session");
  assert(importedProjection.cwd === temporary,
    "unified Codex thread projection did not preserve the imported working directory");
  assert(importedProjection.runtimeNodeId === (discovered.suggestedRuntimeNodeId ?? null) &&
    importedProjection.sourceNodeId === nodeId,
    "imported thread did not preserve its suggested execution node and original storage node");
  const transcript = await admin(
    `/v1/codex/threads/${importedThreadId}/transcript?storeId=${encodeURIComponent(importStoreId)}`,
  );
  assert(transcript.response.ok, `Codex transcript projection failed: ${JSON.stringify(transcript.body)}`);
  assert(transcript.body.itemCount === 2 && transcript.body.trace?.length === 1,
    "Codex transcript projection omitted the imported history");
  assert(transcript.body.nextCursor === null && transcript.body.totalTraceItems === 1,
    "Codex transcript projection omitted pagination metadata");
  assert(transcript.body.trace[0].kind === "user" && transcript.body.trace[0].body === "Imported Mira session",
    "Codex transcript projection returned an incorrect user message");
  const nodeIdentity = JSON.parse(await fs.readFile(identityFile, "utf8"));
  if (desktopFixture) {
    const found = scanned.body.sessions.find((item) => item.threadId === desktopFixture.id);
    assert(found?.clientKind === "desktop" && found.archived, "real desktop source/archived discovery failed");
    const desktopStoreId = `desktop-resume-${process.pid}`;
    const desktop = await admin(`/v1/nodes/${nodeId}/codex-session-imports`, {
      method: "POST", body: JSON.stringify({ path: desktopFixture.path, storeId: desktopStoreId }),
    });
    assert(desktop.response.ok, "real desktop import failed");
    const resumeHome = path.join(temporary, "resume-home");
    await fs.mkdir(resumeHome);
    const output = execFileSync("python3", [path.join(projectDirectory, "tests/desktop_resume_e2e.py")], {
      cwd: projectDirectory, timeout: 150_000, encoding: "utf8",
      env: { ...process.env, MIRA_NODE_TOKEN: nodeIdentity.token,
        MIRA_DESKTOP_TEST_CODEX_HOME: resumeHome, MIRA_DESKTOP_TEST_STORE_ID: desktopStoreId,
        MIRA_DESKTOP_TEST_THREAD_ID: desktopFixture.id, MIRA_SERVER_URL: serverUrl },
    });
    assert(output.includes('"desktopResume": true'), "real App Server did not resume the desktop thread");
    console.log(output.trim());
  }
  const storeHeaders = { authorization: `Bearer ${nodeIdentity.token}`, "x-mira-client-type": "codex" };
  const cancelledStore = `cancel-http-${process.pid}`;
  const cancelController = new AbortController();
  // Stall only our disposable Node so abort reliably reaches the Server before
  // the scan completes, then let the queued capability finish normally.
  child.kill("SIGSTOP");
  try {
    const transfer = await fetch(`${serverUrl}/v1/nodes/${nodeId}/codex-session-imports`, {
      method: "POST", signal: cancelController.signal,
      headers: { accept: "application/x-ndjson", "content-type": "application/json", cookie: session.cookie, "x-mira-csrf": session.csrf },
      body: JSON.stringify({ path: sessionPath, storeId: cancelledStore }),
    });
    const reader = transfer.body.getReader();
    const first = await reader.read();
    assert(new TextDecoder().decode(first.value).includes('"scanning"'), "import progress did not arrive before scan finished");
    cancelController.abort();
  } finally { child.kill("SIGCONT"); }
  await new Promise((resolve) => setTimeout(resolve, 1500));
  const cancelledHead = await fetchBody(`/v2/stores/${cancelledStore}`, { headers: storeHeaders });
  assert(cancelledHead.response.ok && cancelledHead.body.version === 0, "HTTP cancellation published a partial thread");
  const importedHead = await fetchBody(`/v2/stores/${encodeURIComponent(importStoreId)}`, { headers: storeHeaders });
  assert(importedHead.response.ok, `imported store head failed: ${JSON.stringify(importedHead.body)}`);
  assert(importedHead.body.state?.created_threads?.[importedThreadId]?.history_mode === "legacy",
    "paginated local session was not adapted to the remote ThreadStore history mode");
  const importedManifest = importedHead.body.historyManifest?.[importedThreadId];
  const importedHistory = await fetchBody(
    `/v2/stores/${encodeURIComponent(importStoreId)}/threads/${importedThreadId}/history?generation=${importedManifest.generation}`,
    { headers: storeHeaders },
  );
  assert(importedHistory.response.ok, `imported history failed: ${JSON.stringify(importedHistory.body)}`);
  assert(importedHistory.body.items?.[0]?.payload?.history_mode === "legacy",
    "imported session metadata did not use the remote ThreadStore history mode");
  const preFixItems = structuredClone(importedHistory.body.items);
  preFixItems[0].payload.history_mode = "paginated";
  const preFixState = await fetchBody(`/v2/stores/${encodeURIComponent(importStoreId)}/commits`, {
    method: "POST",
    headers: { ...storeHeaders, "content-type": "application/json", "x-codex-operation-id": randomUUID() },
    body: JSON.stringify({
      expectedVersion: importedHead.body.version,
      stateChanges: [{
        path: ["created_threads", importedThreadId, "history_mode"], mode: "set",
        conflictPolicy: "compareAndSwap", expected: { exists: true, value: "legacy" }, value: "paginated",
      }],
      historyChanges: [{
        threadId: importedThreadId, mode: "replace",
        expectedGeneration: importedManifest.generation, expectedItemCount: importedManifest.itemCount,
        items: preFixItems,
      }],
    }),
  });
  assert(preFixState.response.ok, `failed to create pre-fix import fixture: ${JSON.stringify(preFixState.body)}`);
  const duplicateImport = await admin(`/v1/nodes/${nodeId}/codex-session-imports`, {
    method: "POST",
    body: JSON.stringify({ path: sessionPath, storeId: importStoreId }),
  });
  assert(duplicateImport.response.ok && duplicateImport.body.duplicate === true,
    "Codex session import was not idempotent");
  const repairedHead = await fetchBody(`/v2/stores/${encodeURIComponent(importStoreId)}`, { headers: storeHeaders });
  const repairedManifest = repairedHead.body.historyManifest?.[importedThreadId];
  const repairedHistory = await fetchBody(
    `/v2/stores/${encodeURIComponent(importStoreId)}/threads/${importedThreadId}/history?generation=${repairedManifest.generation}`,
    { headers: storeHeaders },
  );
  assert(repairedHead.body.state?.created_threads?.[importedThreadId]?.history_mode === "legacy" &&
    repairedHistory.body.items?.[0]?.payload?.history_mode === "legacy",
  "re-import did not repair a pre-fix paginated ThreadStore adaptation");

  // The workbench's aggregate state is assembled from the trusted Node record and
  // the live status capability, using the same administrator session as the UI.
  const status = await invoke(nodeId, "status", {});
  assert(status.hostname === online.hostname, "live status did not match the registered Node");
  assert(Number.isInteger(status.processCount) && status.processCount > 0, "status omitted the system process count");
  assert(Number.isInteger(status.managedProcesses), "status omitted the managed process count");
  assert(Number.isInteger(status.ptySessions), "status omitted the terminal session count");
  assert(Number.isInteger(status.cpu?.logicalCount) && status.cpu.logicalCount > 0, "status omitted CPU configuration");
  assert(status.memory?.totalBytes > 0 && status.memory?.usedBytes >= 0, "status omitted memory usage");
  assert(typeof status.memory?.usagePercent === "number", "status omitted memory utilization");
  assert(status.disk?.some((disk) => disk.totalBytes > 0 && typeof disk.usagePercent === "number"), "status omitted disk usage");
  const debugStatus = await dynamicCall("status", { action: "get", nodeId });
  assert(debugStatus.hostname === online.hostname, "dynamic tool debugger returned incorrect status");

  const roots = await invoke(nodeId, "file", { action: "roots" });
  assert(roots.roots?.some((root) => root.configured === temporary), "file browser did not expose the configured root");
  const directory = await invoke(nodeId, "file", { action: "list", path: temporary });
  assert(directory.entries?.some((entry) => path.basename(entry.path) === fixtureName && entry.type === "file"), "file browser did not list the fixture");
  const file = await invoke(nodeId, "file", { action: "read", path: fixturePath });
  assert(file.encoding === "utf8" && file.content === fixtureContent && file.eof === true, "file browser read returned incorrect content");
  const debugFile = await dynamicCall("file", { action: "read", nodeId, path: fixturePath });
  assert(debugFile.encoding === "utf8" && debugFile.content === fixtureContent, "dynamic tool debugger returned incorrect file content");

  const countedProcesses = await invoke(nodeId, "process", { action: "count" });
  assert(
    Number.isInteger(countedProcesses.processCount) && countedProcesses.processCount > 0,
    "system process count was empty",
  );
  assert(countedProcesses.systemVisible === countedProcesses.processCount, "system-visible count disagreed with processCount");
  assert(Number.isInteger(countedProcesses.managedRunning), "process count omitted managedRunning");
  assert(Number.isInteger(countedProcesses.managedRetained), "process count omitted managedRetained");
  assert(typeof countedProcesses.sampledAt === "string", "system process count omitted its sample time");

  const started = await invoke(nodeId, "process", {
    action: "start",
    command: "/bin/sh",
    args: ["-c", `printf ${processMarker}; sleep 30`],
    cwd: temporary,
  });
  managedProcessId = started.processId;
  assert(managedProcessId, "process start did not return a processId");
  const managed = await invoke(nodeId, "process", { action: "list", cursor: 0 });
  assert(managed.processes?.some((item) => item.processId === managedProcessId), "managed process list omitted the started process");
  assert(managed.processes.length >= 1, "managed process count did not include the started process");

  const opened = await invoke(nodeId, "pty", {
    action: "open",
    command: "/bin/sh",
    cwd: temporary,
    rows: 28,
    cols: 100,
  });
  terminalSessionId = opened.sessionId;
  assert(terminalSessionId && opened.running === true, "interactive terminal did not open");
  const written = await invoke(nodeId, "pty", {
    action: "write",
    sessionId: terminalSessionId,
    input: `printf '${terminalMarker}\\n'\n`,
  });
  assert(written.bytesWritten > 0, "interactive terminal did not accept input");
  const terminal = await waitFor(async () => {
    const value = await invoke(nodeId, "pty", { action: "poll", sessionId: terminalSessionId, cursor: 0 });
    const output = value.output?.chunks?.map((chunk) => chunk.text).join("") ?? "";
    return output.includes(terminalMarker) ? { value, output } : null;
  }, "interactive terminal output");
  assert(terminal.value.output.cursor > 0, "interactive terminal cursor did not advance");
  const closed = await invoke(nodeId, "pty", { action: "close", sessionId: terminalSessionId });
  assert(closed.closed === true, "interactive terminal did not close");
  terminalSessionId = null;

  const stopped = await invoke(nodeId, "process", {
    action: "signal",
    processId: managedProcessId,
    signal: "SIGTERM",
  });
  assert(stopped.accepted === true, "managed process did not accept SIGTERM");
  managedProcessId = null;

  console.log(JSON.stringify({
    ok: true,
    adminSession: true,
    csrfEnforced: true,
    websiteAssets: true,
    dynamicToolCatalog: [...tools.keys()],
    dynamicToolSchemas: true,
    dynamicToolDebugger: true,
    friendlyDebuggerForm: true,
    nodeApproval: true,
    nodeDefaultCwd: true,
    aggregateStatus: true,
    machineConfiguration: true,
    resourceUsage: true,
    fileBrowser: true,
    systemProcessCount: countedProcesses.processCount,
    managedProcessList: true,
    interactiveTerminal: true,
    codexSessionDiscovery: true,
    codexSessionImport: true,
    codexSessionImportIdempotent: true,
    nodeId,
  }));
} catch (error) {
  console.error(JSON.stringify({ ok: false, error: error.message, logs: logs.join("").slice(-4000) }));
  process.exitCode = 1;
} finally {
  if (nodeId && terminalSessionId) {
    try { await invoke(nodeId, "pty", { action: "close", sessionId: terminalSessionId }); } catch { /* Node shutdown also closes it. */ }
  }
  if (nodeId && managedProcessId) {
    try { await invoke(nodeId, "process", { action: "signal", processId: managedProcessId, signal: "SIGTERM" }); } catch { /* Node shutdown also closes it. */ }
  }
  if (child.exitCode === null) {
    const exited = new Promise((resolve) => child.once("exit", resolve));
    child.kill("SIGTERM");
    await exited;
  }
  await fs.rm(temporary, { recursive: true, force: true });
}

import { FitAddon } from "/vendor/xterm-addon-fit.js";
import { Terminal } from "/vendor/xterm.js";
import DOMPurify from "/vendor/dompurify.js";
import { marked } from "/vendor/marked.js";
import { toolItemView, activitySummary, summarizeActivities, activityStatus, formatActivityDuration, reasoningText, reasoningParts, reasoningHeading } from "/trace-activity.js";

marked.setOptions({ gfm: true, breaks: false });

const $ = (selector) => document.querySelector(selector);
let csrfToken = null;
let csrfRefreshPromise = null;
let dashboardNodes = new Map();

const workspace = {
  node: null,
  status: null,
  roots: [],
  currentRoot: null,
  currentPath: null,
  sessionId: null,
  ptyResizeSupported: false,
  cursor: 0,
  pollTimer: null,
  pollBusy: false,
  terminal: null,
  fitAddon: null,
  terminalDataDisposable: null,
  terminalResizeDisposable: null,
  terminalResizeObserver: null,
  terminalResizeFrame: null,
  terminalResizeTimer: null,
  inputBuffer: "",
  inputFlushTimer: null,
  inputWriteQueue: Promise.resolve(),
  dynamicNamespace: null,
  dynamicTools: [],
  selectedDynamicTool: null,
};

const agent = {
  socket: null,
  socketNodeId: null,
  pending: new Map(),
  requestId: 0,
  threadId: null,
  turnId: null,
  activeTurns: new Map(),
  turnThreads: new Map(),
  sessions: [],
  threads: [],
  transcriptThreadId: null,
  transcriptGeneration: null,
  transcriptItems: [],
  transcriptCursor: null,
  transcriptTotal: 0,
  transcriptLoadingOlder: false,
  threadRuntimeNodeId: null,
  previousRuntimeNodeId: null,
  attachments: [],
  fileObjectUrl: null,
  sendPromise: null,
  newThreadRequestId: null,
  newThreadRequestSignature: null,
};

const transcriptPageSize = 60;
const maximumAttachmentBytes = 4 * 1024 * 1024;
const maximumAttachmentTotalBytes = 8 * 1024 * 1024;
const maximumConversationFileBytes = 128 * 1024 * 1024;
const nodeFileChunkBytes = 4 * 1024 * 1024;

async function refreshAdminCsrf() {
  if (!csrfRefreshPromise) {
    csrfRefreshPromise = (async () => {
      const response = await fetch("/v1/admin/session", { credentials: "same-origin" });
      let body = {};
      try { body = await response.json(); } catch { /* empty response */ }
      if (!response.ok || typeof body.csrfToken !== "string") {
        const error = new Error(body.error ?? "管理员会话已经失效，请重新登录");
        error.status = response.status;
        error.code = body.code;
        throw error;
      }
      csrfToken = body.csrfToken;
      return csrfToken;
    })().finally(() => { csrfRefreshPromise = null; });
  }
  return csrfRefreshPromise;
}

async function api(path, options = {}, retryCsrf = true) {
  const headers = { ...(options.body ? { "content-type": "application/json" } : {}), ...(options.headers ?? {}) };
  const mutation = !["GET", "HEAD"].includes(options.method ?? "GET");
  if (csrfToken && mutation) headers["x-mira-csrf"] = csrfToken;
  const response = await fetch(path, { credentials: "same-origin", ...options, headers });
  let body = {};
  try { body = await response.json(); } catch { /* empty response */ }
  if (!response.ok && response.status === 403 && body.code === "invalid_csrf" && mutation && retryCsrf) {
    await refreshAdminCsrf();
    return api(path, options, false);
  }
  if (!response.ok) {
    const error = new Error(body.error ?? `HTTP ${response.status}`);
    error.status = response.status;
    error.code = body.code;
    throw error;
  }
  return body;
}

function element(tag, className, text) {
  const value = document.createElement(tag);
  if (className) value.className = className;
  if (text !== undefined) value.textContent = String(text);
  return value;
}

function clear(value) {
  value.replaceChildren();
  return value;
}

function show(view) {
  for (const id of ["loginView", "setupView", "dashboardView", "workspaceView", "agentView"]) {
    $("#" + id).classList.toggle("hidden", id !== view);
  }
  $("#logoutButton").classList.toggle("hidden", !["dashboardView", "workspaceView", "agentView"].includes(view));
}

function toast(message) {
  $("#toast").textContent = message;
  $("#toast").classList.remove("hidden");
  setTimeout(() => $("#toast").classList.add("hidden"), 2800);
}

function when(value) {
  return value ? new Intl.DateTimeFormat("zh-CN", { dateStyle: "short", timeStyle: "medium" }).format(new Date(value)) : "—";
}

function formatBytes(value) {
  const amount = Number(value);
  if (!Number.isFinite(amount) || amount < 0) return "—";
  if (amount < 1024) return `${amount} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let scaled = amount / 1024;
  let unit = units[0];
  for (let index = 1; index < units.length && scaled >= 1024; index += 1) {
    scaled /= 1024;
    unit = units[index];
  }
  return `${scaled >= 10 ? scaled.toFixed(0) : scaled.toFixed(1)} ${unit}`;
}

function formatUptime(seconds) {
  const value = Math.max(0, Number(seconds) || 0);
  const days = Math.floor(value / 86400);
  const hours = Math.floor((value % 86400) / 3600);
  const minutes = Math.floor((value % 3600) / 60);
  if (days) return `${days} 天 ${hours} 小时`;
  if (hours) return `${hours} 小时 ${minutes} 分`;
  return `${minutes} 分钟`;
}

function capabilityEnabled(node, name) {
  return node?.capabilities?.[name] === true;
}

function actionButton(label, action, id, className) {
  const button = element("button", className, label);
  button.type = "button";
  button.dataset.action = action;
  button.dataset.id = id;
  return button;
}

function renderEnrollments(items) {
  const list = clear($("#enrollmentList"));
  if (!items.length) {
    list.append(element("div", "empty", "目前没有待审批的设备"));
    return;
  }
  for (const item of items) {
    const article = element("article", "enrollment");
    const identity = element("div");
    identity.append(element("div", "title", item.hostname), element("div", "sub", item.nodeKey));
    const code = element("div");
    code.append(element("div", "sub", "验证码"), element("div", "code", item.verificationCode));
    const platform = element("div");
    platform.append(element("div", "sub", "平台"), element("div", "", `${item.platform ?? "—"} · ${item.architecture ?? "—"}`));
    const source = element("div");
    source.append(element("div", "sub", "来源 / 到期"), element("div", "", item.requestedFrom ?? "未知"), element("div", "sub", when(item.expiresAt)));
    const actions = element("div", "actions");
    actions.append(actionButton("批准", "approve", item.enrollmentId, "approve"), actionButton("拒绝", "reject", item.enrollmentId, "danger"));
    article.append(identity, code, platform, source, actions);
    list.append(article);
  }
}

function renderNodes(nodes) {
  const grid = clear($("#nodeGrid"));
  if (!nodes.length) {
    grid.append(element("div", "empty", "还没有已知设备。启动 mira-node 来提交第一台设备。"));
    return;
  }
  for (const node of nodes) {
    const card = element("article", "node-card");
    const top = element("div", "node-top");
    const identity = element("div");
    identity.append(element("div", "title", node.hostname), element("div", "sub", `${node.nodeKey ?? "—"} · ${node.nodeId}`));
    top.append(identity, element("span", `badge ${node.status ?? "offline"}`, node.status ?? "offline"));

    const metadata = element("div", "node-meta");
    for (const [label, value] of [["系统", `${node.platform ?? "—"} / ${node.architecture ?? "—"}`], ["Node", node.nodeVersion ?? "—"], ["最后在线", when(node.lastSeenAt)]]) {
      const cell = element("div");
      cell.append(element("small", "", label), element("span", "", value));
      metadata.append(cell);
    }
    const capabilities = element("div", "capabilities");
    const enabled = Object.entries(node.capabilities ?? {}).filter(([, value]) => value === true).map(([name]) => name);
    for (const name of enabled.length ? enabled : ["未上报能力"]) capabilities.append(element("span", "", name));

    const actions = element("div", "node-actions");
    if (node.approvalStatus === "approved") {
      actions.append(actionButton("打开工作台", "workspace", node.nodeId, "approve"));
      actions.append(actionButton("撤销设备", "revoke", node.nodeId, "danger"));
    }
    card.append(top, metadata, capabilities, actions);
    grid.append(card);
  }
}

function renderAudit(events) {
  const rows = clear($("#auditRows"));
  for (const event of events) {
    const row = document.createElement("tr");
    const values = [when(event.createdAt), event.action, event.clientType ?? event.actorType ?? "system", event.targetNodeId ?? event.metadata?.nodeKey ?? "—"];
    for (const value of values) row.append(element("td", "", value));
    row.append(element("td", event.success ? "result-ok" : "result-fail", event.success ? "成功" : event.errorCode ?? "失败"));
    rows.append(row);
  }
}

async function loadDashboard() {
  const includeRevoked = $("#showRevoked").checked;
  const [enrollments, nodes, audit] = await Promise.all([
    api("/v1/admin/enrollments?status=pending"),
    api(`/v1/nodes?includeRevoked=${includeRevoked}`),
    api("/v1/admin/audit-events?limit=30"),
  ]);
  const pending = enrollments.data ?? [];
  const allNodes = nodes.data ?? [];
  dashboardNodes = new Map(allNodes.map((node) => [node.nodeId, node]));
  $("#pendingCount").textContent = pending.length;
  $("#onlineCount").textContent = allNodes.filter((node) => node.status === "online").length;
  $("#approvedCount").textContent = allNodes.filter((node) => node.approvalStatus === "approved").length;
  renderEnrollments(pending);
  renderNodes(allNodes);
  renderAudit(audit.data ?? []);
}

async function decide(id, action) {
  await api(`/v1/admin/enrollments/${id}/${action}`, { method: "POST", body: "{}" });
  toast(action === "approve" ? "设备已批准" : "申请已拒绝");
  await loadDashboard();
}

async function revoke(id) {
  if (!confirm("撤销后该设备的 HTTP 与 WebSocket 凭证会立即失效。继续吗？")) return;
  await api(`/v1/admin/nodes/${id}/revoke`, { method: "POST", body: JSON.stringify({ reason: "revoked from admin console" }) });
  toast("设备已撤销；历史和审计记录已保留");
  await loadDashboard();
}

async function invokeNode(nodeId, capability, params, timeoutMs = 30000) {
  if (!nodeId) throw new Error("尚未选择节点");
  const response = await api(`/v1/nodes/${nodeId}/invoke`, {
    method: "POST",
    body: JSON.stringify({ capability, params, timeoutMs }),
  });
  return response.result;
}

async function invoke(capability, params, timeoutMs = 30000) {
  if (!workspace.node) throw new Error("尚未选择节点");
  return invokeNode(workspace.node.nodeId, capability, params, timeoutMs);
}

async function callDynamicTool(tool, arguments_, timeoutMs = 30000) {
  const response = await api("/v1/dynamic-tools/call", {
    method: "POST",
    body: JSON.stringify({ tool, arguments: arguments_, timeoutMs }),
  });
  return response.result;
}

function setWorkspaceNotice(message = "", kind = "") {
  const notice = $("#workspaceNotice");
  notice.textContent = message;
  notice.className = `workspace-notice${message ? "" : " hidden"}${kind ? ` ${kind}` : ""}`;
}

function renderWorkspaceHeader() {
  const node = workspace.node;
  $("#workspaceTitle").textContent = node?.hostname ?? "设备工作台";
  $("#workspaceSubtitle").textContent = node ? `${node.nodeKey ?? "—"} · ${node.platform ?? "—"}/${node.architecture ?? "—"} · ${node.nodeId}` : "";
  $("#workspaceStatus").textContent = node?.status ?? "offline";
  $("#workspaceStatus").className = `badge ${node?.status ?? "offline"}`;
  if (node?.status !== "online") setWorkspaceNotice("此节点当前离线。文件、进程和 Shell 工具将在节点重新连接后可用。", "warning");
  else setWorkspaceNotice();
}

function renderCapabilities() {
  const target = clear($("#workspaceCapabilities"));
  const entries = Object.entries(workspace.node?.capabilities ?? {}).filter(([, value]) => value === true);
  if (!entries.length) target.append(element("span", "", "未上报能力"));
  for (const [name] of entries) target.append(element("span", "", name));
}

function dynamicToolAvailable(name) {
  if (name === "status") return true;
  if (name === "file") return capabilityEnabled(workspace.node, "files");
  if (name === "process") return capabilityEnabled(workspace.node, "processes");
  if (name === "pty") return capabilityEnabled(workspace.node, "pty");
  if (name === "screen") return capabilityEnabled(workspace.node, "screen") || capabilityEnabled(workspace.node, "input");
  return false;
}

function dynamicToolActions(tool) {
  const values = tool?.inputSchema?.properties?.action?.enum;
  return Array.isArray(values) ? values : [];
}

const debugFieldSets = {
  file: {
    roots: [], stat: ["path"], list: ["path"], read: ["path", "offset", "length"],
    write: ["path", "content", "encoding", "overwrite"], mkdir: ["path"],
    move: ["path", "destination", "overwrite"], remove: ["path", "recursive"],
  },
  process: {
    count: [], list: ["cursor", "system"], start: ["command", "args", "cwd", "env"],
    poll: ["processId", "cursor"], signal: ["processId", "signal"],
  },
  pty: {
    list: ["cursor"], open: ["command", "args", "cwd", "rows", "cols"],
    write: ["sessionId", "input"], poll: ["sessionId", "cursor"],
    resize: ["sessionId", "rows", "cols"], close: ["sessionId"],
  },
  screen: {
    display: [], screenshot: [], hierarchy: [], tap: ["x", "y"],
    swipe: ["startX", "startY", "endX", "endY", "durationMs"], key: ["keyCode"], text: ["text"],
  },
  status: { list: [], get: [] },
};

const debugActionLabels = {
  status: { list: "列出全部节点", get: "读取当前节点" },
  file: { roots: "查看文件入口", stat: "查看文件信息", list: "列出目录", read: "读取文件", write: "写入文件", mkdir: "新建目录", move: "移动/改名", remove: "删除" },
  process: { count: "统计进程", list: "列出进程", start: "启动进程", poll: "读取输出", signal: "发送信号" },
  pty: { list: "列出会话", open: "打开终端", write: "发送输入", poll: "读取输出", resize: "调整窗口", close: "关闭终端" },
  screen: { display: "显示参数", screenshot: "截取屏幕", hierarchy: "界面层级", tap: "点击", swipe: "滑动", key: "按键", text: "输入文字" },
};

const debugFieldDefinitions = {
  path: { label: "路径", type: "text", placeholder: "绝对路径" },
  destination: { label: "目标路径", type: "text", placeholder: "移动后的绝对路径" },
  content: { label: "文件内容", type: "textarea", placeholder: "要写入的 UTF-8 文本或 Base64" },
  encoding: { label: "内容编码", type: "select", options: ["utf8", "base64"] },
  offset: { label: "读取偏移", type: "number", minimum: 0 },
  length: { label: "读取长度", type: "number", minimum: 1, maximum: 4194304 },
  recursive: { label: "递归删除", type: "checkbox" },
  overwrite: { label: "允许覆盖", type: "checkbox" },
  processId: { label: "Process ID", type: "text", placeholder: "先由 process.start/list 获得" },
  command: { label: "命令", type: "text", placeholder: "留空时 PTY 使用节点默认 Shell" },
  args: { label: "命令参数", type: "lines", placeholder: "每行一个参数" },
  cwd: { label: "工作目录", type: "text", placeholder: "绝对路径" },
  env: { label: "环境变量", type: "json", placeholder: "{\n  \"NAME\": \"value\"\n}" },
  cursor: { label: "输出游标", type: "number", minimum: 0 },
  signal: { label: "信号", type: "select", options: ["SIGINT", "SIGTERM", "SIGKILL"] },
  system: { label: "列出系统进程", type: "checkbox" },
  sessionId: { label: "Session ID", type: "text", placeholder: "先由 pty.open/list 获得" },
  input: { label: "终端输入", type: "textarea", placeholder: "发送到 PTY 的原始输入" },
  rows: { label: "终端行数", type: "number", minimum: 1, maximum: 500 },
  cols: { label: "终端列数", type: "number", minimum: 1, maximum: 1000 },
  x: { label: "X 坐标", type: "number", minimum: 0 }, y: { label: "Y 坐标", type: "number", minimum: 0 },
  startX: { label: "起点 X", type: "number", minimum: 0 }, startY: { label: "起点 Y", type: "number", minimum: 0 },
  endX: { label: "终点 X", type: "number", minimum: 0 }, endY: { label: "终点 Y", type: "number", minimum: 0 },
  durationMs: { label: "滑动时间（ms）", type: "number", minimum: 1, maximum: 60000 },
  keyCode: { label: "Android KeyCode", type: "text", placeholder: "KEYCODE_HOME 或整数" },
  text: { label: "输入文字", type: "textarea", placeholder: "发送到当前 Android 输入焦点" },
};

function debugPath() {
  return workspace.currentPath ?? workspace.status?.allowedRoots?.[0] ?? workspace.node?.machineStatus?.allowedRoots?.[0]
    ?? (workspace.node?.platform === "windows" ? "C:\\" : "/");
}

function debugArgumentsPreset(toolName, action) {
  const nodeId = workspace.node?.nodeId;
  if (toolName === "status") return action === "list" ? { action: "list" } : { action: "get", nodeId };
  const value = { nodeId, action };
  const path = debugPath();
  if (toolName === "file" && action !== "roots") value.path = path;
  if (toolName === "file" && action === "read") Object.assign(value, { offset: 0, length: 65536 });
  if (toolName === "file" && action === "write") Object.assign(value, { path: joinPath(path, "mira-debug.txt"), content: "Mira capability debugger\n", encoding: "utf8", overwrite: false });
  if (toolName === "file" && action === "mkdir") value.path = joinPath(path, "mira-debug-directory");
  if (toolName === "file" && action === "move") value.destination = `${path}${path.endsWith("/") || path.endsWith("\\") ? "" : "/"}mira-debug-destination`;
  if (toolName === "file" && action === "remove") value.recursive = false;
  if (toolName === "process" && ["list", "poll"].includes(action)) value.cursor = 0;
  if (toolName === "process" && action === "start") Object.assign(value, workspace.node?.platform === "windows"
    ? { command: "cmd.exe", args: ["/c", "echo Mira capability debugger"], cwd: path }
    : { command: "/bin/sh", args: ["-c", "printf 'Mira capability debugger\\n'"], cwd: path });
  if (toolName === "process" && ["poll", "signal"].includes(action)) value.processId = "<processId>";
  if (toolName === "process" && action === "signal") value.signal = "SIGTERM";
  if (toolName === "pty" && ["list", "poll"].includes(action)) value.cursor = 0;
  if (toolName === "pty" && action === "open") Object.assign(value, { cwd: path, rows: 30, cols: 120 });
  if (toolName === "pty" && ["write", "poll", "resize", "close"].includes(action)) value.sessionId = "<sessionId>";
  if (toolName === "pty" && action === "write") value.input = "";
  if (toolName === "pty" && action === "resize") Object.assign(value, { rows: 30, cols: 120 });
  if (toolName === "screen" && action === "tap") Object.assign(value, { x: 0, y: 0 });
  if (toolName === "screen" && action === "swipe") Object.assign(value, { startX: 0, startY: 0, endX: 0, endY: 0, durationMs: 300 });
  if (toolName === "screen" && action === "key") value.keyCode = "KEYCODE_HOME";
  if (toolName === "screen" && action === "text") value.text = "";
  return value;
}

function debugFieldInput(name, definition, value) {
  let input;
  if (definition.type === "textarea" || definition.type === "lines" || definition.type === "json") {
    input = document.createElement("textarea");
    input.rows = definition.type === "textarea" ? 4 : 3;
    if (definition.type === "lines") input.value = Array.isArray(value) ? value.join("\n") : "";
    else if (definition.type === "json") input.value = value && typeof value === "object" ? JSON.stringify(value, null, 2) : "";
    else input.value = value ?? "";
  } else if (definition.type === "select") {
    input = document.createElement("select");
    for (const optionValue of definition.options) {
      const option = element("option", "", optionValue);
      option.value = optionValue;
      input.append(option);
    }
    input.value = value ?? definition.options[0];
  } else {
    input = document.createElement("input");
    input.type = definition.type;
    if (definition.type === "checkbox") input.checked = Boolean(value);
    else input.value = value ?? "";
  }
  input.dataset.debugParam = name;
  input.dataset.debugType = definition.type;
  if (definition.placeholder) input.placeholder = definition.placeholder;
  if (definition.minimum !== undefined) input.min = String(definition.minimum);
  if (definition.maximum !== undefined) input.max = String(definition.maximum);
  return input;
}

function renderDebugPresetFields(tool, action, preset) {
  const target = clear($("#debugPresetFields"));
  const fields = debugFieldSets[tool.name]?.[action] ?? [];
  if (!fields.length) {
    const explanation = tool.name === "status" && action === "list"
      ? "无需参数：列出 Agent 当前可发现的全部节点。"
      : `无需额外参数：将直接对 ${workspace.node?.hostname ?? "当前节点"} 执行 ${tool.name}.${action}。`;
    target.append(element("p", "debug-no-params", explanation));
    return;
  }
  for (const name of fields) {
    const definition = debugFieldDefinitions[name];
    if (!definition) continue;
    const label = element("label", `debug-preset-field${definition.type === "textarea" || definition.type === "lines" || definition.type === "json" ? " wide" : ""}`);
    if (definition.type === "checkbox") {
      label.classList.add("checkbox");
      label.append(debugFieldInput(name, definition, preset[name]), element("span", "", definition.label));
    } else {
      label.append(element("span", "", definition.label), debugFieldInput(name, definition, preset[name]));
    }
    target.append(label);
  }
}

function debugArgumentsFromPresetFields() {
  const tool = workspace.selectedDynamicTool;
  if (!tool) throw new Error("尚未选择工具");
  const action = document.querySelector("[data-debug-action].active")?.dataset.debugAction ?? dynamicToolActions(tool)[0];
  const result = tool.name === "status" && action === "list"
    ? { action }
    : { nodeId: workspace.node?.nodeId, action };
  for (const input of document.querySelectorAll("[data-debug-param]")) {
    const name = input.dataset.debugParam;
    const type = input.dataset.debugType;
    if (type === "checkbox") result[name] = input.checked;
    else if (type === "number") {
      if (input.value !== "") result[name] = Number(input.value);
    } else if (type === "lines") {
      result[name] = input.value.split(/\r?\n/).filter((line) => line.length > 0);
    } else if (type === "json") {
      if (input.value.trim()) {
        try { result[name] = JSON.parse(input.value); }
        catch { throw new Error(`${debugFieldDefinitions[name].label} 必须是有效 JSON`); }
      }
    } else if (name === "keyCode" && /^\d+$/.test(input.value.trim())) result[name] = Number(input.value);
    else result[name] = input.value;
  }
  return result;
}

function syncDebugJsonFromPresetFields() {
  try {
    $("#debugArguments").value = JSON.stringify(debugArgumentsFromPresetFields(), null, 2);
    $("#debugPresetFields").classList.remove("invalid");
  } catch {
    $("#debugPresetFields").classList.add("invalid");
  }
}

function renderDebugResult(result, toolName, arguments_, elapsedMs) {
  const meta = $("#debugResultMeta");
  const output = $("#debugResult");
  const image = $("#debugResultImage");
  image.classList.add("hidden");
  image.removeAttribute("src");
  let display = result;
  if (toolName === "screen" && arguments_.action === "screenshot" && result?.mimeType === "image/png" && result?.encoding === "base64" && typeof result.content === "string") {
    image.src = `data:image/png;base64,${result.content}`;
    image.classList.remove("hidden");
    display = { ...result, content: `<base64 image omitted · ${result.content.length} characters>` };
  }
  meta.textContent = `成功 · ${Math.round(elapsedMs)} ms · ${new Date().toLocaleTimeString("zh-CN")}`;
  meta.className = "debug-result-meta result-ok";
  output.textContent = JSON.stringify(display, null, 2);
}

function selectedDebugAction(tool) {
  try {
    const parsed = JSON.parse($("#debugArguments").value);
    if (typeof parsed.action === "string" && dynamicToolActions(tool).includes(parsed.action)) return parsed.action;
  } catch { /* The editor may temporarily contain invalid JSON. */ }
  return dynamicToolActions(tool)[0];
}

function selectDynamicTool(name, action = null) {
  const tool = workspace.dynamicTools.find((item) => item.name === name);
  if (!tool) return;
  workspace.selectedDynamicTool = tool;
  const available = dynamicToolAvailable(tool.name);
  for (const button of document.querySelectorAll("[data-debug-tool]")) button.classList.toggle("active", button.dataset.debugTool === name);
  $("#debugToolTitle").textContent = `${workspace.dynamicNamespace?.name ?? "home_nodes"}.${tool.name}`;
  $("#debugToolDescription").textContent = tool.description ?? "没有工具说明";
  $("#debugToolSchema").textContent = JSON.stringify(tool.inputSchema ?? {}, null, 2);
  $("#debugToolAvailability").textContent = available ? "当前节点可调用" : "当前节点未提供";
  $("#debugToolAvailability").className = `debug-availability ${available ? "available" : "unavailable"}`;
  const actions = dynamicToolActions(tool);
  const selectedAction = action ?? actions[0];
  const actionsTarget = clear($("#debugActions"));
  for (const actionName of actions) {
    const button = element("button", `debug-action${actionName === selectedAction ? " active" : ""}`, debugActionLabels[tool.name]?.[actionName] ?? actionName);
    button.type = "button";
    button.dataset.debugAction = actionName;
    button.title = actionName;
    actionsTarget.append(button);
  }
  const preset = debugArgumentsPreset(tool.name, selectedAction);
  renderDebugPresetFields(tool, selectedAction, preset);
  $("#debugArguments").value = JSON.stringify(preset, null, 2);
  $("#debugAdvanced").open = false;
  $("#debugCallHint").textContent = available
    ? "目标已锁定为当前节点；需要非常规参数时可展开高级 JSON 模式。"
    : "工具定义仍会注入 Agent，但当前节点没有声明此能力；实际调用将返回 capability_unavailable。";
}

function renderDynamicTools() {
  const namespace = workspace.dynamicNamespace;
  $("#debugNamespace").textContent = namespace?.name ?? "—";
  $("#debugToolCount").textContent = `${workspace.dynamicTools.length} 个 Agent 工具 · schema 来自 Server`;
  const target = clear($("#debugToolList"));
  for (const tool of workspace.dynamicTools) {
    const available = dynamicToolAvailable(tool.name);
    const button = element("button", `debug-tool${workspace.selectedDynamicTool?.name === tool.name ? " active" : ""}`);
    button.type = "button";
    button.dataset.debugTool = tool.name;
    const title = element("span", "debug-tool-title");
    title.append(element("i", available ? "available" : "unavailable"), element("strong", "", tool.name));
    button.append(title, element("small", "", dynamicToolActions(tool).join(" · ") || "无 action"));
    target.append(button);
  }
  if (!workspace.dynamicTools.length) {
    target.append(element("div", "empty", "Server 没有返回 dynamicTools"));
    return;
  }
  selectDynamicTool(workspace.selectedDynamicTool?.name ?? workspace.dynamicTools[0].name);
}

async function loadDynamicTools(force = false) {
  if (!workspace.dynamicNamespace || force) {
    const result = await api("/v1/dynamic-tools");
    workspace.dynamicNamespace = (result.dynamicTools ?? []).find((item) => item.type === "namespace" && Array.isArray(item.tools)) ?? null;
    workspace.dynamicTools = workspace.dynamicNamespace?.tools ?? [];
    workspace.selectedDynamicTool = null;
  }
  renderDynamicTools();
}

function addFact(target, label, value) {
  target.append(element("dt", "", label), element("dd", "", value ?? "—"));
}

function percent(used, total) {
  const numerator = Number(used);
  const denominator = Number(total);
  if (!Number.isFinite(numerator) || !Number.isFinite(denominator) || denominator <= 0) return null;
  return Math.max(0, Math.min(100, numerator / denominator * 100));
}

function resourceMeter(label, used, total, detail) {
  const wrapper = element("div", "resource-meter");
  const head = element("div", "resource-meter-head");
  head.append(element("span", "resource-label", label), element("span", "resource-value", detail));
  const track = element("div", "meter-track");
  const fill = element("span", "meter-fill");
  const usage = percent(used, total);
  fill.style.width = `${usage ?? 0}%`;
  if (usage !== null && usage >= 90) fill.classList.add("critical");
  else if (usage !== null && usage >= 75) fill.classList.add("warning");
  track.append(fill);
  wrapper.append(head, track);
  return wrapper;
}

function renderResources(status) {
  const memoryTarget = clear($("#memoryResource"));
  const memory = status.memory ?? {};
  const memoryTotal = Number(memory.totalBytes);
  const memoryAvailable = Number(memory.availableBytes ?? memory.freeBytes);
  if (memoryTotal > 0 && Number.isFinite(memoryAvailable)) {
    const memoryUsed = Number.isFinite(Number(memory.usedBytes)) ? Number(memory.usedBytes) : Math.max(0, memoryTotal - memoryAvailable);
    const usage = Number.isFinite(Number(memory.usagePercent)) ? Number(memory.usagePercent) : percent(memoryUsed, memoryTotal);
    memoryTarget.append(resourceMeter("内存", memoryUsed, memoryTotal, `已用 ${formatBytes(memoryUsed)} / 总量 ${formatBytes(memoryTotal)} · ${usage?.toFixed(0) ?? "—"}%`));
  } else {
    memoryTarget.append(element("p", "muted compact", "节点未报告内存数据"));
  }

  const disksTarget = clear($("#diskResources"));
  const disks = Array.isArray(status.disk) ? status.disk : [];
  if (!disks.length) disksTarget.append(element("p", "muted compact", "节点未报告磁盘数据"));
  for (const disk of disks) {
    if (disk.error) {
      disksTarget.append(element("p", "muted compact", `${disk.path ?? "磁盘"} · ${disk.error}`));
      continue;
    }
    const total = Number(disk.totalBytes);
    const available = Number(disk.availableBytes ?? disk.freeBytes);
    const used = Number.isFinite(Number(disk.usedBytes)) ? Number(disk.usedBytes) : total > 0 && Number.isFinite(available) ? Math.max(0, total - available) : null;
    const usage = Number.isFinite(Number(disk.usagePercent)) ? Number(disk.usagePercent) : percent(used, total);
    const detail = used === null
      ? "容量不可用"
      : `已用 ${formatBytes(used)} / 容量 ${formatBytes(total)} · ${usage?.toFixed(0) ?? "—"}% · 可用 ${formatBytes(available)}`;
    disksTarget.append(resourceMeter(`文件系统 ${disk.path ?? "—"}`, used, total, detail));
  }
}

function renderNetworks(status) {
  const target = clear($("#networkInterfaces"));
  const networks = status.networks;
  if (typeof networks === "string") {
    const lines = networks.split(/\r?\n/).map((line) => line.trim()).filter(Boolean);
    if (!lines.length) target.append(element("p", "muted compact", "节点未报告网络接口"));
    for (const line of lines) target.append(element("code", "network-line", line));
    return;
  }
  if (!networks || typeof networks !== "object" || Array.isArray(networks)) {
    target.append(element("p", "muted compact", "节点未报告网络接口"));
    return;
  }
  const entries = Object.entries(networks);
  if (!entries.length) target.append(element("p", "muted compact", "节点未报告网络接口"));
  for (const [name, network] of entries) {
    const item = element("details", "network-interface");
    const addresses = Array.isArray(network?.addresses) ? network.addresses : [];
    const summary = element("summary");
    summary.append(element("strong", "", name), element("span", "", addresses[0] ?? network?.flags ?? "无地址"));
    item.append(summary);
    const values = element("div", "network-addresses");
    for (const address of addresses) values.append(element("code", "", address));
    if (network?.hardwareAddress) values.append(element("small", "", `MAC ${network.hardwareAddress}`));
    if (network?.flags) values.append(element("small", "", network.flags));
    item.append(values);
    target.append(item);
  }
}

function renderStatus(status) {
  workspace.status = status;
  $("#nodeCpuCount").textContent = status.cpu?.logicalCount ?? status.cpuCount ?? "—";
  $("#nodeMemoryAvailable").textContent = formatBytes(status.memory?.availableBytes ?? status.memory?.freeBytes);
  $("#nodeMemoryHint").textContent = status.memory?.totalBytes ? `总计 ${formatBytes(status.memory.totalBytes)}` : "节点未报告内存";
  $("#nodeUptime").textContent = formatUptime(status.uptimeSeconds);
  $("#nodeRuntimeHint").textContent = `${status.managedProcesses ?? 0} 个 Mira 进程 · ${status.ptySessions ?? 0} 个 PTY`;
  const cpuUsage = status.cpu?.usagePercent ?? status.cpuUtilizationPercent ?? status.cpuUsagePercent ?? status.cpu?.utilizationPercent;
  const loadAverage = status.cpu?.loadAverage ?? status.loadAverage;
  const load = Array.isArray(loadAverage)
    ? loadAverage.join(" / ")
    : loadAverage && typeof loadAverage === "object"
      ? [loadAverage.one, loadAverage.five, loadAverage.fifteen].filter((value) => value !== undefined).join(" / ")
      : loadAverage;
  $("#nodeLoadHint").textContent = [cpuUsage === undefined ? null : `CPU ${Number(cpuUsage).toFixed(1)}%`, load === undefined ? null : `load ${load}`, `采样 ${when(status.sampledAt)}`].filter(Boolean).join(" · ");
  const facts = clear($("#nodeFacts"));
  addFact(facts, "主机名", status.hostname ?? workspace.node.hostname);
  addFact(facts, "Node Key", workspace.node.nodeKey);
  addFact(facts, "Node ID", workspace.node.nodeId);
  addFact(facts, "操作系统", `${status.platform ?? workspace.node.platform ?? "—"} ${status.release ?? ""}`.trim());
  addFact(facts, "架构", status.architecture ?? workspace.node.architecture);
  addFact(facts, "运行模式", workspace.node.nodeMode ?? workspace.node.capabilities?.nodeMode);
  addFact(facts, "Node 版本", workspace.node.nodeVersion);
  addFact(facts, "构建提交", workspace.node.nodeBuild?.commit ?? "unknown");
  addFact(facts, "节点协议", workspace.node.nodeBuild?.protocolVersion ?? workspace.node.channelStatus?.protocolVersion ?? "—");
  const logicalCount = status.cpu?.logicalCount ?? status.cpuCount;
  addFact(facts, "CPU", [status.cpu?.model, logicalCount ? `${logicalCount} 核` : null].filter(Boolean).join(" · ") || "—");
  addFact(facts, "PTY 后端", status.ptyBackend ?? (capabilityEnabled(workspace.node, "pty") ? "已启用" : "不可用"));
  addFact(facts, "Root 能力", String(status.rootEnabled ?? workspace.node.capabilities?.rootAvailable ?? false));
  addFact(facts, "允许根目录", Array.isArray(status.allowedRoots) ? status.allowedRoots.join(" · ") : "—");
  renderResources(status);
  renderNetworks(status);
}

function renderProcessCount(result, fallback = {}) {
  const count = result?.systemVisible ?? result?.processCount ?? fallback.processCount;
  $("#systemProcessCount").textContent = Number.isFinite(Number(count)) ? Number(count) : "—";
  const managedRunning = result?.managedRunning;
  $("#processCountHint").textContent = managedRunning === undefined
    ? `节点最近于 ${when(result?.sampledAt ?? fallback.sampledAt)} 采样`
    : `${managedRunning} 个由 Mira 启动的进程正在运行`;
}

async function loadOverview() {
  const nodeId = workspace.node?.nodeId;
  const fallbackStatus = {
    ...(workspace.node?.machineStatus ?? {}),
    hostname: workspace.node?.machineStatus?.hostname ?? workspace.node?.hostname,
    platform: workspace.node?.machineStatus?.platform ?? workspace.node?.platform,
    architecture: workspace.node?.machineStatus?.architecture ?? workspace.node?.architecture,
  };
  renderStatus(fallbackStatus);
  $("#systemProcessCount").textContent = "…";
  $("#processCountHint").textContent = "正在读取系统进程数";
  renderCapabilities();
  if (!nodeId || workspace.node.status !== "online") {
    renderProcessCount(null, fallbackStatus);
    $("#processCountHint").textContent = workspace.node?.machineStatus?.processCount === undefined ? "节点离线" : "节点离线 · 显示最后一次心跳采样";
    return;
  }
  const statusTask = invoke("status", {});
  const processTask = capabilityEnabled(workspace.node, "processes")
    ? invoke("process", { action: "count" })
    : Promise.reject(new Error("节点未提供进程能力"));
  const [statusResult, processResult] = await Promise.allSettled([statusTask, processTask]);
  if (workspace.node?.nodeId !== nodeId) return;
  if (statusResult.status === "fulfilled") renderStatus(statusResult.value);
  else {
    renderStatus(fallbackStatus);
    setWorkspaceNotice(`无法读取实时节点状态，正在显示最后一次心跳：${statusResult.reason.message}`, "error");
  }
  if (processResult.status === "fulfilled") renderProcessCount(processResult.value, statusResult.value ?? {});
  else {
    renderProcessCount(null, statusResult.value ?? fallbackStatus);
    $("#processCountHint").textContent = processResult.reason.message;
  }
}

function pathSeparator(path) {
  return /^[A-Za-z]:\\/.test(path) || (path.includes("\\") && !path.includes("/")) ? "\\" : "/";
}

function baseName(path) {
  const parts = String(path).split(/[\\/]/).filter(Boolean);
  return parts.at(-1) ?? path;
}

function joinPath(parent, name) {
  const separator = pathSeparator(parent);
  return parent.endsWith(separator) ? parent + name : parent + separator + name;
}

function pathSegments(root, current) {
  const separator = pathSeparator(root);
  const insensitive = separator === "\\";
  const rootValue = insensitive ? root.toLowerCase() : root;
  const currentValue = insensitive ? current.toLowerCase() : current;
  if (currentValue === rootValue) return [];
  const prefix = rootValue.endsWith(separator) ? rootValue : rootValue + separator;
  if (!currentValue.startsWith(prefix)) return [];
  return current.slice(prefix.length).split(separator).filter(Boolean);
}

function renderBreadcrumbs() {
  const nav = clear($("#fileBreadcrumbs"));
  if (!workspace.currentRoot || !workspace.currentPath) return;
  const rootPath = workspace.currentRoot.configured;
  const root = element("button", "breadcrumb", baseName(rootPath) || rootPath);
  root.type = "button";
  root.dataset.path = rootPath;
  nav.append(root);
  let path = rootPath;
  for (const part of pathSegments(rootPath, workspace.currentPath)) {
    nav.append(element("span", "breadcrumb-separator", "›"));
    path = joinPath(path, part);
    const button = element("button", "breadcrumb", part);
    button.type = "button";
    button.dataset.path = path;
    nav.append(button);
  }
}

function clearPreview() {
  $("#previewName").textContent = "文件预览";
  $("#previewMeta").textContent = "选择一个文本文件。最多读取前 256 KiB，不提供写入或删除操作。";
  $("#filePreview").textContent = "尚未选择文件";
  $("#previewClose").classList.add("hidden");
}

function fileRow(entry) {
  const name = baseName(entry.path);
  const button = element("button", "file-row");
  button.type = "button";
  button.dataset.path = joinPath(workspace.currentPath, name);
  button.dataset.type = entry.type;
  const icon = entry.type === "directory" ? "▰" : entry.type === "symlink" ? "↗" : "·";
  const nameCell = element("span", "file-name");
  nameCell.append(element("span", `file-icon ${entry.type}`, icon), element("span", "", name));
  button.append(nameCell, element("span", "file-size", entry.type === "directory" ? "—" : formatBytes(entry.size)), element("span", "file-time", when(entry.modifiedAt)));
  return button;
}

async function loadDirectory(path) {
  const target = $("#fileList");
  clear(target).append(element("div", "file-loading", "正在读取目录…"));
  clearPreview();
  try {
    const result = await invoke("file", { action: "list", path }, 60000);
    workspace.currentPath = path;
    renderBreadcrumbs();
    clear(target);
    const entries = [...(result.entries ?? [])].sort((left, right) => {
      if (left.type === "directory" && right.type !== "directory") return -1;
      if (right.type === "directory" && left.type !== "directory") return 1;
      return baseName(left.path).localeCompare(baseName(right.path), "zh-CN", { numeric: true });
    });
    if (!entries.length) target.append(element("div", "empty", "这个目录是空的"));
    for (const entry of entries) target.append(fileRow(entry));
  } catch (error) {
    clear(target).append(element("div", "empty error-text", `无法读取目录：${error.message}`));
  }
}

async function loadFileRoots(force = false) {
  if (!capabilityEnabled(workspace.node, "files")) {
    clear($("#fileList")).append(element("div", "empty", "此节点没有提供文件能力"));
    return;
  }
  if (workspace.roots.length && !force) return;
  const result = await invoke("file", { action: "roots" });
  workspace.roots = result.roots ?? [];
  const select = clear($("#fileRootSelect"));
  for (const [index, root] of workspace.roots.entries()) {
    const option = element("option", "", root.configured);
    option.value = String(index);
    select.append(option);
  }
  if (!workspace.roots.length) {
    clear($("#fileList")).append(element("div", "empty", "节点没有配置可浏览的根目录"));
    return;
  }
  workspace.currentRoot = workspace.roots[0];
  workspace.currentPath = workspace.currentRoot.configured;
  await loadDirectory(workspace.currentPath);
}

async function inspectFile(path, expectedType) {
  $("#previewName").textContent = baseName(path);
  $("#previewMeta").textContent = "正在读取文件信息…";
  $("#filePreview").textContent = "读取中…";
  $("#previewClose").classList.remove("hidden");
  try {
    const stat = await invoke("file", { action: "stat", path });
    if (stat.type === "directory" || expectedType === "directory") {
      await loadDirectory(path);
      return;
    }
    if (stat.type !== "file") {
      $("#previewMeta").textContent = `${stat.type ?? "未知类型"} · ${stat.mode ?? "—"}`;
      $("#filePreview").textContent = "仅支持预览普通文件。";
      return;
    }
    const result = await invoke("file", { action: "read", path, offset: 0, length: 256 * 1024 }, 60000);
    const content = String(result.content ?? "");
    const looksBinary = content.includes("\0") || (content.match(/�/g)?.length ?? 0) > 8;
    $("#previewMeta").textContent = `${formatBytes(stat.size)} · ${stat.mode ?? "—"} · ${when(stat.modifiedAt)}${result.eof ? "" : " · 仅显示前 256 KiB"}`;
    $("#filePreview").textContent = looksBinary ? "检测到二进制内容，已停止文本预览。" : content;
  } catch (error) {
    $("#previewMeta").textContent = "读取失败";
    $("#filePreview").textContent = error.message;
  }
}

function resetPTYInput() {
  if (workspace.inputFlushTimer) clearTimeout(workspace.inputFlushTimer);
  workspace.inputFlushTimer = null;
  workspace.inputBuffer = "";
  workspace.inputWriteQueue = Promise.resolve();
}

function flushPTYInput() {
  if (workspace.inputFlushTimer) clearTimeout(workspace.inputFlushTimer);
  workspace.inputFlushTimer = null;
  const input = workspace.inputBuffer;
  const sessionId = workspace.sessionId;
  const nodeId = workspace.node?.nodeId;
  workspace.inputBuffer = "";
  if (!input || !sessionId || !nodeId) return workspace.inputWriteQueue;

  const chunks = [];
  for (let offset = 0; offset < input.length; offset += 32 * 1024) chunks.push(input.slice(offset, offset + 32 * 1024));
  const write = async () => {
    for (const chunk of chunks) {
      if (workspace.sessionId !== sessionId || workspace.node?.nodeId !== nodeId) return;
      try {
        await invoke("pty", { action: "write", sessionId, input: chunk });
      } catch (error) {
        if (workspace.sessionId === sessionId) {
          $("#shellState").textContent = `输入失败：${error.message}`;
          toast(`终端输入失败：${error.message}`);
        }
        return;
      }
    }
  };
  workspace.inputWriteQueue = workspace.inputWriteQueue.then(write, write);
  return workspace.inputWriteQueue;
}

function enqueuePTYInput(data, { immediate = false } = {}) {
  if (!workspace.sessionId || !data) return Promise.resolve();
  workspace.inputBuffer += data;
  if (immediate || workspace.inputBuffer.length >= 8 * 1024) return flushPTYInput();
  if (!workspace.inputFlushTimer) workspace.inputFlushTimer = setTimeout(() => void flushPTYInput(), 12);
  return workspace.inputWriteQueue;
}

function fitTerminal() {
  const container = $("#terminalOutput");
  if (!workspace.fitAddon || !container || container.clientWidth === 0 || container.clientHeight === 0) return false;
  try {
    workspace.fitAddon.fit();
    return true;
  } catch {
    return false;
  }
}

function scheduleTerminalFit() {
  if (workspace.terminalResizeFrame) cancelAnimationFrame(workspace.terminalResizeFrame);
  workspace.terminalResizeFrame = requestAnimationFrame(() => {
    workspace.terminalResizeFrame = null;
    fitTerminal();
  });
}

function schedulePTYResize(cols, rows) {
  if (workspace.terminalResizeTimer) clearTimeout(workspace.terminalResizeTimer);
  const sessionId = workspace.sessionId;
  const nodeId = workspace.node?.nodeId;
  if (!sessionId || !nodeId || !workspace.ptyResizeSupported) return;
  workspace.terminalResizeTimer = setTimeout(async () => {
    workspace.terminalResizeTimer = null;
    if (workspace.sessionId !== sessionId || workspace.node?.nodeId !== nodeId) return;
    try {
      await invoke("pty", { action: "resize", sessionId, rows, cols });
    } catch (error) {
      if (workspace.sessionId === sessionId) $("#shellState").textContent = `调整终端尺寸失败：${error.message}`;
    }
  }, 120);
}

function ensureTerminal() {
  if (workspace.terminal) return workspace.terminal;
  const terminal = new Terminal({
    allowTransparency: true,
    convertEol: false,
    cursorBlink: true,
    disableStdin: true,
    drawBoldTextInBrightColors: true,
    fontFamily: '"Cascadia Mono", "SFMono-Regular", Consolas, "Liberation Mono", monospace',
    fontSize: 13,
    lineHeight: 1.22,
    scrollback: 5000,
    scrollOnUserInput: true,
    theme: {
      background: "#050806",
      foreground: "#d5e6dc",
      cursor: "#98f0bd",
      cursorAccent: "#07100b",
      selectionBackground: "#315b47",
      black: "#07100b",
      brightBlack: "#607168",
      green: "#28c875",
      brightGreen: "#98f0bd",
    },
  });
  const fitAddon = new FitAddon();
  terminal.loadAddon(fitAddon);
  terminal.open($("#terminalOutput"));
  workspace.terminal = terminal;
  workspace.fitAddon = fitAddon;
  workspace.terminalDataDisposable = terminal.onData((data) => enqueuePTYInput(data));
  workspace.terminalResizeDisposable = terminal.onResize(({ cols, rows }) => schedulePTYResize(cols, rows));
  workspace.terminalResizeObserver = new ResizeObserver(scheduleTerminalFit);
  workspace.terminalResizeObserver.observe($("#terminalOutput"));
  terminal.write("连接后可在此执行交互式命令。命令会直接在所选节点上运行。\r\n");
  scheduleTerminalFit();
  return terminal;
}

function disposeTerminal() {
  if (workspace.terminalResizeFrame) cancelAnimationFrame(workspace.terminalResizeFrame);
  workspace.terminalResizeFrame = null;
  workspace.terminalResizeObserver?.disconnect();
  workspace.terminalResizeObserver = null;
  workspace.terminalDataDisposable?.dispose();
  workspace.terminalDataDisposable = null;
  workspace.terminalResizeDisposable?.dispose();
  workspace.terminalResizeDisposable = null;
  if (workspace.terminalResizeTimer) clearTimeout(workspace.terminalResizeTimer);
  workspace.terminalResizeTimer = null;
  workspace.terminal?.dispose();
  workspace.terminal = null;
  workspace.fitAddon = null;
  $("#terminalOutput").replaceChildren();
}

function appendPTYOutput(output) {
  if (!output) return;
  const terminal = ensureTerminal();
  if (output.lostOutput) terminal.write("\r\n\x1b[33m[Mira：较早的终端输出已被节点丢弃]\x1b[0m\r\n");
  for (const chunk of output.chunks ?? []) terminal.write(String(chunk.text ?? ""));
  const cursor = Number(output.cursor);
  if (Number.isSafeInteger(cursor) && cursor >= 0) workspace.cursor = cursor;
}

function setShellConnected(connected, label = connected ? "已连接" : "尚未连接") {
  $("#shellState").textContent = label;
  $("#shellConnect").classList.toggle("hidden", connected);
  $("#shellDisconnect").classList.toggle("hidden", !connected);
  $("#shellInterrupt").classList.toggle("hidden", !connected);
  if (workspace.terminal) workspace.terminal.options.disableStdin = !connected;
  if (connected) workspace.terminal?.focus();
}

function stopShellPolling() {
  if (workspace.pollTimer) clearTimeout(workspace.pollTimer);
  workspace.pollTimer = null;
  workspace.pollBusy = false;
}

async function pollShell() {
  if (!workspace.sessionId || workspace.pollBusy) return;
  workspace.pollBusy = true;
  const sessionId = workspace.sessionId;
  try {
    const result = await invoke("pty", { action: "poll", sessionId, cursor: workspace.cursor }, 15000);
    if (workspace.sessionId !== sessionId) return;
    appendPTYOutput(result.output);
    if (result.running === false) {
      workspace.sessionId = null;
      setShellConnected(false, `会话已结束 · exit ${result.exitCode ?? "—"}`);
      stopShellPolling();
      return;
    }
  } catch (error) {
    if (workspace.sessionId === sessionId) $("#shellState").textContent = `轮询失败：${error.message}`;
  } finally {
    workspace.pollBusy = false;
    if (workspace.sessionId === sessionId) workspace.pollTimer = setTimeout(pollShell, 700);
  }
}

async function connectShell() {
  if (!capabilityEnabled(workspace.node, "pty")) throw new Error("此节点没有提供 PTY 能力");
  if (workspace.node.status !== "online") throw new Error("节点当前离线");
  $("#shellConnect").disabled = true;
  $("#shellState").textContent = "正在创建远程 PTY…";
  try {
    const terminal = ensureTerminal();
    fitTerminal();
    if (!workspace.roots.length && capabilityEnabled(workspace.node, "files")) {
      const roots = await invoke("file", { action: "roots" });
      workspace.roots = roots.roots ?? [];
    }
    const cwd = workspace.currentPath ?? workspace.roots[0]?.configured;
    const params = {
      action: "open",
      rows: Math.max(1, Math.min(500, terminal.rows)),
      cols: Math.max(1, Math.min(1000, terminal.cols)),
    };
    if (cwd) params.cwd = cwd;
    const result = await invoke("pty", params, 60000);
    workspace.sessionId = result.sessionId;
    workspace.ptyResizeSupported = result.resizeSupported === true;
    workspace.cursor = 0;
    resetPTYInput();
    terminal.reset();
    terminal.clear();
    appendPTYOutput(result.output);
    setShellConnected(true, `已连接 · PID ${result.pid ?? "—"} · ${result.backend ?? "PTY"}`);
    void pollShell();
  } finally {
    $("#shellConnect").disabled = false;
  }
}

async function disconnectShell({ quiet = false } = {}) {
  const sessionId = workspace.sessionId;
  stopShellPolling();
  workspace.sessionId = null;
  workspace.ptyResizeSupported = false;
  resetPTYInput();
  if (workspace.terminalResizeTimer) clearTimeout(workspace.terminalResizeTimer);
  workspace.terminalResizeTimer = null;
  setShellConnected(false, "已断开");
  if (!sessionId) return;
  try {
    await invoke("pty", { action: "close", sessionId });
    if (!quiet) toast("远程 Shell 已断开");
  } catch (error) {
    if (!quiet) toast(`会话已在本地关闭：${error.message}`);
  }
}

function activateWorkspaceTab(name) {
  for (const button of document.querySelectorAll("[data-workspace-tab]")) {
    const active = button.dataset.workspaceTab === name;
    button.classList.toggle("active", active);
    button.setAttribute("aria-selected", String(active));
  }
  for (const panel of document.querySelectorAll("[data-workspace-panel]")) panel.classList.toggle("hidden", panel.dataset.workspacePanel !== name);
  if (name === "files" && !workspace.roots.length && workspace.node?.status === "online") {
    loadFileRoots().catch((error) => setWorkspaceNotice(`无法加载文件浏览器：${error.message}`, "error"));
  }
  if (name === "shell") {
    ensureTerminal();
    scheduleTerminalFit();
  }
  if (name === "debug") {
    loadDynamicTools().catch((error) => {
      $("#debugToolCount").textContent = `读取失败：${error.message}`;
      setWorkspaceNotice(`无法读取 Agent 工具定义：${error.message}`, "error");
    });
  }
}

async function openWorkspace(nodeId) {
  disposeTerminal();
  workspace.node = dashboardNodes.get(nodeId);
  workspace.status = null;
  workspace.roots = [];
  workspace.currentRoot = null;
  workspace.currentPath = null;
  workspace.dynamicNamespace = null;
  workspace.dynamicTools = [];
  workspace.selectedDynamicTool = null;
  clearPreview();
  $("#debugToolList").replaceChildren();
  $("#debugResultMeta").textContent = "尚未执行";
  $("#debugResultMeta").className = "debug-result-meta";
  $("#debugResult").textContent = "等待调用…";
  $("#debugResultImage").classList.add("hidden");
  $("#debugResultImage").removeAttribute("src");
  clear($("#fileList")).append(element("div", "empty", "打开“文件浏览器”后读取节点目录"));
  setShellConnected(false);
  renderWorkspaceHeader();
  activateWorkspaceTab("overview");
  show("workspaceView");
  await loadOverview();
}

async function refreshWorkspace() {
  if (!workspace.node) return;
  const response = await api("/v1/nodes?includeRevoked=true");
  const refreshed = (response.data ?? []).find((node) => node.nodeId === workspace.node.nodeId);
  if (!refreshed) throw new Error("节点已不存在或不可访问");
  workspace.node = refreshed;
  dashboardNodes.set(refreshed.nodeId, refreshed);
  renderWorkspaceHeader();
  workspace.roots = [];
  await loadOverview();
  if (workspace.dynamicNamespace) renderDynamicTools();
}

async function leaveWorkspace() {
  if (workspace.sessionId) {
    if (!confirm("返回设备列表会断开当前远程 Shell。继续吗？")) return;
    await disconnectShell({ quiet: true });
  }
  disposeTerminal();
  workspace.node = null;
  show("dashboardView");
  await loadDashboard();
}

function setConversationNotice(message = "", kind = "") {
  const notice = $("#conversationNotice");
  notice.textContent = message;
  notice.className = `workspace-notice${message ? "" : " hidden"}${kind ? ` ${kind}` : ""}`;
}

function setAgentRuntimeState(message, status = "offline") {
  $("#agentRuntimeState").textContent = message;
  $("#agentRuntimeBadge").textContent = status;
  $("#agentRuntimeBadge").className = `badge ${status}`;
}

function notificationThreadId(params = {}) {
  const turnId = params.turnId ?? params.turn?.id ?? null;
  return params.threadId ?? (turnId ? agent.turnThreads.get(turnId) : null) ?? null;
}

function notificationIsForOpenThread(params = {}) {
  const threadId = notificationThreadId(params);
  return !threadId || threadId === agent.threadId;
}

function syncActiveTurnUi() {
  agent.turnId = agent.threadId ? (agent.activeTurns.get(agent.threadId) ?? null) : null;
  $("#agentInterrupt").classList.toggle("hidden", !agent.turnId);
}

function syncConversationSendUi() {
  const selectedNode = $("#agentRuntimeNode")?.value;
  const busy = Boolean(agent.sendPromise);
  $("#conversationSend").disabled = busy || !selectedNode;
  $("#agentRuntimeNode").disabled = busy;
  $("#agentNewThread").disabled = busy;
  for (const button of $("#agentThreadList").querySelectorAll("button[data-thread-id]")) button.disabled = busy;
}

function closeAgentSocket() {
  const socket = agent.socket;
  agent.socket = null;
  agent.socketNodeId = null;
  for (const pending of agent.pending.values()) pending.reject(new Error("App Server connection closed"));
  agent.pending.clear();
  agent.activeTurns.clear();
  agent.turnThreads.clear();
  syncActiveTurnUi();
  if (socket && socket.readyState < WebSocket.CLOSING) socket.close(1000, "client closed");
  syncConversationSendUi();
}

function rpc(method, params = {}, timeoutMs = 60_000) {
  if (!agent.socket || agent.socket.readyState !== WebSocket.OPEN) return Promise.reject(new Error("App Server 尚未连接"));
  const id = ++agent.requestId;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      agent.pending.delete(id);
      reject(new Error(`${method} 请求超时`));
    }, timeoutMs);
    agent.pending.set(id, {
      resolve: (value) => { clearTimeout(timer); resolve(value); },
      reject: (error) => { clearTimeout(timer); reject(error); },
    });
    agent.socket.send(JSON.stringify({ id, method, params }));
  });
}

function traceText(value) {
  if (typeof value === "string") return value;
  if (!value) return "";
  if (Array.isArray(value)) return value.map(traceText).filter(Boolean).join("\n");
  if (typeof value === "object") {
    if (typeof value.text === "string") return value.text;
    if (typeof value.inputText === "string") return value.inputText;
    if (value.content) return traceText(value.content);
  }
  return "";
}

function traceUsesMarkdown(kind) {
  return ["user", "assistant", "reasoning"].includes(kind);
}

function currentAgentThread() {
  return agent.threads.find((thread) => thread.threadId === agent.threadId) ?? null;
}

function conversationNodeCandidates() {
  const thread = currentAgentThread();
  return [...new Set([
    agent.threadRuntimeNodeId,
    agent.previousRuntimeNodeId,
    thread?.runtimeNodeId,
    thread?.sourceNodeId,
    agent.socketNodeId,
    $("#agentRuntimeNode")?.value,
  ].filter(Boolean))];
}

function resolveNodeFileReference(value) {
  let reference = String(value ?? "").trim();
  if (!reference || reference.startsWith("#") || /^(?:https?|mailto|tel|data|blob|javascript):/i.test(reference)) return null;
  try { reference = decodeURIComponent(reference); } catch { /* retain malformed percent escapes */ }
  if (reference.startsWith("<") && reference.endsWith(">")) reference = reference.slice(1, -1).trim();
  if (/^[a-z][a-z0-9+.-]*:/i.test(reference) && !/^file:/i.test(reference) && !/^[A-Za-z]:[\\/]/.test(reference)) return null;
  if (/^file:/i.test(reference)) {
    try {
      const url = new URL(reference);
      reference = decodeURIComponent(url.pathname);
      if (/^\/[A-Za-z]:\//.test(reference)) reference = reference.slice(1);
      if (url.hostname && url.hostname !== "localhost") reference = `\\\\${url.hostname}${reference.replaceAll("/", "\\")}`;
    } catch { return null; }
  }
  const lineMatch = reference.match(/:(\d+)(?::(\d+))?$/);
  const line = lineMatch ? Number(lineMatch[1]) : null;
  const column = lineMatch?.[2] ? Number(lineMatch[2]) : null;
  if (lineMatch) reference = reference.slice(0, -lineMatch[0].length);
  const cwd = $("#conversationCwd")?.value.trim() ?? "";
  const absolute = reference.startsWith("/") || /^[A-Za-z]:[\\/]/.test(reference) || reference.startsWith("\\\\");
  const pathLike = absolute || reference.startsWith("./") || reference.startsWith("../") ||
    reference.includes("/") || reference.includes("\\") || /\.[\p{L}][\p{L}\p{N}._-]{0,15}$/u.test(reference);
  if (!pathLike || (!absolute && !cwd)) return null;
  if (!absolute) {
    const separator = pathSeparator(cwd);
    reference = joinPath(cwd, reference.replace(/[\\/]/g, separator));
  }
  return { path: reference, line, column };
}

function decorateTraceFileReferences(root) {
  for (const anchor of root.querySelectorAll("a[href]")) {
    const reference = resolveNodeFileReference(anchor.getAttribute("href"));
    if (reference) {
      anchor.dataset.nodeFilePath = reference.path;
      if (reference.line) anchor.dataset.nodeFileLine = String(reference.line);
      if (reference.column) anchor.dataset.nodeFileColumn = String(reference.column);
      anchor.classList.add("node-file-link");
      anchor.title = `从 Mira Node 打开 ${reference.path}`;
      anchor.setAttribute("href", "#");
    } else {
      anchor.target = "_blank";
      anchor.rel = "noopener noreferrer";
    }
  }
  for (const image of root.querySelectorAll("img[src]")) {
    const reference = resolveNodeFileReference(image.getAttribute("src"));
    if (!reference) continue;
    const button = element("button", "node-file-image-link", `▧ ${image.alt || baseName(reference.path)}`);
    button.type = "button";
    button.dataset.nodeFilePath = reference.path;
    if (reference.line) button.dataset.nodeFileLine = String(reference.line);
    image.replaceWith(button);
  }
  for (const code of root.querySelectorAll("code")) {
    if (code.closest("pre, a, button")) continue;
    const reference = resolveNodeFileReference(code.textContent);
    if (!reference) continue;
    const button = element("button", "node-file-code-link");
    button.type = "button";
    button.dataset.nodeFilePath = reference.path;
    if (reference.line) button.dataset.nodeFileLine = String(reference.line);
    button.title = `从 Mira Node 打开 ${reference.path}`;
    button.append(code.cloneNode(true));
    code.replaceWith(button);
  }
}

function createTraceCopyButton(card) {
  const button = element("button", "trace-copy", "复制");
  button.type = "button";
  button.title = "复制消息原文（保留 Markdown）";
  button.setAttribute("aria-label", "复制这条消息的原文");
  button.addEventListener("click", async (event) => {
    event?.preventDefault();
    event?.stopPropagation();
    // Read at click time: the same card may still be receiving streamed deltas.
    const source = card.querySelector(".trace-body")._miraSource ?? "";
    if (!source || button.disabled) return;
    button.disabled = true;
    try {
      await navigator.clipboard.writeText(source);
      toast("消息原文已复制");
    } catch {
      toast("浏览器未允许复制，请选中消息手动复制");
    } finally {
      button.disabled = false;
    }
  });
  return button;
}

function setTraceBody(card, body, kind = card.dataset.traceKind) {
  const value = body ?? "";
  const node = card.querySelector(".trace-body");
  node._miraSource = value;
  const markdown = traceUsesMarkdown(kind);
  node.classList.toggle("markdown-body", markdown);
  if (markdown) {
    node.innerHTML = DOMPurify.sanitize(marked.parse(value));
    decorateTraceFileReferences(node);
  }
  else node.textContent = value;
  node.hidden = value.length === 0;
  card.classList.toggle("trace-card-empty", value.length === 0);
  card.querySelector(".trace-copy").hidden = value.length === 0;
  if (kind === "reasoning" && card._miraExpandable) {
    card.querySelector(".trace-kind").textContent = reasoningHeading(value);
  }
}

function traceNearBottom(trace = $("#conversationTrace"), threshold = 96) {
  return trace.scrollHeight - trace.clientHeight - trace.scrollTop <= threshold;
}

function scrollTraceToBottom(trace = $("#conversationTrace")) {
  trace.scrollTop = trace.scrollHeight;
}

function updateToolGroup(group) {
  if (!group) return;
  const cards = [...group.querySelectorAll(".trace-card.tool")];
  const activities = cards.map((card) => card._miraActivity ?? {
    status: activityStatus(card.dataset.traceStatus),
    actions: [{ kind: "tool", label: card.dataset.traceTitle || "工具" }],
  });
  const running = activities.filter((activity) => activity.status === "running").length;
  group.querySelector(".tool-group-total").textContent = `工具调用 · ${cards.length} 次${running ? ` · ${running} 运行中` : ""}`;
  group.querySelector(".tool-group-counts").textContent = summarizeActivities(activities);
  const latest = activities.findLast((activity) => activity.status === "running") ?? activities.at(-1);
  const duration = formatActivityDuration(latest?.durationMs);
  group.querySelector(".tool-group-latest").textContent = `${activitySummary(latest)}${duration ? ` · ${duration}` : ""}`;
  group.classList.toggle("has-running-tool", running > 0);
}

function ensureToolGroup(trace, turnId = "") {
  let group = trace.lastElementChild;
  if (group?.classList.contains("tool-group") && group.dataset.turnId === turnId) return group;
  group = element("details", "tool-group");
  group.dataset.turnId = turnId;
  const summary = element("summary", "tool-group-summary");
  summary.append(element("span", "tool-group-total"), element("span", "tool-group-counts"), element("span", "tool-group-latest"));
  group.append(summary, element("div", "tool-group-items"));
  trace.append(group);
  return group;
}

function upsertTrace(key, kind, title, body = undefined, status = "", options = {}) {
  const trace = $("#conversationTrace");
  const follow = options.forceScroll === true || traceNearBottom(trace);
  trace.querySelector(".conversation-empty")?.remove();
  let card = key ? trace.querySelector(`[data-trace-key="${CSS.escape(key)}"]`) : null;
  if (!card) {
    card = element("article", `trace-card ${kind}`);
    if (key) card.dataset.traceKey = key;
    card.dataset.traceKind = kind;
    card._miraExpandable = ["tool", "reasoning"].includes(kind);
    const head = element(card._miraExpandable ? "summary" : "div", "trace-head");
    const actions = element("div", "trace-actions");
    actions.append(element("span", "trace-status", status), createTraceCopyButton(card));
    head.append(element("span", "trace-kind", title), actions);
    if (card._miraExpandable) {
      const details = element("details", "trace-detail");
      details.append(head, element("div", "trace-body"));
      card.append(details);
    } else card.append(head, element("div", "trace-body"));
    setTraceBody(card, body, kind);
    if (kind === "tool" && options.collapseTools !== false) {
      ensureToolGroup(trace, options.turnId ?? "").querySelector(".tool-group-items").append(card);
    } else {
      trace.append(card);
    }
  } else {
    card.className = `trace-card ${kind}`;
    card.dataset.traceKind = kind;
    card.querySelector(".trace-kind").textContent = title;
    card.querySelector(".trace-status").textContent = status;
    if (body !== undefined) setTraceBody(card, body, kind);
  }
  card.dataset.traceTitle = title;
  card.dataset.traceStatus = status;
  if (options.turnId) card.dataset.turnId = options.turnId;
  if (options.activity !== undefined) card._miraActivity = options.activity;
  if (options.summaryParts?.some((part) => part.trim())) card._miraSummaryParts = [...options.summaryParts];
  if (card._miraActivity) {
    card.querySelector(".trace-kind").textContent = activitySummary(card._miraActivity);
    card.querySelector(".trace-status").textContent = formatActivityDuration(card._miraActivity.durationMs);
    card.classList.toggle("activity-failed", ["failed", "declined", "interrupted"].includes(card._miraActivity.status));
  } else if (kind === "reasoning") {
    card.querySelector(".trace-kind").textContent = reasoningHeading(card.querySelector(".trace-body")._miraSource);
    card.querySelector(".trace-status").textContent = "";
  }
  updateToolGroup(card.closest(".tool-group"));
  if (options.autoScroll !== false && follow) scrollTraceToBottom(trace);
  return card;
}

function nodeFileMimeType(path) {
  const extension = String(path).split(/[?#]/, 1)[0].split(".").at(-1)?.toLowerCase() ?? "";
  const types = {
    png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif", webp: "image/webp",
    svg: "image/svg+xml", bmp: "image/bmp", ico: "image/x-icon", avif: "image/avif",
    pdf: "application/pdf", txt: "text/plain", md: "text/markdown", markdown: "text/markdown",
    json: "application/json", jsonl: "application/x-ndjson", csv: "text/csv", log: "text/plain",
    js: "text/javascript", mjs: "text/javascript", cjs: "text/javascript", ts: "text/plain", tsx: "text/plain",
    jsx: "text/plain", css: "text/css", html: "text/html", htm: "text/html", xml: "application/xml",
    yaml: "text/yaml", yml: "text/yaml", toml: "text/plain", ini: "text/plain", conf: "text/plain",
    sh: "text/x-shellscript", zsh: "text/x-shellscript", bash: "text/x-shellscript", py: "text/x-python",
    go: "text/plain", rs: "text/plain", java: "text/plain", kt: "text/plain", nix: "text/plain",
    mp4: "video/mp4", webm: "video/webm", mov: "video/quicktime", mp3: "audio/mpeg",
    wav: "audio/wav", ogg: "audio/ogg", m4a: "audio/mp4",
  };
  return types[extension] ?? "application/octet-stream";
}

function base64Bytes(value) {
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
  return bytes;
}

function bytesBase64(bytes) {
  let binary = "";
  for (let offset = 0; offset < bytes.length; offset += 32 * 1024) {
    binary += String.fromCharCode(...bytes.subarray(offset, Math.min(offset + 32 * 1024, bytes.length)));
  }
  return btoa(binary);
}

async function readNodeFile(nodeId, path, stat) {
  const size = Number(stat.size ?? 0);
  if (!Number.isSafeInteger(size) || size < 0) throw new Error("Node 返回了无效的文件大小");
  if (size > maximumConversationFileBytes) {
    throw new Error(`网页端单次最多读取 ${formatBytes(maximumConversationFileBytes)}；此文件为 ${formatBytes(size)}`);
  }
  const chunks = [];
  for (let offset = 0; offset < size;) {
    $("#nodeFileLoading").textContent = `正在从 Node 读取 ${formatBytes(offset)} / ${formatBytes(size)}…`;
    const result = await invokeNode(nodeId, "file", {
      action: "read", path, offset, length: Math.min(nodeFileChunkBytes, size - offset), encoding: "base64",
    }, 60_000);
    if (result.encoding !== "base64") throw new Error("Node 没有返回预期的二进制文件数据");
    const chunk = base64Bytes(result.content ?? "");
    if (!chunk.length && !result.eof) throw new Error("Node 文件读取没有取得进展");
    chunks.push(chunk);
    offset += chunk.length;
    if (result.eof) break;
  }
  return new Blob(chunks, { type: nodeFileMimeType(path) });
}

function resetNodeFileDialog() {
  if (agent.fileObjectUrl) URL.revokeObjectURL(agent.fileObjectUrl);
  agent.fileObjectUrl = null;
  for (const selector of ["#nodeFileImage", "#nodeFileVideo", "#nodeFileAudio", "#nodeFileFrame"]) {
    const media = $(selector);
    media.classList.add("hidden");
    media.removeAttribute("src");
  }
  $("#nodeFileText").classList.add("hidden");
  $("#nodeFileText").textContent = "";
  $("#nodeFileUnsupported").classList.add("hidden");
  $("#nodeFileDownload").classList.add("hidden");
  $("#nodeFileDownload").removeAttribute("href");
  $("#nodeFileLoading").classList.remove("hidden");
  $("#nodeFileLoading").textContent = "正在从 Node 读取文件…";
}

async function openNodeFile(path, line = null) {
  const dialog = $("#nodeFileDialog");
  resetNodeFileDialog();
  $("#nodeFileTitle").textContent = baseName(path);
  $("#nodeFileMeta").textContent = "正在查找包含此文件的节点…";
  $("#nodeFilePath").textContent = path;
  if (!dialog.open) dialog.showModal();
  const candidates = conversationNodeCandidates();
  if (!candidates.length) throw new Error("这个会话没有可用于读取文件的节点");
  let selectedNode = null;
  let stat = null;
  let lastError = null;
  for (const nodeId of candidates) {
    try {
      const candidate = await invokeNode(nodeId, "file", { action: "stat", path }, 30_000);
      if (candidate.type !== "file") throw new Error("路径不是普通文件");
      selectedNode = nodeId;
      stat = candidate;
      break;
    } catch (error) { lastError = error; }
  }
  if (!selectedNode) throw new Error(`无法从会话关联的节点读取此文件：${lastError?.message ?? "文件不存在"}`);
  const blob = await readNodeFile(selectedNode, path, stat);
  const mime = blob.type || "application/octet-stream";
  const node = dashboardNodes.get(selectedNode);
  $("#nodeFileMeta").textContent = `${formatBytes(blob.size)} · ${mime} · ${node?.hostname ?? selectedNode}${line ? ` · 第 ${line} 行` : ""}`;
  $("#nodeFileLoading").classList.add("hidden");
  agent.fileObjectUrl = URL.createObjectURL(blob);
  const download = $("#nodeFileDownload");
  download.href = agent.fileObjectUrl;
  download.download = baseName(path);
  download.classList.remove("hidden");
  if (mime.startsWith("image/")) {
    $("#nodeFileImage").src = agent.fileObjectUrl;
    $("#nodeFileImage").classList.remove("hidden");
  } else if (mime.startsWith("video/")) {
    $("#nodeFileVideo").src = agent.fileObjectUrl;
    $("#nodeFileVideo").classList.remove("hidden");
  } else if (mime.startsWith("audio/")) {
    $("#nodeFileAudio").src = agent.fileObjectUrl;
    $("#nodeFileAudio").classList.remove("hidden");
  } else if (mime === "application/pdf") {
    $("#nodeFileFrame").src = agent.fileObjectUrl;
    $("#nodeFileFrame").classList.remove("hidden");
  } else if (mime.startsWith("text/") || ["application/json", "application/xml", "application/x-ndjson"].includes(mime)) {
    const preview = $("#nodeFileText");
    if (blob.size <= 4 * 1024 * 1024) {
      preview.textContent = await blob.text();
      preview.classList.remove("hidden");
      if (line) requestAnimationFrame(() => {
        const lineHeight = Number.parseFloat(getComputedStyle(preview).lineHeight) || 20;
        preview.scrollTop = Math.max(0, (line - 3) * lineHeight);
      });
    } else {
      $("#nodeFileUnsupported").textContent = "文本文件超过 4 MiB；为避免卡住页面，请直接下载查看。";
      $("#nodeFileUnsupported").classList.remove("hidden");
    }
  } else {
    $("#nodeFileUnsupported").classList.remove("hidden");
  }
}

function appendTraceText(key, kind, title, delta, status = "运行中") {
  if (!delta) return;
  const trace = $("#conversationTrace");
  const follow = traceNearBottom(trace);
  const existing = trace.querySelector(`[data-trace-key="${CSS.escape(key)}"]`);
  const effectiveTitle = existing?.dataset.traceTitle || title;
  const card = upsertTrace(key, kind, effectiveTitle, undefined, status, { autoScroll: false, turnId: agent.turnId });
  const body = card.querySelector(".trace-body");
  setTraceBody(card, `${body._miraSource ?? body.textContent ?? ""}${delta}`, kind);
  if (follow) scrollTraceToBottom(trace);
}

function reconcilePendingUserTrace(key, body) {
  const candidates = [...$("#conversationTrace").querySelectorAll('[data-pending-user="true"]')].reverse();
  const card = candidates.find((candidate) => {
    const pendingBody = candidate.querySelector(".trace-body")?._miraSource ?? "";
    return !body || pendingBody === body;
  });
  if (!card) return;
  card.dataset.traceKey = key;
  delete card.dataset.pendingUser;
}

function itemView(item) {
  const type = item?.type ?? "item";
  if (type === "userMessage") return { kind: "user", title: "你", body: traceText(item.content) };
  if (type === "agentMessage") return { kind: "assistant", title: "Codex", body: item.text ?? traceText(item.content) };
  if (type === "reasoning") return { kind: "reasoning", title: "推理摘要", body: reasoningText(item), summaryParts: reasoningParts(item) };
  const tool = toolItemView(item);
  if (tool) return tool;
  if (type === "plan") return { kind: "reasoning", title: "计划", body: traceText(item.text ?? item.plan) || JSON.stringify(item, null, 2) };
  if (type === "contextCompaction") return { kind: "system", title: "上下文压缩", body: item.summary ?? "已压缩较早上下文" };
  return { kind: "tool", title: type, body: JSON.stringify(item, null, 2) };
}

function liveTraceKey(params, itemId) {
  return `item-${JSON.stringify([params.threadId ?? notificationThreadId(params) ?? agent.threadId,
    Object.hasOwn(params, "turnId") ? params.turnId : agent.turnId, itemId])}`;
}

function appendReasoningSummary(params) {
  if (!params.delta) return;
  const index = params.summaryIndex ?? 0;
  if (!Number.isSafeInteger(index) || index < 0 || index > 1000) return;
  const key = liveTraceKey(params, params.itemId ?? "reasoning");
  const existing = $("#conversationTrace").querySelector(`[data-trace-key="${CSS.escape(key)}"]`);
  const initial = existing?.querySelector(".trace-body")?._miraSource;
  const parts = existing?._miraSummaryParts ?? (initial ? [initial] : []);
  parts[index] = `${parts[index] ?? ""}${params.delta}`;
  const body = parts.filter(Boolean).join("\n\n");
  if (!body.trim()) return;
  const card = upsertTrace(key, "reasoning", "推理摘要", body, "", { turnId: params.turnId });
  card._miraSummaryParts = parts;
}

function renderThread(thread) {
  const trace = clear($("#conversationTrace"));
  const turns = Array.isArray(thread?.turns) ? thread.turns : [];
  for (const turn of turns) {
    for (const item of turn.items ?? []) {
      const view = itemView(item);
      if (["assistant", "reasoning"].includes(view.kind) && !view.body) continue;
      upsertTrace(liveTraceKey({ turnId: turn.id }, item.id ?? crypto.randomUUID?.() ?? Math.random()), view.kind, view.title, view.body, item.status ?? "", { autoScroll: false, activity: view.activity, summaryParts: view.summaryParts, turnId: turn.id });
    }
  }
  if (!turns.length) trace.append(element("div", "conversation-empty", "此会话还没有可显示的消息。"));
  requestAnimationFrame(() => scrollTraceToBottom(trace));
}

function resetAgentTranscript(threadId = null) {
  agent.transcriptThreadId = threadId;
  agent.transcriptGeneration = null;
  agent.transcriptItems = [];
  agent.transcriptCursor = null;
  agent.transcriptTotal = 0;
  agent.transcriptLoadingOlder = false;
}

function mergeTranscriptItems(current, updates) {
  const merged = new Map(current.map((item) => [item.key, item]));
  for (const item of updates) merged.set(item.key, item);
  return [...merged.values()].sort((left, right) =>
    (left.sourceItemSeq ?? Number.MAX_SAFE_INTEGER) - (right.sourceItemSeq ?? Number.MAX_SAFE_INTEGER));
}

function renderHistoryLoader(trace) {
  if (agent.transcriptCursor === null) return;
  const loader = element("div", "history-loader");
  const button = element("button", "history-load-button",
    agent.transcriptLoadingOlder
      ? "正在加载更早历史…"
      : `加载更早历史 · 已显示 ${agent.transcriptItems.length} / ${agent.transcriptTotal}`);
  button.type = "button";
  button.dataset.loadOlder = "true";
  button.disabled = agent.transcriptLoadingOlder;
  loader.append(button);
  trace.append(loader);
}

function renderTranscript(fallbackThread, options = {}) {
  const expandedItems = new Set([...$("#conversationTrace").querySelectorAll(".trace-detail[open]")]
    .map((details) => details.closest(".trace-card").dataset.traceKey));
  const expandedGroups = new Set([...$("#conversationTrace").querySelectorAll(".tool-group[open] .trace-card")]
    .map((card) => card.dataset.traceKey));
  const trace = clear($("#conversationTrace"));
  renderHistoryLoader(trace);
  for (const item of agent.transcriptItems) {
    const key = item.itemId ? liveTraceKey({ turnId: item.turnId }, item.itemId) : item.key;
    const card = upsertTrace(key, item.kind ?? "tool", item.title ?? "事件", item.body ?? "", item.status ?? "", { autoScroll: false, activity: item.activity, summaryParts: item.summaryParts, turnId: item.turnId });
    if (expandedItems.has(key) && card.querySelector(".trace-detail")) card.querySelector(".trace-detail").open = true;
    if (expandedGroups.has(key) && card.closest(".tool-group")) card.closest(".tool-group").open = true;
  }
  if (!agent.transcriptItems.length) {
    renderThread(fallbackThread);
    if (!fallbackThread?.turns?.some((turn) => (turn.items ?? []).length > 0)) {
      clear(trace).append(element("div", "conversation-empty", "数据库中没有可投影的消息或工具记录。"));
    }
    return;
  }
  requestAnimationFrame(() => {
    if (options.preserveViewport) {
      trace.scrollTop = options.preserveViewport.mode === "prepend"
        ? options.preserveViewport.top + trace.scrollHeight - options.preserveViewport.height
        : options.preserveViewport.top;
    } else if (options.anchorBottom !== false) {
      scrollTraceToBottom(trace);
      requestAnimationFrame(() => scrollTraceToBottom(trace));
    }
  });
}

async function loadAgentTranscript(threadId, fallbackThread = null, options = {}) {
  const query = new URLSearchParams({ storeId: "personal", limit: String(options.limit ?? transcriptPageSize) });
  if (options.cursor !== undefined && options.cursor !== null) query.set("cursor", String(options.cursor));
  const transcript = await api(`/v1/codex/threads/${encodeURIComponent(threadId)}/transcript?${query}`);
  if (agent.threadId !== threadId) return transcript;

  const incoming = Array.isArray(transcript.trace) ? transcript.trace : [];
  const sameThread = agent.transcriptThreadId === threadId &&
    agent.transcriptGeneration === transcript.generation;
  const trace = $("#conversationTrace");
  const preserveViewport = options.prepend
    ? { mode: "prepend", top: trace.scrollTop, height: trace.scrollHeight }
    : options.preserveLoaded && options.anchorBottom === false
      ? { mode: "stable", top: trace.scrollTop, height: trace.scrollHeight }
      : null;
  if (options.prepend && sameThread) {
    agent.transcriptItems = mergeTranscriptItems(incoming, agent.transcriptItems);
    agent.transcriptCursor = transcript.nextCursor ?? null;
  } else if (options.preserveLoaded && sameThread) {
    const wasEmpty = agent.transcriptItems.length === 0;
    agent.transcriptItems = mergeTranscriptItems(agent.transcriptItems, incoming);
    if (wasEmpty) agent.transcriptCursor = transcript.nextCursor ?? null;
  } else {
    agent.transcriptThreadId = threadId;
    agent.transcriptGeneration = transcript.generation ?? null;
    agent.transcriptItems = incoming;
    agent.transcriptCursor = transcript.nextCursor ?? null;
  }
  agent.transcriptTotal = transcript.totalTraceItems ?? agent.transcriptItems.length;
  renderTranscript(fallbackThread, {
    preserveViewport,
    anchorBottom: options.anchorBottom !== false,
  });
  return transcript;
}

async function loadOlderAgentTranscript() {
  if (!agent.threadId || agent.transcriptCursor === null || agent.transcriptLoadingOlder) return;
  agent.transcriptLoadingOlder = true;
  const button = $("#conversationTrace").querySelector("[data-load-older]");
  if (button) {
    button.disabled = true;
    button.textContent = "正在加载更早历史…";
  }
  try {
    await loadAgentTranscript(agent.threadId, null, {
      cursor: agent.transcriptCursor,
      prepend: true,
      anchorBottom: false,
    });
  } finally {
    agent.transcriptLoadingOlder = false;
    const current = $("#conversationTrace").querySelector("[data-load-older]");
    if (current) {
      current.disabled = false;
      current.textContent = `加载更早历史 · 已显示 ${agent.transcriptItems.length} / ${agent.transcriptTotal}`;
    }
  }
}

async function refreshCompletedTranscript(threadId) {
  // ThreadStore persistence can finish shortly after turn/completed. Refresh a
  // few times so missed live notifications are replaced by the authoritative
  // database projection without requiring a manual reopen.
  for (const delay of [250, 750, 1_500]) {
    await new Promise((resolve) => setTimeout(resolve, delay));
    if (agent.threadId !== threadId) return;
    try {
      await loadAgentTranscript(threadId, null, {
        preserveLoaded: true,
        anchorBottom: traceNearBottom(),
      });
    } catch { /* next refresh may succeed */ }
  }
}

function handleAgentNotification(message) {
  const method = message.method ?? "";
  const params = message.params ?? {};
  if (message.id !== undefined) {
    if (notificationIsForOpenThread(params)) {
      upsertTrace(`request-${message.id}`, "system", method,
        "当前网页客户端未启用交互审批；本界面发起的 Turn 使用 never。", "等待处理");
    }
    agent.socket?.send(JSON.stringify({ id: message.id, error: { code: -32601, message: "interactive request is not supported by Mira Web" } }));
    return;
  }
  if (method === "turn/started") {
    const turnId = params.turn?.id ?? null;
    const threadId = params.threadId ?? agent.threadId;
    if (threadId && turnId) {
      agent.activeTurns.set(threadId, turnId);
      agent.turnThreads.set(turnId, threadId);
    }
    syncActiveTurnUi();
    return;
  }
  if (method === "turn/completed") {
    const turn = params.turn ?? {};
    const completedTurnId = turn.id ?? params.turnId ?? null;
    const threadId = notificationThreadId(params) ?? agent.threadId;
    if (threadId && (!completedTurnId || agent.activeTurns.get(threadId) === completedTurnId)) {
      agent.activeTurns.delete(threadId);
    }
    if (completedTurnId) agent.turnThreads.delete(completedTurnId);
    syncActiveTurnUi();
    void loadAgentThreads();
    if (threadId && threadId !== agent.threadId) return;
    const status = String(turn.status ?? "").toLowerCase();
    if (turn.error?.message || ["failed", "error"].includes(status)) {
      upsertTrace(`turn-${completedTurnId ?? Date.now()}`, "error", "Turn 失败",
        turn.error?.message ?? "Codex Turn 执行失败", turn.status ?? "失败");
    } else if (["interrupted", "cancelled", "canceled", "aborted"].includes(status)) {
      upsertTrace(`turn-${completedTurnId ?? Date.now()}`, "system", "Turn 已中断",
        "本次执行没有继续完成。", turn.status);
    }
    if (threadId) void refreshCompletedTranscript(threadId);
    return;
  }
  if (!notificationIsForOpenThread(params)) return;
  if (method === "item/started" || method === "item/completed") {
    const item = params.item ?? {};
    const view = itemView({ ...item, status: item.status ?? (method.endsWith("started") ? "inProgress" : "completed") });
    const itemKey = liveTraceKey(params, item.id ?? (view.kind === "user" ? "current-user" : method));
    if (view.kind === "user") reconcilePendingUserTrace(itemKey, view.body);
    const existing = $("#conversationTrace").querySelector(`[data-trace-key="${CSS.escape(itemKey)}"]`);
    const emptyNarrative = ["assistant", "reasoning"].includes(view.kind) && !view.body;
    if (emptyNarrative && !existing) return;
    upsertTrace(itemKey, view.kind, view.title, emptyNarrative ? undefined : view.body,
      method.endsWith("started") ? "运行中" : (item.status ?? "完成"), { activity: view.activity, summaryParts: view.summaryParts, turnId: params.turnId });
    return;
  }
  if (method === "item/agentMessage/delta") {
    appendTraceText(liveTraceKey(params, params.itemId ?? "assistant"), "assistant", "Codex", params.delta ?? "");
    return;
  }
  if (method === "item/reasoning/summaryTextDelta") {
    appendReasoningSummary(params);
    return;
  }
  if (method === "item/plan/delta") {
    appendTraceText(liveTraceKey(params, params.itemId ?? "plan"), "reasoning", "计划", params.delta ?? "");
    return;
  }
  if (["item/commandExecution/outputDelta", "item/fileChange/outputDelta", "item/mcpToolCall/progress"].includes(method)) {
    appendTraceText(liveTraceKey(params, params.itemId ?? "tool"), "tool", "工具输出", params.delta ?? params.message ?? "");
    return;
  }
  if (method === "error" || method === "warning") {
    upsertTrace(null, method === "error" ? "error" : "system", method === "error" ? "错误" : "警告", params.error?.message ?? params.message ?? JSON.stringify(params), "");
  }
}

function onAgentSocketMessage(event) {
  let message;
  try { message = JSON.parse(event.data); } catch { return; }
  if (message.id !== undefined && (Object.hasOwn(message, "result") || Object.hasOwn(message, "error"))) {
    const pending = agent.pending.get(message.id);
    if (!pending) return;
    agent.pending.delete(message.id);
    if (message.error) pending.reject(new Error(message.error.message ?? JSON.stringify(message.error)));
    else pending.resolve(message.result);
    return;
  }
  handleAgentNotification(message);
}

async function connectAgentSocket(nodeId) {
  if (agent.socket?.readyState === WebSocket.OPEN && agent.socketNodeId === nodeId) return;
  closeAgentSocket();
  setAgentRuntimeState("正在建立 App Server 通道…", "offline");
  const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
  const socket = new WebSocket(`${scheme}//${window.location.host}/v1/nodes/${nodeId}/app-server?storeId=personal`, ["mira-client-v1"]);
  agent.socket = socket;
  agent.socketNodeId = nodeId;
  socket.addEventListener("message", onAgentSocketMessage);
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("App Server WebSocket 连接超时")), 15_000);
    socket.addEventListener("open", () => { clearTimeout(timer); resolve(); }, { once: true });
    socket.addEventListener("error", () => { clearTimeout(timer); reject(new Error("App Server WebSocket 连接失败")); }, { once: true });
  });
  socket.addEventListener("close", () => {
    if (agent.socket !== socket) return;
    closeAgentSocket();
    setAgentRuntimeState("App Server 通道已断开", "offline");
    setConversationNotice("运行节点或 App Server 已断开。重新连接后可继续同一 PostgreSQL thread。", "warning");
  });
  await rpc("initialize", {
    clientInfo: { name: "mira_web", title: "Mira Web", version: "1" },
    capabilities: { experimentalApi: true },
  });
  socket.send(JSON.stringify({ method: "initialized" }));
  syncConversationSendUi();
  setConversationNotice();
  const node = dashboardNodes.get(nodeId);
  setAgentRuntimeState(`已连接 ${node?.hostname ?? nodeId}`, "online");
}

async function refreshAgentNodes() {
  const response = await api("/v1/nodes");
  const nodes = response.data ?? [];
  dashboardNodes = new Map(nodes.map((node) => [node.nodeId, node]));
  const runtimeSelect = $("#agentRuntimeNode");
  const sourceSelect = $("#sessionSourceNode");
  const previousRuntime = runtimeSelect.value;
  const previousSource = sourceSelect.value;
  clear(runtimeSelect);
  clear(sourceSelect);
  for (const node of nodes.filter((value) => value.capabilities?.appServer === true)) {
    const option = element("option", "", `${node.hostname} · ${node.platform} · ${node.status}`);
    option.value = node.nodeId;
    runtimeSelect.append(option);
  }
  for (const node of nodes.filter((value) => value.capabilities?.codexSessions === true)) {
    const option = element("option", "", `${node.hostname} · ${node.nodeMode} · ${node.status}`);
    option.value = node.nodeId;
    sourceSelect.append(option);
  }
  if ([...runtimeSelect.options].some((option) => option.value === previousRuntime)) runtimeSelect.value = previousRuntime;
  if ([...sourceSelect.options].some((option) => option.value === previousSource)) sourceSelect.value = previousSource;
  const selected = dashboardNodes.get(runtimeSelect.value);
  const defaultCwd = selected?.desiredAppServer?.defaultCwd ?? "";
  $("#agentRuntimeDefaultCwd").value = defaultCwd;
  if (!agent.threadId && !$("#conversationCwd").value.trim()) $("#conversationCwd").value = defaultCwd;
  if (selected && agent.socketNodeId !== selected.nodeId) {
    setAgentRuntimeState(`${selected.reportedAppServer?.status ?? "stopped"} · ${selected.hostname}`, selected.status === "online" ? "online" : "offline");
  }
  syncConversationSendUi();
}

async function saveAgentRuntimeDefaultCwd() {
  const nodeId = $("#agentRuntimeNode").value;
  const node = dashboardNodes.get(nodeId);
  if (!node) throw new Error("没有可配置的 Codex 运行节点");
  const defaultCwd = $("#agentRuntimeDefaultCwd").value.trim();
  const result = await api(`/v1/nodes/${nodeId}/desired-app-server`, {
    method: "PUT",
    body: JSON.stringify({
      running: node.desiredAppServer?.running === true,
      defaultCwd: defaultCwd || null,
    }),
  });
  node.desiredAppServer = result.desiredAppServer;
  dashboardNodes.set(nodeId, node);
  $("#agentRuntimeDefaultCwd").value = result.desiredAppServer.defaultCwd ?? "";
  if (!agent.threadId) $("#conversationCwd").value = result.desiredAppServer.defaultCwd ?? "";
  toast(defaultCwd ? "已保存该节点的默认工作目录" : "已清除该节点的默认工作目录");
}

function renderAgentThreads() {
  const list = clear($("#agentThreadList"));
  if (!agent.threads.length) {
    list.append(element("div", "agent-list-empty", "统一数据库里还没有会话"));
    return;
  }
  for (const thread of agent.threads) {
    const button = element("button", `agent-thread${thread.threadId === agent.threadId ? " active" : ""}`);
    button.type = "button";
    button.disabled = Boolean(agent.sendPromise);
    button.dataset.threadId = thread.threadId;
    button.append(
      element("strong", "", thread.title || "未命名会话"),
      element("span", "", `${thread.itemCount} items · ${when(thread.updatedAt)}`),
      element("small", "", thread.parentThreadId ? `subagent · ${thread.threadId}` : thread.threadId),
    );
    list.append(button);
  }
}

async function loadAgentThreads() {
  const response = await api("/v1/codex/threads?storeId=personal&limit=300");
  agent.threads = response.data ?? [];
  renderAgentThreads();
}

function renderLocalSessions() {
  const list = clear($("#localSessionList"));
  if (!agent.sessions.length) {
    list.append(element("div", "agent-list-empty", "没有发现本地 Codex 会话"));
    return;
  }
  for (const [index, session] of agent.sessions.entries()) {
    const card = element("article", "local-session");
    const copy = element("div");
    copy.append(
      element("strong", "", session.title || "未命名会话"),
      element("span", "", `${formatBytes(session.sizeBytes)} · ${when(session.modifiedAt)}`),
      element("small", "", session.threadId),
    );
    const button = element("button", session.import?.unchanged && session.import.status === "imported" ? "ghost" : "approve",
      session.import?.unchanged && session.import.status === "imported" ? "已导入" : "导入");
    button.type = "button";
    button.dataset.sessionIndex = String(index);
    button.disabled = session.import?.unchanged && session.import.status === "imported";
    card.append(copy, button);
    list.append(card);
  }
}

async function scanLocalSessions() {
  const nodeId = $("#sessionSourceNode").value;
  if (!nodeId) throw new Error("没有支持本地会话发现的节点");
  $("#sessionScanState").textContent = "正在扫描默认 CODEX_HOME…";
  const response = await api(`/v1/nodes/${nodeId}/codex-sessions`);
  agent.sessions = response.sessions ?? [];
  renderLocalSessions();
  $("#sessionScanState").textContent = `发现 ${agent.sessions.length} 个会话 · ${response.codexHomes?.length ?? 0} 个 CODEX_HOME`;
}

async function importLocalSession(index, button) {
  const session = agent.sessions[index];
  if (!session) return;
  button.disabled = true;
  button.textContent = "导入中…";
  try {
    const result = await api(`/v1/nodes/${$("#sessionSourceNode").value}/codex-session-imports`, {
      method: "POST", body: JSON.stringify({ path: session.path, storeId: "personal" }),
    });
    toast(`已导入 ${result.itemCount} 条记录`);
    await Promise.all([scanLocalSessions(), loadAgentThreads()]);
  } catch (error) {
    button.disabled = false;
    button.textContent = "重试";
    throw error;
  }
}

async function startAgentRuntime() {
  const nodeId = $("#agentRuntimeNode").value;
  if (!nodeId) throw new Error("没有可运行 Codex 的节点");
  closeAgentSocket();
  setAgentRuntimeState("正在启动受控 App Server…", "offline");
  await api(`/v1/codex/runtimes/${nodeId}/start`, { method: "POST", body: JSON.stringify({ storeId: "personal" }) });
  const deadline = Date.now() + 30_000;
  let node;
  let lastError = "";
  let errorSince = 0;
  while (Date.now() < deadline) {
    node = await api(`/v1/nodes/${nodeId}`);
    dashboardNodes.set(node.nodeId, node);
    if (node.reportedAppServer?.status === "running") break;
    const currentError = node.reportedAppServer?.lastError ?? "";
    if (currentError !== lastError) {
      lastError = currentError;
      errorSince = currentError ? Date.now() : 0;
    } else if (currentError && Date.now() - errorSince > 5_000) {
      throw new Error(currentError);
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  if (node?.reportedAppServer?.status !== "running") throw new Error("App Server 启动超时");
  await connectAgentSocket(nodeId);
}

async function stopAgentRuntime() {
  const nodeId = $("#agentRuntimeNode").value;
  if (!nodeId) return;
  closeAgentSocket();
  await api(`/v1/codex/runtimes/${nodeId}/stop`, { method: "POST", body: JSON.stringify({ storeId: "personal" }) });
  setAgentRuntimeState("已请求停止 App Server", "offline");
}

async function resumeAgentThread(threadId) {
  if (agent.sendPromise) return;
  if (!agent.socket || agent.socket.readyState !== WebSocket.OPEN) await startAgentRuntime();
  setConversationNotice("正在从 PostgreSQL 恢复会话…");
  const projectedThread = agent.threads.find((thread) => thread.threadId === threadId);
  agent.previousRuntimeNodeId = projectedThread?.runtimeNodeId ?? projectedThread?.sourceNodeId ?? null;
  const projectedCwd = typeof projectedThread?.cwd === "string" ? projectedThread.cwd.trim() : "";
  $("#conversationCwd").value = projectedCwd;
  const params = { threadId };
  if (projectedCwd) params.cwd = projectedCwd;
  const result = await rpc("thread/resume", params, 120_000);
  agent.threadId = result.thread.id;
  agent.threadRuntimeNodeId = agent.socketNodeId;
  resetAgentTranscript(agent.threadId);
  syncActiveTurnUi();
  $("#conversationTitle").textContent = result.thread.name || result.thread.preview || "Codex 会话";
  const resumedCwd = result.cwd ?? projectedCwd;
  $("#conversationMeta").textContent = `${agent.threadId} · ${result.model ?? "默认模型"} · ${resumedCwd || "默认目录"}`;
  $("#conversationCwd").value = resumedCwd ?? "";
  await loadAgentTranscript(agent.threadId, result.thread);
  renderAgentThreads();
  setConversationNotice();
}

function newAgentThread() {
  if (agent.sendPromise) return;
  agent.threadId = null;
  agent.newThreadRequestId = null;
  agent.newThreadRequestSignature = null;
  agent.threadRuntimeNodeId = null;
  agent.previousRuntimeNodeId = null;
  syncActiveTurnUi();
  resetAgentTranscript();
  $("#conversationTitle").textContent = "新会话";
  $("#conversationMeta").textContent = "第一条消息发送时在所选节点创建，并立即写入 PostgreSQL。";
  const node = dashboardNodes.get($("#agentRuntimeNode").value);
  $("#conversationCwd").value = node?.desiredAppServer?.defaultCwd ?? "";
  clear($("#conversationTrace")).append(element("div", "conversation-empty", "输入消息开始新的 Codex 会话。"));
  renderAgentThreads();
}

function nativeImageAttachment(file) {
  return /^(?:image\/(?:png|jpeg|webp|gif))$/i.test(file.type || nodeFileMimeType(file.name));
}

function safeAttachmentName(value, index) {
  const normalized = String(value || `attachment-${index + 1}`)
    .normalize("NFKC").replace(/[\\/:*?"<>|\u0000-\u001f]/g, "_").replace(/\s+/g, " ").trim();
  const limited = normalized.slice(0, 140).replace(/[. ]+$/g, "");
  return `${String(index + 1).padStart(2, "0")}-${limited || "attachment"}`;
}

function uploadDirectoryCandidates(node, cwd, batchId) {
  const suffix = ["mira-web-uploads", agent.threadId, batchId];
  const candidates = [];
  const add = (root) => {
    if (!root) return;
    let result = root;
    for (const part of suffix) result = joinPath(result, part);
    if (!candidates.includes(result)) candidates.push(result);
  };
  if (String(node?.platform).toLowerCase() === "windows") {
    const userHome = cwd.match(/^([A-Za-z]:\\Users\\[^\\]+)/i)?.[1];
    if (userHome) add(joinPath(joinPath(joinPath(userHome, "AppData"), "Local"), "Temp"));
    add(cwd);
  } else {
    add("/tmp");
    add(cwd);
  }
  return candidates;
}

async function prepareUploadDirectory(nodeId, cwd) {
  const node = dashboardNodes.get(nodeId);
  if (node?.capabilities?.files !== true) throw new Error("当前 Codex 运行节点没有提供文件能力，无法上传普通文件");
  const batchId = `${Date.now()}-${crypto.randomUUID?.() ?? Math.random().toString(36).slice(2)}`;
  let lastError = null;
  for (const candidate of uploadDirectoryCandidates(node, cwd, batchId)) {
    try {
      await invokeNode(nodeId, "file", { action: "mkdir", path: candidate, recursive: true }, 60_000);
      return candidate;
    } catch (error) { lastError = error; }
  }
  throw new Error(`无法在运行节点创建附件暂存目录：${lastError?.message ?? "没有可写目录"}`);
}

async function fileDataUrl(file) {
  const bytes = new Uint8Array(await file.arrayBuffer());
  return `data:${file.type || nodeFileMimeType(file.name)};base64,${bytesBase64(bytes)}`;
}

async function prepareTurnInput(text, attachments) {
  const images = attachments.filter(nativeImageAttachment);
  const files = attachments.filter((file) => !nativeImageAttachment(file));
  const annotations = [];
  const inputs = [];
  if (images.length) {
    annotations.push(`已附加图片：${images.map((file) => `\`${String(file.name).replaceAll("`", "'")}\``).join("、")}`);
  }
  if (files.length) {
    const nodeId = agent.socketNodeId;
    const cwd = $("#conversationCwd").value.trim();
    $("#conversationHint").textContent = `正在上传 ${files.length} 个文件到运行节点…`;
    const directory = await prepareUploadDirectory(nodeId, cwd);
    const staged = [];
    for (const [index, file] of files.entries()) {
      $("#conversationHint").textContent = `正在上传文件 ${index + 1} / ${files.length}…`;
      const path = joinPath(directory, safeAttachmentName(file.name, index));
      const bytes = new Uint8Array(await file.arrayBuffer());
      await invokeNode(nodeId, "file", {
        action: "write", path, encoding: "base64", content: bytesBase64(bytes), overwrite: false,
      }, 60_000);
      staged.push({ file, path });
    }
    annotations.push([
      "已附加文件（暂存在当前运行节点）：",
      ...staged.map(({ file, path }) => `- \`${String(file.name).replaceAll("`", "'")}\`：\`${path}\``),
    ].join("\n"));
  }
  const message = [text, ...annotations].filter(Boolean).join("\n\n");
  if (message) inputs.push({ type: "text", text: message });
  for (const image of images) inputs.push({ type: "image", url: await fileDataUrl(image) });
  return { inputs, message };
}

function renderComposerAttachments() {
  const target = clear($("#conversationAttachments"));
  target.classList.toggle("hidden", agent.attachments.length === 0);
  for (const [index, file] of agent.attachments.entries()) {
    const item = element("div", "conversation-attachment");
    item.append(
      element("span", "attachment-kind", nativeImageAttachment(file) ? "图片" : "文件"),
      element("span", "attachment-name", file.name),
      element("small", "", formatBytes(file.size)),
    );
    const remove = element("button", "attachment-remove", "×");
    remove.type = "button";
    remove.dataset.attachmentIndex = String(index);
    remove.setAttribute("aria-label", `移除 ${file.name}`);
    item.append(remove);
    target.append(item);
  }
}

function addComposerFiles(files) {
  let total = agent.attachments.reduce((sum, file) => sum + file.size, 0);
  const rejected = [];
  for (const file of files) {
    if (agent.attachments.length >= 8) { rejected.push(`${file.name}：最多 8 个附件`); continue; }
    if (file.size > maximumAttachmentBytes) { rejected.push(`${file.name}：超过 4 MiB`); continue; }
    if (total + file.size > maximumAttachmentTotalBytes) { rejected.push(`${file.name}：附件合计超过 8 MiB`); continue; }
    agent.attachments.push(file);
    total += file.size;
  }
  renderComposerAttachments();
  if (rejected.length) toast(rejected.join("；"));
}

async function sendAgentMessage(text, attachments = []) {
  if (!agent.socket || agent.socket.readyState !== WebSocket.OPEN) await startAgentRuntime();
  if (!agent.threadId) {
    const params = {
      approvalPolicy: "never",
      sandbox: "danger-full-access",
    };
    const cwd = $("#conversationCwd").value.trim();
    if (cwd) params.cwd = cwd;
    const signature = JSON.stringify(params);
    if (!agent.newThreadRequestId || agent.newThreadRequestSignature !== signature) {
      agent.newThreadRequestId = crypto.randomUUID();
      agent.newThreadRequestSignature = signature;
    }
    params.miraRequestId = agent.newThreadRequestId;
    const started = await rpc("thread/start", params, 120_000);
    agent.threadId = started.thread.id;
    agent.newThreadRequestId = null;
    agent.newThreadRequestSignature = null;
    agent.threadRuntimeNodeId = agent.socketNodeId;
    agent.previousRuntimeNodeId = null;
    $("#conversationTitle").textContent = "新会话";
    const startedCwd = started.cwd ?? cwd;
    $("#conversationMeta").textContent = `${agent.threadId} · ${started.model ?? "默认模型"} · ${startedCwd || "默认目录"}`;
    $("#conversationCwd").value = startedCwd ?? "";
  }
  const prepared = await prepareTurnInput(text, attachments);
  const optimistic = upsertTrace(`user-${Date.now()}`, "user", "你", prepared.message, "已发送");
  optimistic.dataset.pendingUser = "true";
  const turnThreadId = agent.threadId;
  let result;
  try {
    result = await rpc("turn/start", {
      threadId: turnThreadId,
      input: prepared.inputs,
      approvalPolicy: "never",
    }, 120_000);
  } catch (error) {
    if (optimistic.dataset.pendingUser === "true") optimistic.remove();
    throw error;
  }
  if (result.turn?.id) {
    agent.activeTurns.set(turnThreadId, result.turn.id);
    agent.turnThreads.set(result.turn.id, turnThreadId);
  }
  syncActiveTurnUi();
}

async function openAgentConsole() {
  show("agentView");
  await Promise.all([refreshAgentNodes(), loadAgentThreads()]);
}

async function leaveAgentConsole() {
  closeAgentSocket();
  show("dashboardView");
  await loadDashboard();
}

$("#loginForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.currentTarget.querySelector("button");
  button.disabled = true;
  $("#loginError").classList.add("hidden");
  try {
    const result = await api("/v1/admin/login", { method: "POST", body: JSON.stringify({ username: $("#username").value, password: $("#password").value }) });
    csrfToken = result.csrfToken;
    $("#password").value = "";
    show("dashboardView");
    await loadDashboard();
  } catch (error) {
    $("#loginError").textContent = error.message;
    $("#loginError").classList.remove("hidden");
  } finally {
    button.disabled = false;
  }
});

$("#logoutButton").addEventListener("click", async () => {
  if (workspace.sessionId && !confirm("退出会断开当前远程 Shell。继续吗？")) return;
  try {
    await disconnectShell({ quiet: true });
    await api("/v1/admin/logout", { method: "POST", body: "{}" });
  } finally {
    csrfToken = null;
    closeAgentSocket();
    disposeTerminal();
    workspace.node = null;
    show("loginView");
  }
});

$("#agentConsoleButton").addEventListener("click", () => openAgentConsole().catch((error) => toast(error.message)));
$("#agentBack").addEventListener("click", () => leaveAgentConsole().catch((error) => toast(error.message)));
$("#agentRefresh").addEventListener("click", () => Promise.all([refreshAgentNodes(), loadAgentThreads()]).then(() => toast("Agent 状态已刷新")).catch((error) => toast(error.message)));
$("#agentRuntimeStart").addEventListener("click", () => startAgentRuntime().catch((error) => {
  setAgentRuntimeState(`启动失败：${error.message}`, "offline");
  setConversationNotice(error.message, "error");
}));
$("#agentRuntimeStop").addEventListener("click", () => stopAgentRuntime().catch((error) => toast(error.message)));
$("#agentRuntimeSaveCwd").addEventListener("click", () => saveAgentRuntimeDefaultCwd().catch((error) => toast(error.message)));
$("#agentRuntimeNode").addEventListener("change", () => {
  if (agent.socketNodeId !== $("#agentRuntimeNode").value) closeAgentSocket();
  const node = dashboardNodes.get($("#agentRuntimeNode").value);
  $("#agentRuntimeDefaultCwd").value = node?.desiredAppServer?.defaultCwd ?? "";
  if (!agent.threadId) $("#conversationCwd").value = node?.desiredAppServer?.defaultCwd ?? "";
  setAgentRuntimeState(`${node?.reportedAppServer?.status ?? "stopped"} · ${node?.hostname ?? ""}`, node?.status === "online" ? "online" : "offline");
  syncConversationSendUi();
});
$("#agentNewThread").addEventListener("click", newAgentThread);
$("#agentThreadList").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-thread-id]");
  if (button) resumeAgentThread(button.dataset.threadId).catch((error) => setConversationNotice(error.message, "error"));
});
$("#conversationTrace").addEventListener("click", (event) => {
  const file = event.target.closest("[data-node-file-path]");
  if (file) {
    event.preventDefault();
    openNodeFile(file.dataset.nodeFilePath, Number(file.dataset.nodeFileLine) || null).catch((error) => {
      $("#nodeFileMeta").textContent = "读取失败";
      $("#nodeFileLoading").textContent = error.message;
      toast(error.message);
    });
    return;
  }
  if (event.target.closest("button[data-load-older]")) {
    loadOlderAgentTranscript().catch((error) => toast(error.message));
  }
});
$("#sessionScan").addEventListener("click", () => scanLocalSessions().catch((error) => {
  $("#sessionScanState").textContent = `扫描失败：${error.message}`;
}));
$("#localSessionList").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-session-index]");
  if (button) importLocalSession(Number(button.dataset.sessionIndex), button).catch((error) => toast(error.message));
});
$("#conversationForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  if (agent.sendPromise) return;
  const text = $("#conversationInput").value.trim();
  const attachments = [...agent.attachments];
  if (!text && !attachments.length) return;
  const operation = sendAgentMessage(text, attachments);
  agent.sendPromise = operation;
  syncConversationSendUi();
  try {
    await operation;
    $("#conversationInput").value = "";
    agent.attachments = [];
    renderComposerAttachments();
    $("#conversationHint").textContent = "可粘贴或拖入 · 单个 4 MiB，合计 8 MiB";
  } catch (error) {
    setConversationNotice(error.message, "error");
    $("#conversationHint").textContent = "发送失败；附件仍保留，可重试";
  } finally {
    if (agent.sendPromise === operation) agent.sendPromise = null;
    syncConversationSendUi();
  }
});
$("#conversationInput").addEventListener("keydown", (event) => {
  if ((event.ctrlKey || event.metaKey) && event.key === "Enter") {
    event.preventDefault();
    if (!agent.sendPromise) $("#conversationForm").requestSubmit();
  }
});
$("#conversationAttach").addEventListener("click", () => $("#conversationFileInput").click());
$("#conversationFileInput").addEventListener("change", (event) => {
  addComposerFiles(event.target.files ?? []);
  event.target.value = "";
});
$("#conversationAttachments").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-attachment-index]");
  if (!button) return;
  agent.attachments.splice(Number(button.dataset.attachmentIndex), 1);
  renderComposerAttachments();
});
$("#conversationInput").addEventListener("paste", (event) => {
  const files = [...(event.clipboardData?.files ?? [])];
  if (!files.length) return;
  event.preventDefault();
  addComposerFiles(files);
});
$("#conversationDropZone").addEventListener("dragover", (event) => {
  if (!event.dataTransfer?.types.includes("Files")) return;
  event.preventDefault();
  event.currentTarget.classList.add("dragging");
});
$("#conversationDropZone").addEventListener("dragleave", (event) => event.currentTarget.classList.remove("dragging"));
$("#conversationDropZone").addEventListener("drop", (event) => {
  event.preventDefault();
  event.currentTarget.classList.remove("dragging");
  addComposerFiles(event.dataTransfer?.files ?? []);
});
$("#nodeFileClose").addEventListener("click", () => $("#nodeFileDialog").close());
$("#nodeFileDialog").addEventListener("close", resetNodeFileDialog);
$("#agentInterrupt").addEventListener("click", () => {
  if (!agent.threadId || !agent.turnId) return;
  rpc("turn/interrupt", { threadId: agent.threadId, turnId: agent.turnId }).catch((error) => toast(error.message));
});
$("#conversationClear").addEventListener("click", () => {
  resetAgentTranscript(agent.threadId);
  clear($("#conversationTrace")).append(element("div", "conversation-empty", "当前视图已清空；数据库中的会话没有删除。"));
});

$("#refreshButton").addEventListener("click", () => loadDashboard().then(() => toast("状态已刷新")).catch((error) => toast(error.message)));
$("#showRevoked").addEventListener("change", () => loadDashboard().catch((error) => toast(error.message)));
$("#workspaceBack").addEventListener("click", () => leaveWorkspace().catch((error) => toast(error.message)));
$("#workspaceRefresh").addEventListener("click", () => refreshWorkspace().then(() => toast("节点已刷新")).catch((error) => toast(error.message)));
$("#previewClose").addEventListener("click", clearPreview);

$("#fileRootSelect").addEventListener("change", (event) => {
  workspace.currentRoot = workspace.roots[Number(event.target.value)];
  workspace.currentPath = workspace.currentRoot?.configured;
  if (workspace.currentPath) loadDirectory(workspace.currentPath);
});
$("#fileRefresh").addEventListener("click", () => {
  if (workspace.currentPath) loadDirectory(workspace.currentPath);
  else loadFileRoots(true).catch((error) => toast(error.message));
});
$("#fileBreadcrumbs").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-path]");
  if (button) loadDirectory(button.dataset.path);
});
$("#fileList").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-path]");
  if (button) inspectFile(button.dataset.path, button.dataset.type);
});

$("#shellConnect").addEventListener("click", () => connectShell().catch((error) => {
  setShellConnected(false, `连接失败：${error.message}`);
  toast(error.message);
}));
$("#shellDisconnect").addEventListener("click", () => disconnectShell());
$("#shellInterrupt").addEventListener("click", () => enqueuePTYInput("\u0003", { immediate: true }));
$("#shellClear").addEventListener("click", () => {
  const terminal = ensureTerminal();
  terminal.clear();
  terminal.write("\x1b[2J\x1b[H");
  if (workspace.sessionId) void enqueuePTYInput("\u000c", { immediate: true });
  terminal.focus();
});

$("#debugToolList").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-debug-tool]");
  if (button) selectDynamicTool(button.dataset.debugTool);
});
$("#debugActions").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-debug-action]");
  if (button && workspace.selectedDynamicTool) selectDynamicTool(workspace.selectedDynamicTool.name, button.dataset.debugAction);
});
$("#debugPresetFields").addEventListener("input", syncDebugJsonFromPresetFields);
$("#debugPresetFields").addEventListener("change", syncDebugJsonFromPresetFields);
$("#debugArguments").addEventListener("input", () => {
  const tool = workspace.selectedDynamicTool;
  if (!tool) return;
  const action = selectedDebugAction(tool);
  for (const button of document.querySelectorAll("[data-debug-action]")) button.classList.toggle("active", button.dataset.debugAction === action);
});
$("#debugForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  const tool = workspace.selectedDynamicTool;
  if (!tool) return;
  const button = $("#debugRun");
  const output = $("#debugResult");
  const meta = $("#debugResultMeta");
  const image = $("#debugResultImage");
  let arguments_;
  try {
    arguments_ = $("#debugAdvanced").open ? JSON.parse($("#debugArguments").value) : debugArgumentsFromPresetFields();
    if (!arguments_ || typeof arguments_ !== "object" || Array.isArray(arguments_)) throw new Error("arguments 必须是 JSON object");
  } catch (error) {
    meta.textContent = "参数错误";
    meta.className = "debug-result-meta result-fail";
    output.textContent = error.message;
    return;
  }
  const timeoutMs = Number($("#debugTimeout").value);
  button.disabled = true;
  image.classList.add("hidden");
  image.removeAttribute("src");
  meta.textContent = "调用中…";
  meta.className = "debug-result-meta";
  output.textContent = `${workspace.dynamicNamespace?.name ?? "home_nodes"}.${tool.name} 正在所选节点上执行…`;
  const startedAt = performance.now();
  try {
    const result = await callDynamicTool(tool.name, arguments_, timeoutMs);
    renderDebugResult(result, tool.name, arguments_, performance.now() - startedAt);
  } catch (error) {
    meta.textContent = `失败 · ${Math.round(performance.now() - startedAt)} ms · ${error.code ?? `HTTP ${error.status ?? "—"}`}`;
    meta.className = "debug-result-meta result-fail";
    output.textContent = JSON.stringify({ error: error.message, code: error.code ?? null, status: error.status ?? null }, null, 2);
  } finally {
    button.disabled = false;
  }
});

document.addEventListener("click", (event) => {
  const tab = event.target.closest("button[data-workspace-tab]");
  if (tab) {
    activateWorkspaceTab(tab.dataset.workspaceTab);
    return;
  }
  const button = event.target.closest("button[data-action]");
  if (!button) return;
  button.disabled = true;
  let task;
  if (button.dataset.action === "revoke") task = revoke(button.dataset.id);
  else if (button.dataset.action === "workspace") task = openWorkspace(button.dataset.id);
  else task = decide(button.dataset.id, button.dataset.action);
  task.catch((error) => toast(error.message)).finally(() => { button.disabled = false; });
});

async function bootstrap() {
  try {
    const health = await api("/healthz");
    $("#healthDot").classList.add("ok");
    $("#healthText").textContent = `Server ${health.version ?? ""} 在线 · PostgreSQL`;
    const origin = window.location.origin;
    $("#installServer").textContent = origin;
    $("#installLinux").textContent = `curl -fsSL https://raw.githubusercontent.com/ssine/mira/main/scripts/install.sh | sh -s -- --server '${origin}'`;
    $("#installWindows").textContent = `& ([scriptblock]::Create((irm https://raw.githubusercontent.com/ssine/mira/main/scripts/install.ps1))) -Server '${origin}'`;
    if (/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(health.version ?? "")) {
      $("#installAndroid").href = `https://github.com/ssine/mira/releases/download/v${health.version}/mira_${health.version}_android_arm64.apk`;
    }
    if (!health.adminConfigured) { show("setupView"); return; }
    try {
      const session = await api("/v1/admin/session");
      csrfToken = session.csrfToken;
      show("dashboardView");
      await loadDashboard();
    } catch {
      show("loginView");
    }
  } catch {
    $("#healthDot").classList.add("bad");
    $("#healthText").textContent = "Server 不可用";
    show("loginView");
  }
}

for (const button of document.querySelectorAll("[data-copy-install]")) {
  button.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText($("#" + button.dataset.copyInstall).textContent);
      toast("安装命令已复制");
    } catch { toast("浏览器未允许复制，请选中命令手动复制"); }
  });
}

void bootstrap();

import { FitAddon } from "/vendor/xterm-addon-fit.js";
import { Terminal } from "/vendor/xterm.js";
import DOMPurify from "/vendor/dompurify.js";
import { marked } from "/vendor/marked.js";
import { toolItemView, activitySummary, summarizeActivities, activityStatus, formatActivityDuration, reasoningText, reasoningParts, reasoningHeading } from "/trace-activity.js";
import { ReplyProgress } from "/conversation-progress.js";
import { initializePwa, rememberAppRoute, clearAppRoute } from "/pwa.js";

marked.setOptions({ gfm: true, breaks: false });

const $ = (selector) => document.querySelector(selector);
let csrfToken = null;
let csrfRefreshPromise = null;
let dashboardNodes = new Map();

const themeStorageKey = "mira.theme";
const agentThreadDrawerWide = window.matchMedia("(min-width: 1100px)");
let agentThreadDrawerOpen = agentThreadDrawerWide.matches;
let browserRouteEpoch = 0;

function writeBrowserRoute(view, threadId = null, { replace = false } = {}) {
  const url = new URL(window.location.href);
  url.searchParams.delete("thread");
  url.searchParams.delete("view");
  if (view === "agent" && threadId) url.searchParams.set("thread", threadId);
  else if (view !== "nodes") url.searchParams.set("view", view);
  if (url.href === window.location.href) return;
  browserRouteEpoch++;
  window.history[replace ? "replaceState" : "pushState"](null, "", url);
  rememberAppRoute();
}

async function restoreBrowserRoute() {
  const epoch = ++browserRouteEpoch;
  const url = new URL(window.location.href);
  const threadId = url.searchParams.get("thread");
  const view = threadId ? "agent" : url.searchParams.get("view");
  rememberAppRoute();
  if (agent.sendPromise) {
    writeBrowserRoute("agent", agent.threadId, { replace: true });
    return;
  }
  if (view === "agent") {
    show("agentView");
    await Promise.all([refreshAgentNodes(), loadAgentThreads()]);
    if (epoch !== browserRouteEpoch) return;
    if (threadId) {
      if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(threadId)) {
        setConversationNotice("会话链接中的 ID 无效。", "error");
        return;
      }
      await resumeAgentThread(threadId, { updateRoute: false });
    } else newAgentThread({ updateRoute: false });
  } else if (view === "runtime") {
    show("runtimeView");
    await Promise.all([refreshAgentNodes(), loadAgentThreads()]);
  } else {
    stopAgentRecovery();
    show("dashboardView");
    await loadDashboard();
  }
}

function terminalTheme() {
  if (document.documentElement.dataset.theme === "dark") {
    return {
      background: "#17191c", foreground: "#e9edf2", cursor: "#6cb8f6", cursorAccent: "#17191c",
      selectionBackground: "#294b68", black: "#17191c", brightBlack: "#77818d",
      green: "#75c890", brightGreen: "#9acaac",
    };
  }
  return {
    background: "#1f2226", foreground: "#eef2f6", cursor: "#6cb8f6", cursorAccent: "#1f2226",
    selectionBackground: "#315b7b", black: "#17191c", brightBlack: "#83909d",
    green: "#72c98f", brightGreen: "#a0d9b3",
  };
}

function syncThemeControl() {
  const dark = document.documentElement.dataset.theme === "dark";
  document.querySelector('meta[name="theme-color"]').content = dark ? "#1f2226" : "#ffffff";
  const button = $("#themeToggle");
  button.setAttribute("aria-label", `切换到${dark ? "浅色" : "深色"}主题`);
  button.querySelector(".topbar-action-label").textContent = dark ? "浅色" : "深色";
  const agentButton = $("#agentThemeToggle");
  if (agentButton) agentButton.setAttribute("aria-label", `切换到${dark ? "浅色" : "深色"}主题`);
  if (workspace.terminal) workspace.terminal.options.theme = terminalTheme();
}

function toggleTheme() {
  const theme = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
  document.documentElement.dataset.theme = theme;
  try { localStorage.setItem(themeStorageKey, theme); } catch { /* unavailable storage */ }
  syncThemeControl();
}

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
  loadedThreadIds: new Set(),
  resumePromises: new Map(),
  runtimePromise: null,
  recoveryPromise: null,
  connectionWanted: false,
  socketInitialized: false,
  reconnectTimer: null,
  heartbeatTimer: null,
  reconnectAttempt: 0,
  lastSocketMessageAt: 0,
  selectionEpoch: 0,
  transcriptRequest: 0,
  liveRevision: 0,
  runtimeStartEpoch: 0,
  pending: new Map(),
  requestId: 0,
  threadId: null,
  turnId: null,
  interruptRequests: new Set(),
  projectOpen: new Map(),
  draftProject: null,
  menuThreadId: null,
  rename: null,
  forkPromise: null,
  forkRequests: new Map(),
  showArchived: false,
  threadActionPromise: null,
  deleteTarget: null,
  activeTurns: new Map(),
  turnStateRequests: new Map(),
  turnThreads: new Map(),
  turnTimings: new Map(),
  turnActivity: new Map(),
  sessions: [],
  sessionNodeId: null,
  sessionScanEpoch: 0,
  sessionVisibleLimit: 40,
  threads: [],
  transcriptThreadId: null,
  transcriptGeneration: null,
  transcriptItems: [],
  transcriptCursor: null,
  transcriptTotal: 0,
  transcriptLoadingOlder: false,
  transcriptOlderError: null,
  transcriptTailVersion: null,
  transcriptReconciliations: new Map(),
  // Ephemeral diagnostics, not a second transcript store. Keep failures across
  // history refresh/reconnect/selection until explicitly dismissed (or page close).
  diagnostics: new Map(),
  threadRuntimeNodeId: null,
  previousRuntimeNodeId: null,
  attachments: [],
  fileObjectUrl: null,
  sendPromise: null,
  replySubmission: null,
  uploadController: null,
  fileReadController: null,
  filePreview: null,
  sessionImportController: null,
  newThreadRequestId: null,
  newThreadRequestSignature: null,
};

const transcriptPageSize = 60;
const replyProgress = new ReplyProgress();
let replyProgressTimer = null;
const traceStreamRenders = new WeakMap();

function readableErrorMessage(value, depth = 0) {
  if (depth > 6 || value === null || value === undefined) return "";
  if (typeof value === "string") {
    const text = value.trim();
    if (!text.startsWith("{") && !text.startsWith("[")) return value;
    try { return readableErrorMessage(JSON.parse(text), depth + 1) || value; } catch { return value; }
  }
  if (typeof value !== "object" || Array.isArray(value)) return "";
  return readableErrorMessage(value.error, depth + 1) ||
    readableErrorMessage(value.message, depth + 1) ||
    readableErrorMessage(value.detail, depth + 1);
}

function renderReplyProgress() {
  const entry = replyProgress.current(agent.threadId);
  $("#conversationProgress").classList.toggle("hidden", !entry);
  if (entry) {
    $("#conversationProgressText").textContent = entry.phase;
    $("#conversationProgressTime").textContent = `${Math.max(0, Math.floor((Date.now() - entry.startedAt) / 1000))} 秒`;
  }
  if (entry && !replyProgressTimer) replyProgressTimer = setInterval(renderReplyProgress, 1000);
  if (!entry && replyProgressTimer) { clearInterval(replyProgressTimer); replyProgressTimer = null; }
  renderTurnActivity(Boolean(entry));
}

function renderTurnActivity(submitting) {
  const turnId = agent.activeTurns.get(agent.threadId);
  const phase = agent.turnActivity.get(turnId) ?? "working";
  const visible = Boolean(agent.activeTurns.has(agent.threadId) && !submitting && phase !== "failed" && agent.socket?.readyState === WebSocket.OPEN);
  const status = $("#conversationActivity");
  if (status.classList.contains("hidden") === visible) {
    const follow = traceNearBottom();
    status.classList.toggle("hidden", !visible);
    if (follow) scrollTraceToBottom();
  }
  const text = phase === "replying" ? "Codex 正在回复…" : phase === "tool" ? "Codex 正在调用工具…" : "Codex 仍在处理中…";
  const label = $("#conversationActivityText");
  if (visible && label.textContent !== text) label.textContent = text;
}

function observeTurnActivity(method, params) {
  const turnId = params.turn?.id ?? params.turnId;
  if (!turnId) return;
  if (method === "turn/completed") { agent.turnActivity.delete(turnId); return; }
  if (agent.turnTimings.get(turnId)?.completedAt) return;
  let phase;
  if (method === "turn/started" || method === "item/completed") phase = "working";
  else if (method === "item/agentMessage/delta" && params.delta) phase = "replying";
  else if (method === "item/started") {
    const type = String(params.item?.type ?? "").replaceAll("_", "").toLowerCase();
    phase = ["commandexecution", "filechange", "mcptoolcall", "dynamictoolcall", "websearch", "collabagenttoolcall"].includes(type) ? "tool" : "working";
  } else if (method === "error" && !params.willRetry) phase = "failed";
  if (phase) agent.turnActivity.set(turnId, phase);
}

function updateReplyProgress(entry, values) {
  replyProgress.update(entry, values);
  renderReplyProgress();
}
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
  for (const id of ["loginView", "setupView", "dashboardView", "workspaceView", "agentView", "runtimeView"]) {
    $("#" + id).classList.toggle("hidden", id !== view);
  }
  const authenticated = ["dashboardView", "workspaceView", "agentView", "runtimeView"].includes(view);
  $("#logoutButton").classList.toggle("hidden", !authenticated);
  $("#globalNav").classList.toggle("hidden", !authenticated);
  $("#globalNodes").classList.toggle("active", ["dashboardView", "workspaceView"].includes(view));
  $("#globalNodes").setAttribute("aria-current", ["dashboardView", "workspaceView"].includes(view) ? "page" : "false");
  $("#globalAgent").classList.toggle("active", view === "agentView");
  $("#globalAgent").setAttribute("aria-current", view === "agentView" ? "page" : "false");
  $("#globalRuntime").classList.toggle("active", view === "runtimeView");
  $("#globalRuntime").setAttribute("aria-current", view === "runtimeView" ? "page" : "false");
  document.body.dataset.view = view;
  document.title = view === "agentView" ? `${$("#conversationTitle").textContent} · Mira` : "Mira";
  if (view === "agentView") setAgentThreadDrawer(agentThreadDrawerOpen, { focus: false });
  else if (!agentThreadDrawerWide.matches) setAgentThreadDrawer(false);
}

function setAgentThreadDrawer(open, { focus = true } = {}) {
  const drawer = $("#agentThreadDrawer");
  const backdrop = $("#agentThreadDrawerBackdrop");
  const toggle = $("#agentThreadDrawerToggle");
  if (!drawer || !backdrop || !toggle) return;
  agentThreadDrawerOpen = open;
  drawer.closest(".chat-shell").classList.toggle("sidebar-open", open);
  drawer.classList.toggle("open", open);
  drawer.toggleAttribute("inert", !open);
  drawer.setAttribute("aria-hidden", String(!open));
  const overlay = open && !agentThreadDrawerWide.matches;
  backdrop.classList.toggle("open", overlay);
  backdrop.tabIndex = overlay ? 0 : -1;
  toggle.setAttribute("aria-expanded", String(open));
  toggle.setAttribute("aria-label", open ? "关闭会话侧边栏" : "打开会话侧边栏");
  if (open && focus) $("#agentThreadDrawerClose").focus({ preventScroll: true });
  else if (!open && document.body.dataset.view === "agentView" && document.activeElement && drawer.contains(document.activeElement)) {
    toggle.focus({ preventScroll: true });
  }
}

function closeAgentThreadDrawerOnMobile() {
  if (!agentThreadDrawerWide.matches) setAgentThreadDrawer(false);
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
    theme: terminalTheme(),
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

function setConversationTitle(title) {
  $("#conversationTitle").textContent = title;
  if (document.body.dataset.view === "agentView") document.title = `${title} · Mira`;
}

function setConversationMeta(cwd, model) {
  const directory = element("span", "conversation-directory", cwd || "默认目录");
  const runtimeModel = element("span", "conversation-model", model || "默认模型");
  directory.title = directory.textContent;
  runtimeModel.title = runtimeModel.textContent;
  $("#conversationMeta").replaceChildren(directory, element("span", "conversation-meta-separator", "·"), runtimeModel);
}

function setAgentRuntimeState(message, status = "offline") {
  $("#agentRuntimeState").textContent = message;
  $("#agentRuntimeBadge").textContent = status;
  $("#agentRuntimeBadge").className = `badge ${status}`;
  const indicator = $("#conversationConnection");
  indicator.textContent = status === "online" ? "" : message;
  indicator.classList.toggle("hidden", status === "online" || !agent.connectionWanted);
}

function agentRecoveryAllowed() {
  return agent.connectionWanted && !document.hidden && navigator.onLine !== false &&
    ["agentView", "runtimeView"].includes(document.body.dataset.view);
}

function scheduleAgentRecovery(delay = null) {
  clearTimeout(agent.reconnectTimer);
  if (!agentRecoveryAllowed()) return;
  const backoff = Math.min(30_000, 500 * 2 ** Math.min(agent.reconnectAttempt++, 6));
  agent.reconnectTimer = setTimeout(() => {
    agent.reconnectTimer = null;
    void recoverAgentSession();
  }, delay ?? backoff + Math.random() * 250);
}

function stopAgentRecovery({ resetTurnState = false } = {}) {
  agent.connectionWanted = false;
  agent.selectionEpoch++;
  agent.runtimePromise = null;
  clearTimeout(agent.reconnectTimer);
  clearTimeout(agent.heartbeatTimer);
  closeAgentSocket({ resetTurnState });
}

function scheduleAgentHeartbeat() {
  clearTimeout(agent.heartbeatTimer);
  if (!agentRecoveryAllowed()) return;
  agent.heartbeatTimer = setTimeout(() => {
    void recoverAgentSession({ probe: true, refresh: false });
  }, 25_000);
}

async function recoverAgentSession({ probe = false, refresh = true } = {}) {
  if (!agentRecoveryAllowed()) return;
  if (agent.recoveryPromise) return agent.recoveryPromise;
  const epoch = agent.selectionEpoch;
  const operation = (async () => {
    try {
      // A suspended mobile socket can remain OPEN after its network is gone.
      // Check the end-to-end path before trusting that browser state.
      if (probe && agent.socketInitialized && agent.socket?.readyState === WebSocket.OPEN) {
        try { await rpc("thread/loaded/list", { limit: 1 }, 8_000); }
        catch { closeAgentSocket(); }
      }
      if (!agentRecoveryAllowed() || epoch !== agent.selectionEpoch) return;
      const threadId = agent.threadId;
      const history = refresh && threadId
        ? loadAgentTranscript(threadId, null, { preserveLoaded: true, anchorBottom: traceNearBottom() })
        : Promise.resolve();
      const connection = (async () => {
        await startAgentRuntime({ allowStart: false });
        if (epoch !== agent.selectionEpoch || !agentRecoveryAllowed()) return;
        if (threadId && !agent.loadedThreadIds.has(threadId)) await resumeAgentThreadOnSocket(threadId);
        else if (threadId && agent.activeTurns.has(threadId)) await refreshActiveTurn(threadId, agent.socket);
      })();
      const results = await Promise.allSettled([history, connection]);
      const failure = results.find((result) => result.status === "rejected");
      if (failure) throw failure.reason;
      agent.reconnectAttempt = 0;
      scheduleAgentHeartbeat();
    } catch (error) {
      if (epoch !== agent.selectionEpoch || !agent.connectionWanted) return;
      if (error.status === 401 || error.status === 403) {
        stopAgentRecovery();
        setConversationNotice("登录已过期，请重新登录。输入内容仍保留。", "warning");
        return;
      }
      setAgentRuntimeState("连接暂时中断，正在自动重连…", "offline");
      scheduleAgentRecovery();
    }
  })();
  agent.recoveryPromise = operation;
  try { await operation; }
  finally { if (agent.recoveryPromise === operation) agent.recoveryPromise = null; }
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
  $("#conversationMenuToggle").classList.toggle("hidden", !agent.threadId);
  syncConversationSendUi();
  renderReplyProgress();
}

function syncConversationSendUi() {
  const selectedNode = $("#agentRuntimeNode")?.value;
  const busy = Boolean(agent.sendPromise || agent.forkPromise || agent.threadActionPromise);
  $("#threadFork").disabled = busy;
  $("#threadArchive").disabled = busy;
  $("#threadDelete").disabled = busy;
  const running = agent.activeTurns.has(agent.threadId);
  const stopping = agent.interruptRequests.has(JSON.stringify([agent.threadId, agent.turnId]));
  $("#conversationSend").classList.toggle("hidden", running);
  $("#conversationSend").disabled = busy || !selectedNode;
  const stop = $("#agentInterrupt");
  stop.classList.toggle("hidden", !running);
  const connected = agent.socketInitialized && agent.socket?.readyState === WebSocket.OPEN;
  stop.disabled = stopping || !connected || !agent.turnId || !agent.loadedThreadIds.has(agent.threadId);
  stop.title = stopping ? "正在停止…" : !connected ? "正在重连，连接恢复后可停止" : !agent.turnId ? "正在确认运行状态…" : "停止 Agent";
  stop.setAttribute("aria-label", stop.title);
  $("#agentRuntimeNode").disabled = busy;
  $("#agentNewThread").disabled = busy;
  $("#agentNewProject").disabled = busy;
  $("#conversationAttach").disabled = busy;
  $("#conversationCwd").disabled = busy;
  for (const button of $("#conversationAttachments").querySelectorAll("button")) button.disabled = busy;
  for (const button of $("#agentThreadList").querySelectorAll("button[data-thread-id]")) button.disabled = busy;
  for (const button of $("#agentThreadList").querySelectorAll("button[data-project-new]")) button.disabled = busy || !button.dataset.projectNode;
}

function closeAgentSocket({ preserveSubmission = false, resetTurnState = false } = {}) {
  replyProgress.clear(preserveSubmission ? agent.replySubmission : null);
  agent.runtimeStartEpoch++;
  const socket = agent.socket;
  agent.socket = null;
  agent.socketNodeId = null;
  agent.socketInitialized = false;
  agent.loadedThreadIds.clear();
  agent.resumePromises.clear();
  clearTimeout(agent.heartbeatTimer);
  for (const pending of agent.pending.values()) pending.reject(new Error("App Server connection closed"));
  agent.pending.clear();
  agent.turnStateRequests.clear();
  // A transport disconnect does not end the Codex turn. Keep the stop control
  // visible (disabled offline) until a completion event or a fresh turn read.
  if (resetTurnState) {
    agent.activeTurns.clear();
    agent.turnThreads.clear();
    agent.turnTimings.clear();
    agent.turnActivity.clear();
  }
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
  const button = element("button", "trace-copy");
  button.type = "button";
  button.title = "复制消息原文（保留 Markdown）";
  button.setAttribute("aria-label", "复制这条消息的原文");
  button.innerHTML = '<svg viewBox="0 0 16 16" aria-hidden="true"><rect x="5.25" y="5.25" width="8" height="8" rx="1.25"></rect><path d="M10.75 5.25V3.5c0-.69-.56-1.25-1.25-1.25h-6c-.69 0-1.25.56-1.25 1.25v6c0 .69.56 1.25 1.25 1.25h1.75"></path></svg>';
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

const traceClockFormatter = new Intl.DateTimeFormat("zh-CN", {
  hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false,
});

function traceClock(value) {
  if (value === null || value === undefined || value === "") return "";
  const date = value instanceof Date ? value : new Date(value);
  return Number.isFinite(date.getTime()) ? traceClockFormatter.format(date) : "";
}

function setTraceMetadata(card, options = {}) {
  const footer = card.querySelector(".trace-footer");
  if (!footer) return;
  if (Object.hasOwn(options, "completedAt")) card._miraCompletedAt = options.completedAt;
  if (Object.hasOwn(options, "elapsedMs")) card._miraElapsedMs = options.elapsedMs;
  if (Object.hasOwn(options, "timingScope")) card._miraTimingScope = options.timingScope;
  if (Object.hasOwn(options, "elapsedApproximate")) card._miraElapsedApproximate = options.elapsedApproximate;
  for (const field of ["turnCompletedAt", "turnElapsedMs", "turnElapsedApproximate"]) {
    if (options[field] != null) card[`_mira${field[0].toUpperCase()}${field.slice(1)}`] = options[field];
  }
  const completed = footer.querySelector(".trace-completed");
  const elapsed = footer.querySelector(".trace-elapsed");
  const clock = card._miraTimingScope === "turn" ? "" : traceClock(card._miraCompletedAt);
  completed.textContent = clock;
  completed.title = card._miraTimingScope === "recorded" ? "消息记录时间" : "消息时间";
  completed.hidden = !clock;
  elapsed.textContent = "";
  elapsed.hidden = true;
}

function refreshTurnFooters(turnId = null) {
  const turns = new Map();
  for (const card of $("#conversationTrace").querySelectorAll(".trace-card.assistant")) {
    const id = card.dataset.turnId;
    if (turnId && id !== turnId) continue;
    setTraceMetadata(card);
    if (!id || !card.querySelector(".trace-body")._miraSource) continue;
    if (!turns.has(id)) turns.set(id, []);
    turns.get(id).push(card);
  }
  for (const [id, cards] of turns) {
    const last = cards.at(-1);
    const stored = cards.findLast((card) => Number.isFinite(card._miraTurnElapsedMs));
    const legacy = cards.findLast((card) => card._miraTimingScope === "turn" && Number.isFinite(card._miraElapsedMs));
    const live = agent.turnTimings.get(id);
    const elapsedMs = stored?._miraTurnElapsedMs ?? (live?.completedAt ? live.elapsedMs : null) ?? legacy?._miraElapsedMs;
    const completedAt = stored?._miraTurnCompletedAt ?? live?.completedAt ?? legacy?._miraCompletedAt;
    const approximate = stored?._miraTurnElapsedApproximate ?? live?.elapsedApproximate ?? legacy?._miraElapsedApproximate;
    // Retain the aggregate through a disconnect or a refresh before storage catches up.
    setTraceMetadata(last, { turnCompletedAt: completedAt, turnElapsedMs: elapsedMs, turnElapsedApproximate: approximate });
    const elapsed = last.querySelector(".trace-elapsed");
    const duration = formatActivityDuration(elapsedMs);
    elapsed.textContent = duration ? `本轮总耗时${approximate ? "约" : ""} ${duration}` : "";
    elapsed.hidden = !duration;
    const clock = last.querySelector(".trace-completed");
    if (clock.hidden && traceClock(completedAt)) {
      clock.textContent = traceClock(completedAt);
      clock.title = "本轮结束时间（历史未记录单条消息时间）";
      clock.hidden = false;
    }
  }
}

function updateTraceBodyState(card, value, kind) {
  const node = card.querySelector(".trace-body");
  node._miraSource = value;
  node.hidden = value.length === 0;
  card.classList.toggle("trace-card-empty", value.length === 0);
  const copy = card.querySelector(".trace-copy");
  if (copy) copy.hidden = value.length === 0;
  if (kind === "reasoning" && card._miraExpandable) {
    card.querySelector(".trace-kind").textContent = reasoningHeading(value);
  }
}

function cancelTraceStreamRender(card) {
  const pending = traceStreamRenders.get(card);
  if (!pending) return;
  cancelAnimationFrame(pending.frame);
  traceStreamRenders.delete(card);
}

function setTraceBody(card, body, kind = card.dataset.traceKind) {
  const value = body ?? "";
  const node = card.querySelector(".trace-body");
  cancelTraceStreamRender(card);
  node._miraStreamText = null;
  updateTraceBodyState(card, value, kind);
  const markdown = traceUsesMarkdown(kind);
  node.classList.toggle("markdown-body", markdown);
  if (markdown) {
    node.innerHTML = DOMPurify.sanitize(marked.parse(value));
    decorateTraceFileReferences(node);
  }
  else node.textContent = value;
}

function queueTraceStreamRender(card, value, kind, follow, delta = null) {
  const node = card.querySelector(".trace-body");
  const queued = traceStreamRenders.get(card);
  // Capture the scroll intent before the first mutation in a frame. Subsequent
  // deltas append text without forcing a layout or waiting for another frame.
  const shouldFollow = queued ? queued.follow : follow && traceNearBottom();
  const scrollTop = queued ? queued.scrollTop : traceScroller().scrollTop;
  const previous = node._miraSource ?? "";
  node._miraSource = value;
  if (node._miraStreamText?.parentNode === node && delta !== null &&
      node._miraStreamText.length === previous.length) {
    node._miraStreamText.appendData(delta);
  } else if (node._miraStreamText?.parentNode === node && value.startsWith(node._miraStreamText.data)) {
    node._miraStreamText.appendData(value.slice(node._miraStreamText.length));
  } else {
    node._miraStreamText = document.createTextNode(value);
    node.replaceChildren(node._miraStreamText);
  }
  if (node.hidden || node.classList.contains("markdown-body")) {
    updateTraceBodyState(card, value, kind);
    node.classList.remove("markdown-body");
  }
  if (queued) {
    return;
  }
  const pending = { follow: shouldFollow, scrollTop, frame: null };
  pending.frame = requestAnimationFrame(() => {
    traceStreamRenders.delete(card);
    if (!card.isConnected) return;
    const trace = $("#conversationTrace");
    const source = node._miraSource ?? "";
    updateTraceBodyState(card, source, kind);
    // Parsing and sanitizing the entire accumulated Markdown for every token is
    // quadratic. Keep live output cheap and lossless, then fully typeset the
    // authoritative item once item/completed arrives.
    if (pending.follow && traceScroller().scrollTop === pending.scrollTop) scrollTraceToBottom(trace);
  });
  traceStreamRenders.set(card, pending);
}

function traceScroller() { return $("#conversationScroll"); }

function traceNearBottom(_trace, threshold = 96) {
  const scroll = traceScroller();
  return scroll.scrollHeight - scroll.clientHeight - scroll.scrollTop <= threshold;
}

function scrollTraceToBottom() {
  const scroll = traceScroller();
  scroll.scrollTop = scroll.scrollHeight;
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

function ensureToolGroup(trace, turnId = "", before = null) {
  let group = before ? before.previousElementSibling : trace.lastElementChild;
  if (group?.classList.contains("tool-group") && group.dataset.turnId === turnId) return group;
  group = element("details", "tool-group");
  group.dataset.turnId = turnId;
  const summary = element("summary", "tool-group-summary");
  summary.append(element("span", "tool-group-total"), element("span", "tool-group-latest"), element("span", "tool-group-counts"));
  group.append(summary, element("div", "tool-group-items"));
  trace.insertBefore(group, before);
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
    const copy = ["user", "compaction"].includes(kind) ? null : createTraceCopyButton(card);
    if (card._miraExpandable) {
      const head = element("summary", "trace-head");
      const actions = element("div", "trace-actions");
      actions.append(element("span", "trace-status", status), copy);
      head.append(element("span", "trace-kind", title), actions);
      const details = element("details", "trace-detail");
      details.append(head, element("div", "trace-body"));
      card.append(details);
    } else if (kind === "assistant") {
      const footer = element("footer", "trace-footer");
      footer.append(element("span", "trace-completed"), element("span", "trace-elapsed"), copy);
      card.append(element("div", "trace-body"), footer);
    } else if (["user", "compaction"].includes(kind)) {
      card.append(element("div", "trace-body"));
    } else {
      const head = element("div", "trace-head");
      const actions = element("div", "trace-actions");
      actions.append(element("span", "trace-status", status), copy);
      head.append(element("span", "trace-kind", title), actions);
      card.append(head, element("div", "trace-body"));
    }
    setTraceBody(card, body, kind);
    setTraceMetadata(card, options);
    if (kind === "tool" && options.collapseTools !== false) {
      ensureToolGroup(trace, options.turnId ?? "").querySelector(".tool-group-items").append(card);
    } else {
      trace.append(card);
    }
  } else {
    card.className = `trace-card ${kind}`;
    card.dataset.traceKind = kind;
    if (card.querySelector(".trace-kind")) card.querySelector(".trace-kind").textContent = title;
    if (card.querySelector(".trace-status")) card.querySelector(".trace-status").textContent = status;
    if (body !== undefined) setTraceBody(card, body, kind);
    setTraceMetadata(card, options);
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
  if (kind === "assistant" && !options.deferTurnFooter) refreshTurnFooters(options.turnId);
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

async function readNodeFile(nodeId, path, stat, controller) {
  const size = Number(stat.size ?? 0);
  if (!Number.isSafeInteger(size) || size < 0) throw new Error("Node 返回了无效的文件大小");
  const chunks = [];
  for (let offset = 0; offset < size;) {
    controller.signal.throwIfAborted();
    $("#nodeFileLoading").textContent = `正在从 Node 读取 ${formatBytes(offset)} / ${formatBytes(size)}…`;
    const result = await invokeNode(nodeId, "file", {
      action: "read", path, offset, length: Math.min(nodeFileChunkBytes, size - offset), encoding: "base64",
    }, 60_000);
    controller.signal.throwIfAborted();
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
  agent.fileReadController?.abort();
  agent.fileReadController = null;
  agent.filePreview = null;
  $("#nodeFileTextMore").classList.add("hidden");
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
  const controller = new AbortController();
  agent.fileReadController = controller;
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
      controller.signal.throwIfAborted();
      if (candidate.type !== "file") throw new Error("路径不是普通文件");
      selectedNode = nodeId;
      stat = candidate;
      break;
    } catch (error) { controller.signal.throwIfAborted(); lastError = error; }
  }
  if (!selectedNode) throw new Error(`无法从会话关联的节点读取此文件：${lastError?.message ?? "文件不存在"}`);
  const blob = await readNodeFile(selectedNode, path, stat, controller);
  controller.signal.throwIfAborted();
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
    agent.filePreview = { blob, offset: 0, decoder: new TextDecoder() };
    preview.classList.remove("hidden");
    await loadMoreFilePreview();
    if (line) requestAnimationFrame(() => {
      const lineHeight = Number.parseFloat(getComputedStyle(preview).lineHeight) || 20;
      preview.scrollTop = Math.max(0, (line - 3) * lineHeight);
    });
  } else {
    $("#nodeFileUnsupported").classList.remove("hidden");
  }
}

async function loadMoreFilePreview() {
  const value = agent.filePreview;
  if (!value || value.loading) return;
  value.loading = true;
  try {
    const end = Math.min(value.offset + 64 * 1024, value.blob.size);
    const bytes = await value.blob.slice(value.offset, end).arrayBuffer();
    if (agent.filePreview !== value) return;
    $("#nodeFileText").append(document.createTextNode(value.decoder.decode(bytes, { stream: end < value.blob.size })));
    value.offset = end;
    $("#nodeFileTextMore").classList.toggle("hidden", end >= value.blob.size);
    $("#nodeFileTextMore").textContent = `显示更多 · 已展示 ${formatBytes(end)} / ${formatBytes(value.blob.size)}`;
  } finally { value.loading = false; }
}

function appendTraceText(key, kind, title, delta, status = "运行中") {
  if (!delta) return;
  const trace = $("#conversationTrace");
  const existing = trace.querySelector(`[data-trace-key="${CSS.escape(key)}"]`);
  const effectiveTitle = existing?.dataset.traceTitle || title;
  const card = existing ?? upsertTrace(key, kind, effectiveTitle, undefined, status, { autoScroll: false, turnId: agent.turnId });
  const body = card.querySelector(".trace-body");
  queueTraceStreamRender(card, `${body._miraSource ?? body.textContent ?? ""}${delta}`, kind, true, delta);
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
  if (type === "contextCompaction") return { kind: "compaction", title: "上下文自动压缩",
    body: item.status === "inProgress" ? "正在自动压缩较早的上下文…" : "较早的上下文已自动压缩。" };
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
  const trace = $("#conversationTrace");
  const follow = traceNearBottom(trace);
  const card = upsertTrace(key, "reasoning", "推理摘要", undefined, "", { autoScroll: false, turnId: params.turnId });
  card._miraSummaryParts = parts;
  queueTraceStreamRender(card, body, "reasoning", follow);
}

function renderThread(thread) {
  const trace = clear($("#conversationTrace"));
  const turns = Array.isArray(thread?.turns) ? thread.turns : [];
  for (const turn of turns) {
    for (const item of turn.items ?? []) {
      const view = itemView(item);
      if (["assistant", "reasoning"].includes(view.kind) && !view.body) continue;
      upsertTrace(liveTraceKey({ turnId: turn.id }, item.id ?? crypto.randomUUID?.() ?? Math.random()), view.kind, view.title, view.body, item.status ?? "", {
        autoScroll: false, deferTurnFooter: true, activity: view.activity, summaryParts: view.summaryParts, turnId: turn.id,
        completedAt: item.completedAt ?? item.updatedAt ?? item.createdAt,
        ...(["completed", "failed", "interrupted"].includes(turn.status) ? {
          turnCompletedAt: turn.completedAt, turnElapsedMs: turn.durationMs ?? turn.elapsedMs,
        } : {}),
      });
    }
  }
  if (!turns.length) trace.append(element("div", "conversation-empty", "此会话还没有可显示的消息。"));
  refreshTurnFooters();
  const last = trace.lastElementChild;
  requestAnimationFrame(() => { if (trace.lastElementChild === last) scrollTraceToBottom(trace); });
}

function resetAgentTranscript(threadId = null) {
  agent.transcriptThreadId = threadId;
  agent.transcriptGeneration = null;
  agent.transcriptItems = [];
  agent.transcriptCursor = null;
  agent.transcriptTotal = 0;
  agent.transcriptLoadingOlder = false;
  agent.transcriptOlderError = null;
  agent.transcriptTailVersion = null;
}

function retainTurnError(threadId, turnId, message) {
  if (!threadId || !message) return;
  const key = JSON.stringify([threadId, turnId ?? "unscoped"]);
  let diagnostic = agent.diagnostics.get(key);
  if (!diagnostic) {
    diagnostic = { threadId, turnId, messages: new Set(), dismissed: false };
    agent.diagnostics.set(key, diagnostic);
  }
  diagnostic.messages.add(message);
  if (threadId === agent.threadId) renderTurnDiagnostics();
}

function renderTurnDiagnostics() {
  for (const [key, diagnostic] of agent.diagnostics) {
    if (diagnostic.threadId !== agent.threadId || diagnostic.dismissed) continue;
    const card = upsertTrace(`diagnostic-${key}`, "error", "执行失败",
      [...diagnostic.messages].join("\n\n"), "", { autoScroll: false, turnId: diagnostic.turnId });
    card.dataset.diagnosticKey = key;
    if (!card.querySelector("[data-dismiss-diagnostic]")) {
      const dismiss = element("button", "trace-dismiss", "关闭此错误");
      dismiss.type = "button";
      dismiss.dataset.dismissDiagnostic = key;
      dismiss.title = "仅关闭提示，不会重新执行或删除数据库历史";
      card.append(dismiss);
    }
  }
}

function mergeTranscriptItems(current, updates) {
  const merged = new Map();
  for (const item of [...current, ...updates]) {
    const previous = merged.get(item.key);
    if (previous?.toolFragment && item.toolFragment) {
      const materialized = item.toolFragment.materialized ? item : previous.toolFragment.materialized ? previous : null;
      const fragments = {
        input: item.toolFragment.input ?? previous.toolFragment.input,
        output: item.toolFragment.output ?? previous.toolFragment.output,
      };
      merged.set(item.key, {
        ...previous, ...item, ...(materialized ?? {}),
        sourceItemSeq: Math.min(previous.sourceItemSeq, item.sourceItemSeq),
        ...(!materialized ? {
          title: fragments.input != null ? (item.toolFragment.input != null ? item.title : previous.title) : item.title,
          activity: item.activity ?? previous.activity,
          status: fragments.output != null ? "完成" : item.status,
          toolFragment: fragments,
          body: [fragments.input ? `输入\n${fragments.input}` : "", fragments.output ? `输出\n${fragments.output}` : ""].filter(Boolean).join("\n\n"),
        } : {}),
      });
    } else merged.set(item.key, item);
  }
  const narratives = new Map();
  return [...merged.values()].sort((left, right) =>
    (left.sourceItemSeq ?? Number.MAX_SAFE_INTEGER) - (right.sourceItemSeq ?? Number.MAX_SAFE_INTEGER))
    .filter((item) => {
      if (!["user", "assistant", "reasoning"].includes(item.kind)) return true;
      const signature = JSON.stringify([item.turnId, item.kind, item.phase, item.body]);
      const previous = narratives.get(signature);
      if (previous && (item.turnId || item.sourceItemSeq - previous.sourceItemSeq <= 3)) return false;
      narratives.set(signature, item);
      return true;
    });
}

function renderHistoryLoader(trace) {
  if (agent.transcriptCursor === null) return;
  const loader = element("div", "history-loader");
  const status = element("span", "history-load-status");
  status.setAttribute("role", "status");
  const button = element("button", "history-load-button", "加载失败，点击重试");
  button.type = "button";
  button.dataset.loadOlder = "true";
  loader.append(status, button);
  trace.append(loader);
  updateHistoryLoader();
}

function updateHistoryLoader() {
  const loader = $("#conversationTrace .history-loader");
  if (!loader) return;
  const failed = Boolean(agent.transcriptOlderError);
  const status = loader.querySelector(".history-load-status");
  status.hidden = failed;
  status.textContent = agent.transcriptLoadingOlder ? "正在加载更早消息…" : "向上滚动加载更早消息";
  const button = loader.querySelector("[data-load-older]");
  button.hidden = !failed;
  button.disabled = agent.transcriptLoadingOlder;
  button.title = agent.transcriptOlderError ?? "";
}

let olderTranscriptFrame = null;
function scheduleOlderTranscriptLoad() {
  if (olderTranscriptFrame !== null) return;
  olderTranscriptFrame = requestAnimationFrame(() => {
    olderTranscriptFrame = null;
    if ($("#agentView").classList.contains("hidden") || !traceScroller().clientHeight ||
        traceScroller().scrollTop > 64 || agent.transcriptOlderError ||
        agent.transcriptThreadId !== agent.threadId) return;
    void loadOlderAgentTranscript();
  });
}

function renderTranscript(fallbackThread, options = {}) {
  const existingTrace = $("#conversationTrace");
  const previousCards = [...existingTrace.querySelectorAll(".trace-card")];
  const liveCards = options.preserveLive || options.preserveViewport?.mode === "prepend"
    ? [...existingTrace.querySelectorAll('.trace-card[data-trace-key^="item-"]:not(.compaction), .trace-card[data-pending-user="true"]')]
    : [];
  const liveCompactions = [...existingTrace.querySelectorAll('.trace-card.compaction')]
    .filter((card) => card.dataset.traceKey?.startsWith("item-"));
  const preciseClocks = new Map([...existingTrace.querySelectorAll('.trace-card.assistant')]
    .filter((card) => card._miraCompletedAt && !card._miraTimingScope)
    .map((card) => [JSON.stringify([card.dataset.turnId ?? null, card.querySelector('.trace-body')._miraSource]),
      { completedAt: card._miraCompletedAt }]));
  const knownTurnTimings = new Map([...existingTrace.querySelectorAll('.trace-card.assistant')]
    .filter((card) => card.dataset.turnId && Number.isFinite(card._miraTurnElapsedMs))
    .map((card) => [card.dataset.turnId, { turnCompletedAt: card._miraTurnCompletedAt,
      turnElapsedMs: card._miraTurnElapsedMs, turnElapsedApproximate: card._miraTurnElapsedApproximate }]));
  const expandedItems = new Set([...$("#conversationTrace").querySelectorAll(".trace-detail[open]")]
    .map((details) => details.closest(".trace-card").dataset.traceKey));
  const expandedGroups = new Set([...$("#conversationTrace").querySelectorAll(".tool-group[open] .trace-card")]
    .map((card) => card.dataset.traceKey));
  const trace = clear($("#conversationTrace"));
  renderHistoryLoader(trace);
  for (const item of agent.transcriptItems) {
    if (item.kind === "error" && agent.diagnostics.has(JSON.stringify([agent.threadId, item.turnId ?? "unscoped"]))) continue;
    const key = item.itemId ? liveTraceKey({ turnId: item.turnId }, item.itemId) : item.key;
    const knownClock = (!item.completedAt || item.timingScope) && preciseClocks.get(JSON.stringify([item.turnId ?? null, item.body]));
    const card = upsertTrace(key, item.kind ?? "tool", item.title ?? "事件", item.body ?? "", item.status ?? "", {
      autoScroll: false, deferTurnFooter: true, activity: item.activity, summaryParts: item.summaryParts, turnId: item.turnId,
      completedAt: item.completedAt, elapsedMs: item.elapsedMs, timingScope: item.timingScope,
      elapsedApproximate: item.elapsedApproximate,
      ...(Number.isFinite(item.turnElapsedMs) ? {
        turnCompletedAt: item.turnCompletedAt, turnElapsedMs: item.turnElapsedMs, turnElapsedApproximate: item.turnElapsedApproximate,
      } : knownTurnTimings.get(item.turnId)),
      ...(knownClock ? { ...knownClock, timingScope: undefined, elapsedApproximate: undefined } : {}),
    });
    if (expandedItems.has(key) && card.querySelector(".trace-detail")) card.querySelector(".trace-detail").open = true;
    if (expandedGroups.has(key) && card.closest(".tool-group")) card.closest(".tool-group").open = true;
  }
  // Older pages must not replace prose or tool output still arriving at the tail.
  const renderedCards = liveCards.length ? [...trace.querySelectorAll(".trace-card")] : [];
  const narrativeKey = (card) => JSON.stringify([card.dataset.turnId, card.dataset.traceKind, card.querySelector(".trace-body")._miraSource]);
  const renderedByKey = new Map(renderedCards.map((card) => [card.dataset.traceKey, card]));
  const renderedByBody = new Map(renderedCards.map((card) => [narrativeKey(card), card]));
  const preservedCards = new Set(liveCards);
  let nextLiveAnchor = null;
  for (const card of previousCards.reverse()) {
    const replacement = renderedByKey.get(card.dataset.traceKey) ?? renderedByBody.get(narrativeKey(card));
    if (replacement) {
      // Canonical completion supersedes an older partial live item. Prepending
      // older pages must instead keep the still-running tail untouched.
      if (preservedCards.has(card) && options.preserveViewport?.mode === "prepend") { replacement.replaceWith(card); nextLiveAnchor = card; }
      else nextLiveAnchor = replacement;
    }
    else if (preservedCards.has(card)) {
      // Nested tool notifications may have no rollout counterpart. Preserve
      // their position before the next known live item (often the final reply),
      // instead of appending them after the completed turn during reconciliation.
      const anchorGroup = nextLiveAnchor?.closest(".tool-group");
      const before = anchorGroup ?? nextLiveAnchor;
      if (card.dataset.traceKind === "tool") {
        const sameGroup = anchorGroup?.dataset.turnId === (card.dataset.turnId ?? "");
        const group = sameGroup ? anchorGroup : ensureToolGroup(trace, card.dataset.turnId ?? "", before);
        group.querySelector(".tool-group-items").insertBefore(card, sameGroup ? nextLiveAnchor : null);
      } else trace.insertBefore(card, before);
      nextLiveAnchor = card;
    }
  }
  for (const group of trace.querySelectorAll(".tool-group")) updateToolGroup(group);
  const storedCompactions = new Map();
  for (const item of agent.transcriptItems.filter((item) => item.kind === "compaction")) {
    const turn = item.turnId ?? "";
    storedCompactions.set(turn, (storedCompactions.get(turn) ?? 0) + 1);
  }
  for (const card of liveCompactions) {
    const turn = card.dataset.turnId ?? "";
    const remaining = storedCompactions.get(turn) ?? 0;
    if (remaining) storedCompactions.set(turn, remaining - 1);
    else trace.append(card); // Keep the notice while its durable write catches up.
  }
  renderTurnDiagnostics();
  if (!agent.transcriptItems.length && !liveCompactions.length && !liveCards.length && !trace.querySelector("[data-diagnostic-key]")) {
    if (agent.transcriptCursor !== null) {
      trace.append(element("div", "conversation-empty", "此页没有可显示的消息，可继续加载更早历史。"));
      scheduleOlderTranscriptLoad();
      return;
    }
    renderThread(fallbackThread);
    if (!fallbackThread?.turns?.some((turn) => (turn.items ?? []).length > 0)) {
      clear(trace).append(element("div", "conversation-empty", "数据库中没有可投影的消息或工具记录。"));
    }
    return;
  }
  refreshTurnFooters();
  const last = trace.lastElementChild;
  const scroll = traceScroller();
  const scrollTop = scroll.scrollTop;
  requestAnimationFrame(() => {
    if (trace.lastElementChild !== last || scroll.scrollTop !== scrollTop) return;
    if (options.preserveViewport) {
      const viewport = options.preserveViewport;
      const anchor = viewport.anchorKey && trace.querySelector(`[data-trace-key="${CSS.escape(viewport.anchorKey)}"]`);
      scroll.scrollTop = viewport.mode === "prepend"
        ? anchor ? scroll.scrollTop + anchor.getBoundingClientRect().top - viewport.anchorTop
          : viewport.top + scroll.scrollHeight - viewport.height
        : viewport.top;
    } else if (options.anchorBottom !== false) {
      scrollTraceToBottom(trace);
      requestAnimationFrame(() => {
        if (trace.lastElementChild === last && traceNearBottom(trace)) scrollTraceToBottom(trace);
      });
    }
    scheduleOlderTranscriptLoad();
  });
}

async function loadAgentTranscript(threadId, fallbackThread = null, options = {}) {
  const epoch = agent.selectionEpoch;
  const request = ++agent.transcriptRequest;
  const liveRevision = agent.liveRevision;
  const query = new URLSearchParams({ storeId: "personal", tail: "1", limit: String(options.limit ?? transcriptPageSize) });
  if (options.cursor !== undefined && options.cursor !== null) query.set("cursor", String(options.cursor));
  const transcript = await api(`/v1/codex/threads/${encodeURIComponent(threadId)}/transcript?${query}`);
  if (agent.threadId !== threadId || agent.selectionEpoch !== epoch || request !== agent.transcriptRequest) return transcript;

  const incoming = Array.isArray(transcript.trace) ? transcript.trace : [];
  const sameThread = agent.transcriptThreadId === threadId &&
    agent.transcriptGeneration === transcript.generation;
  const tailVersion = transcript.storeVersion == null ? null
    : JSON.stringify([transcript.generation, transcript.storeVersion, transcript.itemCount]);
  if (sameThread && options.preserveLoaded && !options.prepend && tailVersion !== null &&
      agent.transcriptTailVersion === tailVersion) {
    renderTurnDiagnostics();
    return transcript; // No changed canonical items: avoid rebuilding Markdown/DOM.
  }
  const trace = $("#conversationTrace");
  const scroll = traceScroller();
  const preserveViewport = options.prepend
    ? { mode: "prepend", top: scroll.scrollTop, height: scroll.scrollHeight }
    : options.preserveLoaded && options.anchorBottom === false
      ? { mode: "stable", top: scroll.scrollTop, height: scroll.scrollHeight }
      : null;
  if (options.prepend) {
    const top = $(".conversation-head").getBoundingClientRect().bottom;
    const anchor = [...trace.querySelectorAll(".trace-card[data-trace-key]")].find((card) => card.getBoundingClientRect().bottom > top);
    if (anchor) Object.assign(preserveViewport, { anchorKey: anchor.dataset.traceKey, anchorTop: anchor.getBoundingClientRect().top });
  }
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
    agent.transcriptItems = mergeTranscriptItems([], incoming);
    agent.transcriptCursor = transcript.nextCursor ?? null;
  }
  agent.transcriptTotal = transcript.totalTraceItems ?? agent.transcriptItems.length;
  // Do not replace live output that arrived after this HTTP snapshot began.
  if (agent.liveRevision !== liveRevision && agent.transcriptThreadId === threadId && options.preserveLoaded) return transcript;
  renderTranscript(fallbackThread, {
    preserveViewport,
    preserveLive: options.preserveLoaded && sameThread,
    anchorBottom: options.anchorBottom !== false,
  });
  if (!options.prepend) agent.transcriptTailVersion = tailVersion;
  return transcript;
}

async function loadOlderAgentTranscript() {
  if (!agent.threadId || agent.transcriptCursor === null || agent.transcriptLoadingOlder) return;
  const threadId = agent.threadId;
  const epoch = agent.selectionEpoch;
  agent.transcriptLoadingOlder = true;
  agent.transcriptOlderError = null;
  updateHistoryLoader();
  try {
    await loadAgentTranscript(threadId, null, {
      cursor: agent.transcriptCursor,
      prepend: true,
      anchorBottom: false,
    });
  } catch (error) {
    if (threadId === agent.threadId && epoch === agent.selectionEpoch) agent.transcriptOlderError = error.message;
  } finally {
    if (threadId === agent.threadId && epoch === agent.selectionEpoch) {
      agent.transcriptLoadingOlder = false;
      updateHistoryLoader();
      // Continue past empty pages or fill a short viewport, after its anchor is restored.
      scheduleOlderTranscriptLoad();
    }
  }
}

async function refreshCompletedTranscript(threadId) {
  const epoch = agent.selectionEpoch;
  const key = JSON.stringify([threadId, epoch]);
  if (agent.transcriptReconciliations.has(key)) return agent.transcriptReconciliations.get(key);
  // One coalesced read per completion burst, not a fixed three-read polling loop.
  // Unpersisted live items/diagnostics remain visible; reconnect also reconciles.
  const operation = (async () => {
    await new Promise((resolve) => setTimeout(resolve, 250));
    if (agent.threadId !== threadId || agent.selectionEpoch !== epoch) return;
    try {
      await loadAgentTranscript(threadId, null, {
        preserveLoaded: true,
        anchorBottom: traceNearBottom(),
      });
    } catch (error) {
      if (agent.threadId === threadId && agent.selectionEpoch === epoch) {
        setConversationNotice(`历史对账暂未完成，实时内容仍保留：${error.message}`, "warning");
      }
    }
  })();
  agent.transcriptReconciliations.set(key, operation);
  try { await operation; }
  finally { agent.transcriptReconciliations.delete(key); }
}

function handleAgentNotification(message) {
  const method = message.method ?? "";
  const params = message.params ?? {};
  if (notificationIsForOpenThread(params) && (/^(item|turn)\//.test(method) || method === "thread/status/changed")) agent.liveRevision++;
  if (method === "thread/closed") agent.loadedThreadIds.delete(params.threadId);
  if (message.id !== undefined) {
    if (notificationIsForOpenThread(params)) {
      upsertTrace(`request-${message.id}`, "system", method,
        "当前网页客户端未启用交互审批；本界面发起的 Turn 使用 never。", "等待处理");
    }
    agent.socket?.send(JSON.stringify({ id: message.id, error: { code: -32601, message: "interactive request is not supported by Mira Web" } }));
    return;
  }
  // Live output can arrive after reconnect without replaying turn/started.
  // It proves that this turn is active; finishing an item never finishes a turn.
  const liveTurnId = params.turnId;
  const liveThreadId = notificationThreadId(params);
  if (method.startsWith("item/") && liveThreadId && liveTurnId &&
      !agent.activeTurns.get(liveThreadId) && !agent.turnTimings.get(liveTurnId)?.completedAt) {
    agent.activeTurns.set(liveThreadId, liveTurnId);
    agent.turnThreads.set(liveTurnId, liveThreadId);
    syncActiveTurnUi();
  }
  if (method === "thread/status/changed") {
    const threadId = params.threadId;
    if (threadId && params.status?.type === "active" && !agent.activeTurns.has(threadId)) agent.activeTurns.set(threadId, null);
    syncActiveTurnUi();
    if (threadId === agent.threadId && agent.activeTurns.has(threadId) && agent.loadedThreadIds.has(threadId)) {
      void refreshActiveTurn(threadId, agent.socket);
    }
    return;
  }
  replyProgress.observe(method, { ...params, threadId: notificationThreadId(params) });
  observeTurnActivity(method, params);
  renderReplyProgress();
  if (method === "turn/started") {
    const turnId = params.turn?.id ?? null;
    const threadId = params.threadId ?? agent.threadId;
    if (threadId && turnId) {
      agent.activeTurns.set(threadId, turnId);
      agent.turnThreads.set(turnId, threadId);
      const timing = agent.turnTimings.get(turnId) ?? {};
      timing.startedAt ??= replyProgress.current(threadId)?.startedAt ?? Date.now();
      agent.turnTimings.set(turnId, timing);
    }
    syncActiveTurnUi();
    return;
  }
  if (method === "turn/completed") {
    const turn = params.turn ?? {};
    const completedTurnId = turn.id ?? params.turnId ?? null;
    const threadId = notificationThreadId(params) ?? agent.threadId;
    const completedAt = Date.now();
    const timing = completedTurnId ? (agent.turnTimings.get(completedTurnId) ?? {}) : {};
    timing.completedAt = completedAt;
    timing.elapsedMs = turn.durationMs ?? turn.elapsedMs ?? (Number.isFinite(timing.startedAt) ? Math.max(0, completedAt - timing.startedAt) : null);
    if (completedTurnId) agent.turnTimings.set(completedTurnId, timing);
    if (threadId && (!completedTurnId || agent.activeTurns.get(threadId) === completedTurnId)) {
      agent.activeTurns.delete(threadId);
    }
    if (completedTurnId) agent.turnThreads.delete(completedTurnId);
    if (completedTurnId && threadId === agent.threadId) {
      refreshTurnFooters(completedTurnId);
    }
    syncActiveTurnUi();
    void loadAgentThreads().catch((error) => console.warn("Unable to refresh thread list after turn completion", error));
    const status = String(turn.status ?? "").toLowerCase();
    const turnError = readableErrorMessage(turn.error);
    if (turnError || ["failed", "error"].includes(status)) {
      retainTurnError(threadId, completedTurnId, turnError || "Codex Turn 执行失败");
    }
    if (threadId && threadId !== agent.threadId) return;
    if (["interrupted", "cancelled", "canceled", "aborted"].includes(status)) {
      upsertTrace(`turn-${completedTurnId ?? Date.now()}`, "system", "Turn 已中断",
        "本次执行没有继续完成。", turn.status);
    }
    if (threadId) void refreshCompletedTranscript(threadId);
    return;
  }
  if (method === "error") {
    const threadId = notificationThreadId(params) ?? agent.threadId;
    if (params.willRetry === true) {
      if (threadId === agent.threadId) upsertTrace(`retry-${params.turnId ?? "current"}`, "system", "正在重试",
        readableErrorMessage(params.error ?? params.message) || "连接暂时中断，Codex 正在重试。", "");
      return; // A recoverable transport warning is not a failed turn.
    }
    retainTurnError(threadId, params.turnId ?? agent.activeTurns.get(threadId),
      readableErrorMessage(params.error ?? params.message ?? params) || JSON.stringify(params));
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
    const completedAt = method.endsWith("completed") ? item.completedAt ?? new Date().toISOString() : undefined;
    const completedStreamBody = method.endsWith("completed") && emptyNarrative
      ? existing?.querySelector(".trace-body")?._miraSource
      : undefined;
    upsertTrace(itemKey, view.kind, view.title, emptyNarrative ? completedStreamBody : view.body,
      method.endsWith("started") ? "运行中" : (item.status ?? "完成"), {
        activity: view.activity, summaryParts: view.summaryParts, turnId: params.turnId,
        ...(completedAt ? { completedAt } : {}),
      });
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
    upsertTrace(null, method === "error" ? "error" : "system", method === "error" ? "错误" : "警告",
      readableErrorMessage(params.error ?? params.message ?? params) || JSON.stringify(params), "");
  }
}

function onAgentSocketMessage(event) {
  agent.lastSocketMessageAt = Date.now();
  let message;
  try { message = JSON.parse(event.data); } catch { return; }
  if (message.id !== undefined && (Object.hasOwn(message, "result") || Object.hasOwn(message, "error"))) {
    const pending = agent.pending.get(message.id);
    if (!pending) return;
    agent.pending.delete(message.id);
    if (message.error) pending.reject(new Error(readableErrorMessage(message.error) || JSON.stringify(message.error)));
    else pending.resolve(message.result);
    return;
  }
  handleAgentNotification(message);
}

async function connectAgentSocket(nodeId) {
  if (agent.socketInitialized && agent.socket?.readyState === WebSocket.OPEN && agent.socketNodeId === nodeId) return;
  closeAgentSocket({ preserveSubmission: true });
  clearTimeout(agent.reconnectTimer);
  setAgentRuntimeState("正在建立 App Server 通道…", "offline");
  const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
  const socket = new WebSocket(`${scheme}//${window.location.host}/v1/nodes/${nodeId}/app-server?storeId=personal`, ["mira-client-v1"]);
  agent.socket = socket;
  agent.socketNodeId = nodeId;
  socket.addEventListener("message", (event) => { if (agent.socket === socket) onAgentSocketMessage(event); });
  socket.addEventListener("close", () => {
    if (agent.socket !== socket) return;
    closeAgentSocket();
    setAgentRuntimeState("连接暂时中断，正在自动重连…", "offline");
    scheduleAgentRecovery();
  });
  try {
    await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error("App Server WebSocket 连接超时")), 15_000);
      socket.addEventListener("open", () => { clearTimeout(timer); resolve(); }, { once: true });
      for (const type of ["error", "close"]) socket.addEventListener(type, () => {
        clearTimeout(timer); reject(new Error("App Server WebSocket 连接失败"));
      }, { once: true });
    });
    if (agent.socket !== socket) throw new Error("App Server 连接已取消");
    await rpc("initialize", {
      clientInfo: { name: "mira_web", title: "Mira Web", version: "1" },
      capabilities: { experimentalApi: true },
    }, 15_000);
    if (agent.socket !== socket) throw new Error("App Server 连接已取消");
    socket.send(JSON.stringify({ method: "initialized" }));
    agent.socketInitialized = true;
  } catch (error) {
    if (agent.socket === socket) closeAgentSocket();
    throw error;
  }
  syncConversationSendUi();
  const node = dashboardNodes.get(nodeId);
  setAgentRuntimeState(`已连接 ${node?.hostname ?? nodeId}`, "online");
  scheduleAgentHeartbeat();
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
  renderAgentThreads();
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

function projectForThread(thread) {
  const nodeId = thread?.runtimeNodeId || thread?.sourceNodeId || "";
  const cwd = thread?.cwd || "";
  const node = dashboardNodes.get(nodeId);
  const windows = node?.platform === "windows" || /^[a-z]:[\\/]|^\\\\/i.test(cwd);
  let path = windows ? cwd.replaceAll("\\", "/").toLowerCase() : cwd;
  path = path.replace(/\/+$/, "") || "/";
  return { key: JSON.stringify([nodeId, cwd ? path : ""]), nodeId, cwd };
}

function renderAgentThreads() {
  const list = clear($("#agentThreadList"));
  const groups = new Map();
  const threads = agent.threads.filter(thread => Boolean(thread.archived) === agent.showArchived).sort((a, b) =>
    (Date.parse(b.updatedAt) || 0) - (Date.parse(a.updatedAt) || 0) || b.threadId.localeCompare(a.threadId));
  for (const thread of threads) {
    const project = projectForThread(thread);
    if (!groups.has(project.key)) groups.set(project.key, { ...project, threads: [] });
    groups.get(project.key).threads.push(thread);
  }
  if (agent.draftProject && !groups.has(agent.draftProject.key)) groups.set(agent.draftProject.key, { ...agent.draftProject, threads: [] });
  if (!groups.size) list.append(element("div", "agent-list-empty", agent.showArchived ? "没有已归档的对话" : "添加项目目录，开始新的对话"));
  for (const group of groups.values()) {
    const project = element("details", "thread-project");
    project.dataset.projectKey = group.key;
    project.open = agent.projectOpen.get(group.key) ?? true;
    project.addEventListener("toggle", () => agent.projectOpen.set(group.key, project.open));
    const summary = element("summary", "thread-project-summary");
    const copy = element("span", "thread-project-identity");
    const name = group.cwd.replace(/[\\/]+$/, "").split(/[\\/]/).at(-1) || group.cwd || "未分配目录";
    const node = dashboardNodes.get(group.nodeId);
    const location = `${node?.hostname || (group.nodeId ? "未连接的机器" : "未关联运行机器")} · ${group.cwd || "目录未知"}`;
    copy.append(element("strong", "", name), element("small", "", location));
    copy.title = location;
    const add = element("button", "chat-icon-button project-new-thread", "+");
    add.type = "button";
    add.dataset.projectNew = group.key;
    add.dataset.projectNode = node?.capabilities?.appServer === true ? group.nodeId : "";
    add.dataset.projectPath = group.cwd;
    add.disabled = Boolean(agent.sendPromise || agent.forkPromise || agent.threadActionPromise) || !add.dataset.projectNode;
    add.title = add.dataset.projectNode ? `在 ${group.cwd || "默认目录"} 新建对话` : "该项目未关联可运行 Codex 的机器";
    add.setAttribute("aria-label", add.title);
    summary.append(copy, add);
    project.append(summary);
    for (const thread of group.threads) {
      const row = element("div", "agent-thread-row");
      row.dataset.threadRow = thread.threadId;
      const button = element("button", `agent-thread${thread.threadId === agent.threadId ? " active" : ""}`);
      button.type = "button";
      button.disabled = Boolean(agent.sendPromise || agent.forkPromise || agent.threadActionPromise);
      button.dataset.threadId = thread.threadId;
      button.title = thread.title || "未命名会话";
      button.append(element("strong", "", button.title), element("span", "", `${thread.parentThreadId ? "子对话 · " : ""}${when(thread.updatedAt)}`));
      const openWindow = element("a", "chat-icon-button thread-open-window", "↗");
      openWindow.href = `/?thread=${encodeURIComponent(thread.threadId)}`;
      openWindow.target = "_blank";
      openWindow.rel = "noopener";
      openWindow.dataset.openThreadWindow = thread.threadId;
      openWindow.title = `在新窗口打开：${button.title}`;
      openWindow.setAttribute("aria-label", openWindow.title);
      const menu = element("button", "chat-icon-button thread-menu-toggle", "⋯");
      menu.type = "button";
      menu.dataset.threadMenu = thread.threadId;
      menu.title = `对话选项：${button.title}`;
      menu.setAttribute("aria-label", menu.title);
      menu.setAttribute("aria-haspopup", "menu");
      row.append(button, openWindow, menu);
      project.append(row);
    }
    list.append(project);
  }
}

function openThreadWindow(event, link) {
  if (event.ctrlKey || event.metaKey || event.shiftKey || event.altKey || event.button !== 0) return;
  event.preventDefault();
  window.open(link.href, "_blank", `width=${Math.min(1100, screen.availWidth)},height=${Math.min(900, screen.availHeight)},noopener`);
}

function openThreadMenu(threadId, anchor, point = null) {
  if (!threadId) return;
  agent.menuThreadId = threadId;
  const menu = $("#threadOptionsMenu");
  $("#threadOpenWindow").href = `/?thread=${encodeURIComponent(threadId)}`;
  $("#threadArchive").textContent = agent.threads.find(thread => thread.threadId === threadId)?.archived ? "恢复对话" : "归档对话";
  menu.showPopover();
  const bounds = anchor.getBoundingClientRect();
  menu.style.left = `${Math.max(8, Math.min(point?.x ?? bounds.right - menu.offsetWidth, innerWidth - menu.offsetWidth - 8))}px`;
  menu.style.top = `${Math.max(8, Math.min(point?.y ?? bounds.bottom + 4, innerHeight - menu.offsetHeight - 8))}px`;
  $("#threadRename").focus();
}

async function editThreadTitle() {
  const threadId = agent.menuThreadId;
  $("#threadOptionsMenu").hidePopover();
  // Fetch the latest name for compare-and-swap, including edits from another window.
  const thread = await api(`/v1/codex/threads/${encodeURIComponent(threadId)}?storeId=personal`);
  agent.rename = { threadId, expectedName: thread.name ?? null, generation: thread.generation };
  $("#threadRenameInput").value = thread.title || "";
  $("#threadRenameError").textContent = "";
  $("#threadRenameDialog").showModal();
  $("#threadRenameInput").select();
}

async function forkWithNode(node, params) {
  if (node.status !== "online") throw new Error("运行机器离线，连接后才能创建分支。");
  if (node.reportedAppServer?.status !== "running") {
    await api(`/v1/codex/runtimes/${node.nodeId}/start`, { method: "POST", body: JSON.stringify({ storeId: "personal" }) });
    const deadline = Date.now() + 120_000;
    while (node.reportedAppServer?.status !== "running") {
      if (Date.now() >= deadline) throw new Error("等待运行机器启动超时，请稍后重试。");
      await new Promise((resolve) => setTimeout(resolve, 500));
      node = await api(`/v1/nodes/${node.nodeId}`);
      if (node.status !== "online") throw new Error("运行机器已离线，请连接后重试。");
    }
  }
  // A separate connection keeps the current conversation and its live stream intact.
  const scheme = location.protocol === "https:" ? "wss:" : "ws:";
  const socket = new WebSocket(`${scheme}//${location.host}/v1/nodes/${node.nodeId}/app-server?storeId=personal`, ["mira-client-v1"]);
  const pending = new Map();
  let next = 0;
  socket.addEventListener("message", (event) => {
    let message; try { message = JSON.parse(event.data); } catch { return; }
    if (!Object.hasOwn(message, "result") && !Object.hasOwn(message, "error")) return;
    const request = pending.get(message.id);
    if (!request) return;
    message.error ? request.reject(new Error(readableErrorMessage(message.error) || "创建分支失败")) : request.resolve(message.result);
  });
  socket.addEventListener("close", () => { for (const request of pending.values()) request.reject(new Error("分支连接已断开，请重试。")); });
  const call = (method, values) => new Promise((resolve, reject) => {
    const id = ++next;
    const timer = setTimeout(() => finish(reject, new Error("创建分支超时，请重试。")), 120_000);
    const finish = (callback, result) => { clearTimeout(timer); pending.delete(id); callback(result); };
    pending.set(id, { resolve: (result) => finish(resolve, result), reject: (error) => finish(reject, error) });
    socket.send(JSON.stringify({ id, method, params: values }));
  });
  try {
    await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error("连接运行机器超时。")), 15_000);
      socket.addEventListener("open", () => { clearTimeout(timer); resolve(); }, { once: true });
      for (const event of ["close", "error"]) socket.addEventListener(event, () => { clearTimeout(timer); reject(new Error("无法连接运行机器。")); }, { once: true });
    });
    await call("initialize", { clientInfo: { name: "mira_web_fork", version: "1" }, capabilities: { experimentalApi: true } });
    socket.send(JSON.stringify({ method: "initialized" }));
    return await call("thread/fork", params);
  } finally { socket.close(); }
}

async function forkThreadFromMenu() {
  if (agent.sendPromise || agent.forkPromise || agent.threadActionPromise) return;
  const sourceId = agent.menuThreadId;
  const epoch = agent.selectionEpoch;
  $("#threadOptionsMenu").hidePopover();
  setConversationNotice("正在创建对话分支…");
  const operation = (async () => {
    const source = await api(`/v1/codex/threads/${encodeURIComponent(sourceId)}?storeId=personal`);
    const nodeId = source.runtimeNodeId || source.sourceNodeId;
    if (!nodeId) throw new Error("请先为该对话选择运行机器，再创建分支。");
    const node = await api(`/v1/nodes/${nodeId}`);
    if (node.capabilities?.appServer !== true) throw new Error("该机器不能运行 Codex，请先选择兼容的运行机器。");
    let request = agent.forkRequests.get(sourceId);
    if (!request) {
      request = { threadId: sourceId, excludeTurns: true, deferGoalContinuation: true, miraRequestId: crypto.randomUUID(),
        approvalPolicy: "never", sandbox: "danger-full-access", ...(source.cwd ? { cwd: source.cwd } : {}) };
      agent.forkRequests.set(sourceId, request);
    }
    const result = await forkWithNode(node, request);
    agent.forkRequests.delete(sourceId);
    await loadAgentThreads();
    const created = agent.threads.find((thread) => thread.threadId === result.thread.id);
    if (created && !created.runtimeNodeId) created.runtimeNodeId = nodeId;
    if (agent.selectionEpoch === epoch && document.body.dataset.view === "agentView") await resumeAgentThread(result.thread.id);
    else toast("分支已创建，可在项目下打开。");
  })();
  agent.forkPromise = operation;
  syncConversationSendUi();
  try { await operation; }
  catch (error) { setConversationNotice(error.message, "error"); }
  finally { if (agent.forkPromise === operation) agent.forkPromise = null; syncConversationSendUi(); }
}

async function showProjectDialog() {
  await refreshAgentNodes();
  $("#projectNode").replaceChildren(...[...$("#agentRuntimeNode").options].map((option) => option.cloneNode(true)));
  $("#projectNode").value = $("#agentRuntimeNode").value;
  $("#projectPath").value = $("#conversationCwd").value || dashboardNodes.get($("#projectNode").value)?.desiredAppServer?.defaultCwd || "";
  $("#projectError").textContent = "";
  $("#projectDialog").showModal();
  $("#projectPath").focus();
}

async function loadAgentThreads() {
  const archived = agent.showArchived;
  const response = await api(`/v1/codex/threads?storeId=personal&limit=300&archived=${archived ? 1 : 0}`);
  if (archived !== agent.showArchived) return;
  const selected = currentAgentThread();
  agent.threads = response.data ?? [];
  if (selected && !agent.threads.some(thread => thread.threadId === selected.threadId) && Boolean(selected.archived) !== archived) agent.threads.push(selected);
  if (currentAgentThread()) agent.draftProject = null;
  const title = currentAgentThread()?.title;
  if (title) setConversationTitle(title);
  renderAgentThreads();
}

function removeThreadFromWindow(threadId, deleted) {
  const selected = agent.threadId === threadId;
  const project = selected ? projectForThread(currentAgentThread()) : null;
  agent.threads = agent.threads.filter(thread => thread.threadId !== threadId);
  if (deleted) {
    agent.activeTurns.delete(threadId);
    agent.loadedThreadIds.delete(threadId);
    for (const [key, diagnostic] of agent.diagnostics) if (diagnostic.threadId === threadId) agent.diagnostics.delete(key);
  }
  if (selected) newAgentThread({ project, force: deleted });
  renderAgentThreads();
}

async function archiveThreadFromMenu() {
  if (agent.threadActionPromise || agent.sendPromise || agent.forkPromise) return;
  const threadId = agent.menuThreadId;
  const action = agent.threads.find(thread => thread.threadId === threadId)?.archived ? "restore" : "archive";
  $("#threadOptionsMenu").hidePopover();
  const operation = (async () => {
    const thread = await api(`/v1/codex/threads/${encodeURIComponent(threadId)}?storeId=personal`);
    await api(`/v1/codex/threads/${encodeURIComponent(threadId)}/${action}?storeId=personal`, {
      method: "POST", body: JSON.stringify({ generation: thread.generation, operationId: crypto.randomUUID() }),
    });
    removeThreadFromWindow(threadId, false);
    threadMetadataChannel?.postMessage({ threadId, action });
    await loadAgentThreads();
    toast(action === "archive" ? "已归档，可从侧栏的归档对话中恢复" : "已恢复到对话列表");
  })();
  agent.threadActionPromise = operation; syncConversationSendUi();
  try { await operation; } catch (error) { toast(error.message); }
  finally { agent.threadActionPromise = null; syncConversationSendUi(); }
}

async function showDeleteThreadDialog() {
  const threadId = agent.menuThreadId;
  $("#threadOptionsMenu").hidePopover();
  if (agent.activeTurns.has(threadId)) { toast("请先停止此对话的运行，再删除。"); return; }
  const thread = await api(`/v1/codex/threads/${encodeURIComponent(threadId)}?storeId=personal`);
  agent.deleteTarget = { ...thread, operationId: crypto.randomUUID() };
  $("#threadDeleteName").textContent = thread.title || "未命名会话";
  $("#threadDeleteError").textContent = "";
  $("#threadDeleteDialog").showModal();
  $("#threadDeleteCancel").focus();
}

function renderLocalSessions() {
  const list = clear($("#localSessionList"));
  const source = $("#sessionSourceFilter").value;
  const archive = $("#sessionArchiveFilter").value;
  const query = $("#sessionSearch").value.trim().toLowerCase();
  const filtered = [...agent.sessions.entries()].filter(([, session]) =>
    (source === "all" || (session.clientKind ?? "unknown") === source) &&
    (archive === "all" || Boolean(session.archived) === (archive === "archived")) &&
    [session.title, session.threadId, session.cwd].some((value) => String(value ?? "").toLowerCase().includes(query)));
  $("#sessionShowMore").classList.toggle("hidden", filtered.length <= agent.sessionVisibleLimit);
  if (!agent.sessions.length) {
    list.append(element("div", "agent-list-empty", "没有发现本地 Codex 会话"));
    return;
  }
  if (!filtered.length) list.append(element("div", "agent-list-empty", "没有匹配的会话"));
  for (const [index, session] of filtered.slice(0, agent.sessionVisibleLimit)) {
    const card = element("article", "local-session");
    const copy = element("div");
    const sourceNode = dashboardNodes.get(agent.sessionNodeId);
    const executionLabel = ({ wsl: "WSL", windows: "Windows", linux: "Linux", android: "Android" })[session.executionMode] ?? session.executionMode ?? "未知";
    copy.append(
      element("strong", "", session.title || "未命名会话"),
      element("span", "session-source", `${({ desktop: "Codex Desktop", cli: "CLI", ide: "IDE 扩展", subagent: "子 Agent" })[session.clientKind] ?? "其他 / 旧节点"}${session.archived ? " · 已归档" : ""} · ${session.codexVersion || "版本未知"}`),
      element("span", "", `${formatBytes(session.sizeBytes)} · ${when(session.modifiedAt)}`),
      element("small", "", `存储：${sourceNode?.hostname ?? "来源节点"} · 运行环境：${executionLabel}`),
      element("small", "", session.cwd || "工作目录未知"),
      element("small", "", session.threadId),
      element("small", "", session.path),
    );
    if (session.historyBase) copy.append(element("small", "", "引用式分支：导入时自动读取并补齐祖先历史。"));
    const imported = session.import?.unchanged && session.import.status === "imported" && session.import.storeId === "personal";
    const button = element("button", imported ? "ghost" : "approve", imported ? "打开会话" : "导入");
    button.type = "button";
    button.dataset.sessionIndex = String(index);
    if (imported) button.dataset.importedThreadId = session.import.threadId;
    if (session.suggestedRuntimeNodeId) button.dataset.runtimeNodeId = session.suggestedRuntimeNodeId;
    button.disabled = Boolean(agent.sessionImportController);
    card.append(copy, button);
    list.append(card);
  }
}

async function scanLocalSessions() {
  const nodeId = $("#sessionSourceNode").value;
  if (!nodeId) throw new Error("没有支持本地会话发现的节点");
  const epoch = ++agent.sessionScanEpoch;
  agent.sessions = [];
  agent.sessionNodeId = null;
  renderLocalSessions();
  $("#sessionScanState").textContent = "正在扫描桌面 App、CLI 和归档会话…";
  let response;
  try { response = await api(`/v1/nodes/${nodeId}/codex-sessions`); }
  catch (error) { if (epoch !== agent.sessionScanEpoch) return; throw error; }
  if (epoch !== agent.sessionScanEpoch || nodeId !== $("#sessionSourceNode").value) return;
  agent.sessions = response.sessions ?? [];
  agent.sessionNodeId = nodeId;
  agent.sessionVisibleLimit = 40;
  renderLocalSessions();
  $("#sessionScanState").textContent = `发现 ${agent.sessions.length} 个会话（桌面 ${agent.sessions.filter((s) => s.clientKind === "desktop").length}，归档 ${agent.sessions.filter((s) => s.archived).length}） · ${response.codexHomes?.length ?? 0} 个 CODEX_HOME${response.truncated ? " · 已达扫描上限，结果不完整" : ""}${response.warnings?.length ? ` · ${response.warnings.length} 个读取警告` : ""}${agent.sessions.some((s) => !s.clientKind) ? " · 更新源节点可识别桌面来源并扫描归档" : ""}`;
  $("#sessionScanState").title = (response.warnings ?? []).join("\n");
}

async function openImportedSession(threadId, runtimeNodeId = null) {
  if (agent.sendPromise) throw new Error("请等待当前消息提交完成");
  if (runtimeNodeId && [...$("#agentRuntimeNode").options].some((option) => option.value === runtimeNodeId)) {
    $("#agentRuntimeNode").value = runtimeNodeId;
    if (agent.socketNodeId !== runtimeNodeId) closeAgentSocket();
  }
  if (!$("#agentRuntimeNode").value) throw new Error("请先选择一个 Codex 运行节点。导入来源与运行位置可以不同。");
  show("agentView");
  closeAgentThreadDrawerOnMobile();
  await resumeAgentThread(threadId);
}

async function importLocalSession(index, button) {
  if (agent.sessionImportController) return;
  const session = agent.sessions[index];
  const nodeId = agent.sessionNodeId;
  if (!session || !nodeId) return;
  if (button.dataset.importedThreadId) return openImportedSession(button.dataset.importedThreadId, button.dataset.runtimeNodeId);
  button.disabled = true;
  button.textContent = "导入中…";
  const controller = new AbortController();
  agent.sessionImportController = controller;
  $("#sessionImportProgress").classList.remove("hidden");
  $("#sessionImportCancel").classList.remove("hidden");
  $("#sessionImportState").textContent = "正在准备导入…";
  $("#sessionImportMeter").removeAttribute("value");
  const startedAt = Date.now();
  for (const button of $("#localSessionList").querySelectorAll("button")) button.disabled = true;
  try {
    await refreshAdminCsrf();
    const response = await fetch(`/v1/nodes/${nodeId}/codex-session-imports`, {
      method: "POST", credentials: "same-origin", signal: controller.signal,
      headers: { "content-type": "application/json", accept: "application/x-ndjson", "x-mira-csrf": csrfToken },
      body: JSON.stringify({ path: session.path, storeId: "personal", runtimeNodeId: session.suggestedRuntimeNodeId ?? null }),
    });
    if (!response.ok) throw new Error((await response.json()).error ?? `HTTP ${response.status}`);
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let carry = "";
    let result = null;
    for (;;) {
      const { value, done } = await reader.read();
      carry += decoder.decode(value, { stream: !done });
      const lines = carry.split("\n");
      carry = lines.pop();
      for (const line of lines) {
        if (!line.trim()) continue;
        const event = JSON.parse(line);
        if (event.type === "error") throw new Error(event.error ?? "导入失败");
        if (event.type === "complete") result = event;
        if (event.type !== "progress") continue;
        const label = ({ scanning: "扫描源会话", resolving: "定位祖先历史", reading: event.ancestor ? "读取祖先历史" : "读取并暂存", validating: "校验已有历史", publishing: "写入统一会话" })[event.phase] ?? "导入中";
        const current = event.bytes ?? event.records;
        const total = event.totalBytes ?? event.totalRecords;
        const meter = $("#sessionImportMeter");
        if (total > 0) { meter.max = total; meter.value = current ?? 0; } else meter.removeAttribute("value");
        const amount = event.bytes !== undefined ? `${formatBytes(current)} / ${formatBytes(total)}` : `${current ?? 0} / ${total ?? "—"} 条记录`;
        $("#sessionImportState").textContent = `${label} · ${amount} · ${Math.floor((Date.now() - startedAt) / 1000)} 秒`;
      }
      if (done) break;
    }
    if (!result) throw new Error("导入连接已断开，未收到完成确认；请扫描确认状态后重试");
    toast(`已导入 ${result.itemCount} 条记录`);
    $("#sessionImportState").textContent = `导入完成 · ${result.itemCount} 条记录`;
    await Promise.all([nodeId === $("#sessionSourceNode").value ? scanLocalSessions() : Promise.resolve(), loadAgentThreads()]);
  } catch (error) {
    $("#sessionImportState").textContent = error.name === "AbortError" ? "已请求取消；未提交的数据将回滚。若恰好已完成提交，重新扫描可确认结果。" : `导入失败：${error.message}`;
    if (error.name === "AbortError") return;
    throw error;
  } finally {
    if (agent.sessionImportController === controller) agent.sessionImportController = null;
    $("#sessionImportCancel").classList.add("hidden");
    renderLocalSessions();
  }
}

async function startAgentRuntime({ allowStart = true } = {}) {
  const nodeId = $("#agentRuntimeNode").value;
  if (!nodeId) throw new Error("没有可运行 Codex 的节点");
  if (allowStart) agent.connectionWanted = true;
  if (agent.socketInitialized && agent.socket?.readyState === WebSocket.OPEN && agent.socketNodeId === nodeId) return;
  if (agent.runtimePromise?.nodeId === nodeId) return agent.runtimePromise.promise;
  const promise = connectAgentRuntime(nodeId, allowStart);
  agent.runtimePromise = { nodeId, promise };
  try { await promise; }
  finally { if (agent.runtimePromise?.promise === promise) agent.runtimePromise = null; }
}

async function connectAgentRuntime(nodeId, allowStart) {
  closeAgentSocket({ preserveSubmission: true });
  const epoch = agent.runtimeStartEpoch;
  let node = await api(`/v1/nodes/${nodeId}`);
  if (epoch !== agent.runtimeStartEpoch || $("#agentRuntimeNode").value !== nodeId) throw new Error("连接已取消");
  dashboardNodes.set(node.nodeId, node);
  if (node.reportedAppServer?.status === "running") return connectAgentSocket(nodeId);
  if (!allowStart) throw new Error("运行节点尚未就绪");
  setAgentRuntimeState("正在启动运行节点…", "offline");
  await api(`/v1/codex/runtimes/${nodeId}/start`, { method: "POST", body: JSON.stringify({ storeId: "personal" }) });
  // A fresh Node may need its independent Codex package before it can start.
  // Keep the page responsive and show preparation instead of a 30-second timeout.
  const deadline = Date.now() + (dashboardNodes.get(nodeId)?.capabilities?.codexRuntimeDownload ? 21 * 60_000 : 30_000);
  let lastError = "";
  let errorSince = 0;
  while (Date.now() < deadline) {
    if (epoch !== agent.runtimeStartEpoch || $("#agentRuntimeNode").value !== nodeId) throw new Error("已取消等待 App Server 启动");
    node = await api(`/v1/nodes/${nodeId}`);
    dashboardNodes.set(node.nodeId, node);
    if (node.reportedAppServer?.status === "running") break;
    const preparing = node.reportedAppServer?.runtimePreparing === true;
    if (preparing) setAgentRuntimeState("首次准备 Codex 运行包，下载与校验可能需要几分钟；节点其他功能可继续使用…", "offline");
    const currentError = preparing ? "" : (node.reportedAppServer?.lastError ?? "");
    if (currentError !== lastError) {
      lastError = currentError;
      errorSince = currentError ? Date.now() : 0;
    } else if (currentError && Date.now() - errorSince > 5_000) {
      throw new Error(currentError);
    }
    await new Promise((resolve) => setTimeout(resolve, preparing ? 2_000 : 500));
  }
  if (node?.reportedAppServer?.status !== "running") throw new Error("App Server 启动超时");
  if (epoch !== agent.runtimeStartEpoch || $("#agentRuntimeNode").value !== nodeId) throw new Error("已取消等待 App Server 启动");
  await connectAgentSocket(nodeId);
}

async function stopAgentRuntime() {
  const nodeId = $("#agentRuntimeNode").value;
  if (!nodeId) return;
  stopAgentRecovery();
  await api(`/v1/codex/runtimes/${nodeId}/stop`, { method: "POST", body: JSON.stringify({ storeId: "personal" }) });
  setAgentRuntimeState("已请求停止 App Server", "offline");
}

async function resumeAgentThreadOnSocket(threadId) {
  const pending = agent.resumePromises.get(threadId);
  if (pending?.socket === agent.socket) return pending.promise;
  const socket = agent.socket;
  const promise = restoreAgentThread(threadId, socket);
  agent.resumePromises.set(threadId, { socket, promise });
  try { return await promise; }
  finally { if (agent.resumePromises.get(threadId)?.promise === promise) agent.resumePromises.delete(threadId); }
}

async function restoreAgentThread(threadId, socket) {
  const projectedThread = agent.threads.find((thread) => thread.threadId === threadId);
  const projectedCwd = typeof projectedThread?.cwd === "string" ? projectedThread.cwd.trim() : "";
  const params = { threadId, excludeTurns: true };
  const revision = agent.liveRevision;
  if (projectedCwd) params.cwd = projectedCwd;
  const result = await rpc("thread/resume", params, 120_000);
  if (agent.socket !== socket) throw new Error("恢复会话时 App Server 通道已变更，请重新发送");
  agent.loadedThreadIds.add(result.thread.id);
  if (agent.threadId !== threadId) return result.thread;
  agent.previousRuntimeNodeId = projectedThread?.runtimeNodeId ?? projectedThread?.sourceNodeId ?? null;
  agent.threadRuntimeNodeId = agent.socketNodeId;
  const currentProjection = agent.threads.find((thread) => thread.threadId === threadId) ?? projectedThread;
  setConversationTitle(currentProjection?.name || result.thread.name || result.thread.preview || currentProjection?.title || "Codex 会话");
  const resumedCwd = result.cwd ?? projectedCwd;
  setConversationMeta(resumedCwd, result.model);
  $("#conversationCwd").value = resumedCwd ?? "";
  if (result.thread.status?.type === "active" && agent.liveRevision === revision && !agent.activeTurns.has(threadId)) agent.activeTurns.set(threadId, null);
  syncActiveTurnUi();
  if (agent.activeTurns.has(threadId)) await refreshActiveTurn(threadId, socket);
  return result.thread;
}

async function refreshActiveTurn(threadId, socket) {
  if (!socket || agent.socket !== socket || agent.threadId !== threadId) return;
  const pending = agent.turnStateRequests.get(threadId);
  if (pending?.socket === socket) return pending.promise;
  const revision = agent.liveRevision;
  const epoch = agent.selectionEpoch;
  const operation = (async () => {
    try {
      // Only the latest turn's metadata is needed, never its messages or tools.
      const result = await rpc("thread/turns/list", { threadId, limit: 1, sortDirection: "desc", itemsView: "notLoaded" }, 15_000);
      if (agent.socket !== socket || agent.threadId !== threadId || agent.selectionEpoch !== epoch || agent.liveRevision !== revision) return;
      const turn = result.data?.[0];
      if (turn?.status === "inProgress" && !agent.turnTimings.get(turn.id)?.completedAt) {
        agent.activeTurns.set(threadId, turn.id);
        agent.turnThreads.set(turn.id, threadId);
      } else if (turn && ["completed", "failed", "interrupted"].includes(turn.status) &&
          (!agent.activeTurns.get(threadId) || agent.activeTurns.get(threadId) === turn.id)) {
        agent.activeTurns.delete(threadId);
        agent.turnThreads.delete(turn.id);
        const timing = agent.turnTimings.get(turn.id) ?? {};
        timing.completedAt ??= Date.now();
        agent.turnTimings.set(turn.id, timing);
        void refreshCompletedTranscript(threadId);
      }
      syncActiveTurnUi();
    } catch { /* Preserve the last known active state until it can be confirmed. */ }
  })();
  agent.turnStateRequests.set(threadId, { socket, promise: operation });
  try { await operation; }
  finally { if (agent.turnStateRequests.get(threadId)?.promise === operation) agent.turnStateRequests.delete(threadId); }
}

async function resumeAgentThread(threadId, { updateRoute = true } = {}) {
  if (agent.sendPromise) return;
  if (updateRoute) writeBrowserRoute("agent", threadId);
  const epoch = ++agent.selectionEpoch;
  agent.connectionWanted = true;
  agent.threadId = threadId;
  resetAgentTranscript(threadId);
  agent.threadRuntimeNodeId = null;
  setConversationTitle("正在打开会话…");
  $("#conversationMeta").textContent = "";
  clear($("#conversationTrace")).append(element("div", "conversation-empty", "正在加载最近消息…"));
  renderTurnDiagnostics();
  let projected = currentAgentThread();
  if (!projected) {
    try {
      projected = await api(`/v1/codex/threads/${encodeURIComponent(threadId)}?storeId=personal`);
      if (epoch !== agent.selectionEpoch) return;
      agent.threads.push(projected);
    } catch (error) {
      if (epoch !== agent.selectionEpoch) return;
      stopAgentRecovery();
      setConversationTitle("会话不可用");
      setConversationNotice(error.message, "error");
      return;
    }
  }
  agent.threadRuntimeNodeId = projected?.runtimeNodeId ?? null;
  agent.projectOpen.set(projectForThread(projected).key, true);
  const preferredNode = projected?.runtimeNodeId ?? projected?.sourceNodeId;
  if (preferredNode && [...$("#agentRuntimeNode").options].some((option) => option.value === preferredNode)) {
    $("#agentRuntimeNode").value = preferredNode;
  }
  setConversationTitle(projected?.title || "Codex 会话");
  setConversationMeta(projected?.cwd, projected?.model);
  $("#conversationCwd").value = projected?.cwd || "";
  syncActiveTurnUi();
  renderAgentThreads();
  setConversationNotice();
  // Reading a conversation must not wait for a Node, model runtime or a full
  // resume response. The authoritative tail and live subscription are separate.
  const history = loadAgentTranscript(threadId);
  void (async () => {
    try {
      await startAgentRuntime();
      if (epoch !== agent.selectionEpoch) return;
      await resumeAgentThreadOnSocket(threadId);
    } catch {
      if (epoch === agent.selectionEpoch) scheduleAgentRecovery();
    }
  })();
  await history;
}

function newAgentThread({ updateRoute = true, project = null, force = false } = {}) {
  if (agent.sendPromise && !force) return;
  project ??= agent.threadId ? projectForThread(currentAgentThread()) : agent.draftProject;
  if (project?.nodeId) {
    if (![...$("#agentRuntimeNode").options].some((option) => option.value === project.nodeId)) {
      toast("此项目的运行机器不可用，请先添加或选择运行机器。");
      return;
    }
    $("#agentRuntimeNode").value = project.nodeId;
  }
  agent.draftProject = project;
  if (project) agent.projectOpen.set(project.key, true);
  if (updateRoute) writeBrowserRoute("agent");
  agent.selectionEpoch++;
  agent.threadId = null;
  agent.newThreadRequestId = null;
  agent.newThreadRequestSignature = null;
  agent.threadRuntimeNodeId = null;
  agent.previousRuntimeNodeId = null;
  syncActiveTurnUi();
  resetAgentTranscript();
  setConversationTitle("新会话");
  const node = dashboardNodes.get($("#agentRuntimeNode").value);
  $("#conversationCwd").value = project?.cwd || node?.desiredAppServer?.defaultCwd || "";
  setConversationMeta($("#conversationCwd").value);
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

async function prepareTurnInput(text, attachments, progress) {
  const controller = new AbortController();
  agent.uploadController = controller;
  const signal = controller.signal;
  const annotations = [];
  const inputs = [];
  let directory = null;
  let uploaded = 0;
  const total = attachments.reduce((sum, file) => sum + file.size, 0);
  const nodeId = agent.socketNodeId;
  $("#conversationUploadCancel").classList.toggle("hidden", !attachments.length);
  try {
    if (attachments.length) {
      const cwd = $("#conversationCwd").value.trim();
      directory = await prepareUploadDirectory(nodeId, cwd);
      signal.throwIfAborted();
      const staged = [];
      for (const [index, file] of attachments.entries()) {
        const path = joinPath(directory, safeAttachmentName(file.name, index));
        let offset = 0;
        do {
          signal.throwIfAborted();
          const status = `上传 ${index + 1}/${attachments.length} · ${file.name} · ${formatBytes(uploaded)} / ${formatBytes(total)}${total ? ` · ${Math.floor(uploaded / total * 100)}%` : ""}`;
          $("#conversationHint").textContent = status;
          updateReplyProgress(progress, { phase: status });
          const bytes = new Uint8Array(await file.slice(offset, offset + nodeFileChunkBytes).arrayBuffer());
          signal.throwIfAborted();
          // Await the in-flight chunk even after cancellation, then remove only our
          // unique batch directory. This avoids racing cleanup against a late write.
          await invokeNode(nodeId, "file", {
            action: "write", path, encoding: "base64", content: bytesBase64(bytes), overwrite: false,
            append: offset > 0, offset,
          }, 60_000);
          offset += bytes.length;
          uploaded += bytes.length;
          signal.throwIfAborted();
        } while (offset < file.size);
        staged.push({ file, path });
        if (nativeImageAttachment(file)) inputs.push({ type: "localImage", path });
      }
      annotations.push([
        "已附加文件（暂存在当前运行节点）：",
        ...staged.map(({ file, path }) => `- \`${String(file.name).replaceAll("`", "'")}\`：\`${path}\``),
      ].join("\n"));
    }
    const message = [text, ...annotations].filter(Boolean).join("\n\n");
    if (message) inputs.unshift({ type: "text", text: message });
    return { inputs, message };
  } catch (error) {
    if (directory) {
      try { await invokeNode(nodeId, "file", { action: "remove", path: directory, recursive: true }, 60_000); }
      catch { toast(`附件暂存文件清理失败，可在运行节点删除：${directory}`); }
    }
    throw error;
  } finally {
    if (agent.uploadController === controller) agent.uploadController = null;
    $("#conversationUploadCancel").classList.add("hidden");
  }
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

function resizeConversationInput() {
  const input = $("#conversationInput");
  const follow = traceNearBottom();
  const maximum = 144;
  input.style.height = "36px";
  const height = Math.min(maximum, Math.max(36, input.scrollHeight));
  input.style.height = `${height}px`;
  input.style.overflowY = input.scrollHeight > maximum ? "auto" : "hidden";
  if (follow) scrollTraceToBottom();
}

function addComposerFiles(files) {
  if (agent.sendPromise) { toast("请等待当前提交完成，或取消上传后修改附件"); return; }
  for (const file of files) {
    agent.attachments.push(file);
  }
  renderComposerAttachments();
}

async function sendAgentMessage(text, attachments = [], progress = null) {
  updateReplyProgress(progress, { phase: agent.socket?.readyState === WebSocket.OPEN ? "正在发送…" : "正在连接运行节点…" });
  agent.connectionWanted = true;
  if (!agent.socketInitialized || !agent.socket || agent.socket.readyState !== WebSocket.OPEN) await startAgentRuntime();
  // A thread ID survives reconnects, but the new App Server may have no live
  // thread handle. Restore it before uploading attachments or starting a turn.
  if (agent.threadId && !agent.loadedThreadIds.has(agent.threadId)) {
    updateReplyProgress(progress, { phase: "正在恢复会话…" });
    await resumeAgentThreadOnSocket(agent.threadId);
  }
  if (!agent.threadId) {
    updateReplyProgress(progress, { phase: "正在创建会话…" });
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
    writeBrowserRoute("agent", agent.threadId, { replace: true });
    agent.loadedThreadIds.add(agent.threadId);
    agent.newThreadRequestId = null;
    agent.newThreadRequestSignature = null;
    agent.threadRuntimeNodeId = agent.socketNodeId;
    agent.previousRuntimeNodeId = null;
    setConversationTitle("新会话");
    const startedCwd = started.cwd ?? cwd;
    setConversationMeta(startedCwd, started.model);
    $("#conversationCwd").value = startedCwd ?? "";
  }
  updateReplyProgress(progress, { threadId: agent.threadId, phase: attachments.length ? "正在上传附件…" : "正在发送…" });
  const prepared = await prepareTurnInput(text, attachments, progress);
  updateReplyProgress(progress, { phase: "正在提交消息…" });
  const optimisticCompletedAt = new Date().toISOString();
  const optimistic = upsertTrace(`user-${Date.now()}`, "user", "你", prepared.message, "已发送", {
    completedAt: optimisticCompletedAt,
    elapsedMs: Number.isFinite(progress?.startedAt) ? Math.max(0, Date.parse(optimisticCompletedAt) - progress.startedAt) : null,
  });
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
  // If the live user-item notification was missed, the canonical user message
  // can still replace this optimistic card by turn + body during reconciliation.
  if (result.turn?.id) optimistic.dataset.turnId = result.turn.id;
  // turn/start can accept input into an already-running turn, without emitting
  // another turn/started. The RPC acknowledgement ends submission in both cases.
  replyProgress.finish(progress);
  renderReplyProgress();
  if (result.turn?.id && !agent.turnTimings.get(result.turn.id)?.completedAt &&
      !["completed", "failed", "interrupted"].includes(result.turn.status)) {
    updateReplyProgress(progress, { turnId: result.turn.id });
    agent.activeTurns.set(turnThreadId, result.turn.id);
    agent.turnThreads.set(result.turn.id, turnThreadId);
    const timing = agent.turnTimings.get(result.turn.id) ?? {};
    timing.startedAt ??= progress?.startedAt ?? Date.now();
    agent.turnTimings.set(result.turn.id, timing);
  }
  syncActiveTurnUi();
}

async function openAgentConsole() {
  writeBrowserRoute("agent", agent.threadId);
  show("agentView");
  await Promise.all([refreshAgentNodes(), loadAgentThreads()]);
  if (agent.threadId) {
    agent.connectionWanted = true;
    void recoverAgentSession({ probe: true });
  }
}

async function openRuntimeConsole() {
  writeBrowserRoute("runtime");
  show("runtimeView");
  await Promise.all([refreshAgentNodes(), loadAgentThreads()]);
}

async function leaveAgentConsole() {
  writeBrowserRoute("nodes");
  stopAgentRecovery();
  show("dashboardView");
  await loadDashboard();
}

async function navigateGlobal(target) {
  if (target === "agent") {
    if (!$("#workspaceView").classList.contains("hidden")) await leaveWorkspace();
    if ($("#agentView").classList.contains("hidden")) await openAgentConsole();
    return;
  }
  if (target === "runtime") {
    if (!$("#workspaceView").classList.contains("hidden")) await leaveWorkspace();
    await openRuntimeConsole();
    return;
  }
  if (!$("#workspaceView").classList.contains("hidden")) await leaveWorkspace();
  else if (!$("#agentView").classList.contains("hidden") || !$("#runtimeView").classList.contains("hidden")) await leaveAgentConsole();
  else if (!$("#dashboardView").classList.contains("hidden")) await loadDashboard();
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
    await restoreBrowserRoute();
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
    agent.diagnostics.clear();
    clearAppRoute();
    stopAgentRecovery({ resetTurnState: true });
    disposeTerminal();
    workspace.node = null;
    show("loginView");
  }
});

$("#themeToggle").addEventListener("click", toggleTheme);
$("#agentThemeToggle").addEventListener("click", toggleTheme);
$("#globalNodes").addEventListener("click", () => navigateGlobal("nodes").catch((error) => toast(error.message)));
$("#globalAgent").addEventListener("click", () => navigateGlobal("agent").catch((error) => toast(error.message)));
$("#globalRuntime").addEventListener("click", () => navigateGlobal("runtime").catch((error) => toast(error.message)));

$("#agentConsoleButton").addEventListener("click", () => openAgentConsole().catch((error) => toast(error.message)));
$("#runtimeOpenChat").addEventListener("click", () => navigateGlobal("agent").catch((error) => toast(error.message)));
$("#agentHome").addEventListener("click", () => navigateGlobal("nodes").catch((error) => toast(error.message)));
$("#agentManage").addEventListener("click", () => navigateGlobal("runtime").catch((error) => toast(error.message)));
$("#agentThreadDrawerToggle").addEventListener("click", () => setAgentThreadDrawer(!$("#agentThreadDrawer").classList.contains("open")));
$("#agentThreadDrawerClose").addEventListener("click", () => setAgentThreadDrawer(false));
$("#agentThreadDrawerBackdrop").addEventListener("click", () => setAgentThreadDrawer(false));
window.addEventListener("popstate", () => {
  if (["loginView", "setupView"].includes(document.body.dataset.view)) return;
  agent.selectionEpoch++;
  void restoreBrowserRoute().catch((error) => setConversationNotice(error.message, "error"));
});
agentThreadDrawerWide.addEventListener("change", () => setAgentThreadDrawer(agentThreadDrawerWide.matches, { focus: false }));
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && $("#agentThreadDrawer").classList.contains("open")) setAgentThreadDrawer(false);
});
$("#agentRefresh").addEventListener("click", () => Promise.all([refreshAgentNodes(), loadAgentThreads()]).then(() => toast("Agent 状态已刷新")).catch((error) => toast(error.message)));
$("#agentRuntimeStart").addEventListener("click", () => startAgentRuntime().catch((error) => {
  setAgentRuntimeState(`启动失败：${error.message}`, "offline");
  setConversationNotice(error.message, "error");
}));
$("#agentRuntimeStop").addEventListener("click", () => stopAgentRuntime().catch((error) => toast(error.message)));
$("#agentRuntimeSaveCwd").addEventListener("click", () => saveAgentRuntimeDefaultCwd().catch((error) => toast(error.message)));
$("#agentRuntimeNode").addEventListener("change", () => {
  if (agent.socketNodeId !== $("#agentRuntimeNode").value) stopAgentRecovery();
  const node = dashboardNodes.get($("#agentRuntimeNode").value);
  $("#agentRuntimeDefaultCwd").value = node?.desiredAppServer?.defaultCwd ?? "";
  if (!agent.threadId) {
    $("#conversationCwd").value = node?.desiredAppServer?.defaultCwd ?? "";
    setConversationMeta($("#conversationCwd").value);
  }
  setAgentRuntimeState(`${node?.reportedAppServer?.status ?? "stopped"} · ${node?.hostname ?? ""}`, node?.status === "online" ? "online" : "offline");
  syncConversationSendUi();
});

document.addEventListener("visibilitychange", () => {
  if (document.hidden) {
    clearTimeout(agent.reconnectTimer);
    clearTimeout(agent.heartbeatTimer);
  } else void recoverAgentSession({ probe: true });
});
window.addEventListener("pageshow", (event) => { if (event.persisted) void recoverAgentSession({ probe: true }); });
document.addEventListener("resume", () => { void recoverAgentSession({ probe: true }); });
window.addEventListener("online", () => { void recoverAgentSession({ probe: true }); });
window.addEventListener("offline", () => {
  clearTimeout(agent.reconnectTimer);
  clearTimeout(agent.heartbeatTimer);
  if (agent.connectionWanted) setAgentRuntimeState("网络已断开，恢复后将自动连接", "offline");
});

const conversationOverlayObserver = new ResizeObserver(() => {
  const head = $(".conversation-head").getBoundingClientRect().bottom - $(".conversation-card").getBoundingClientRect().top;
  const notice = $("#conversationNotice");
  const noticeHeight = notice.classList.contains("hidden") ? 0 : notice.getBoundingClientRect().height + 8;
  $(".conversation-card").style.setProperty("--conversation-overlay-height", `${head + noticeHeight}px`);
});
conversationOverlayObserver.observe($(".conversation-head"));
conversationOverlayObserver.observe($("#conversationNotice"));
const conversationWidthObserver = new ResizeObserver(() => {
  const scroll = traceScroller();
  $(".conversation-card").style.setProperty("--conversation-scrollbar-width", `${scroll.offsetWidth - scroll.clientWidth}px`);
});
conversationWidthObserver.observe(traceScroller());
traceScroller().addEventListener("scroll", scheduleOlderTranscriptLoad, { passive: true });
$("#agentNewThread").addEventListener("click", () => { newAgentThread(); closeAgentThreadDrawerOnMobile(); });
$("#agentThreadList").addEventListener("click", (event) => {
  const project = event.target.closest("button[data-project-new]");
  if (project) {
    event.preventDefault();
    newAgentThread({ project: { key: project.dataset.projectNew, nodeId: project.dataset.projectNode, cwd: project.dataset.projectPath } });
    closeAgentThreadDrawerOnMobile();
    $("#conversationInput").focus();
    return;
  }
  const menu = event.target.closest("button[data-thread-menu]");
  if (menu) { openThreadMenu(menu.dataset.threadMenu, menu); return; }
  const openWindow = event.target.closest("a[data-open-thread-window]");
  if (openWindow) {
    openThreadWindow(event, openWindow);
    return;
  }
  const button = event.target.closest("button[data-thread-id]");
  if (button) {
    closeAgentThreadDrawerOnMobile();
    resumeAgentThread(button.dataset.threadId).catch((error) => setConversationNotice(error.message, "error"));
  }
});
$("#conversationTrace").addEventListener("click", (event) => {
  const dismiss = event.target.closest("[data-dismiss-diagnostic]");
  if (dismiss) {
    const diagnostic = agent.diagnostics.get(dismiss.dataset.dismissDiagnostic);
    if (diagnostic) diagnostic.dismissed = true;
    dismiss.closest("[data-diagnostic-key]").remove();
    return;
  }
  const file = event.target.closest("[data-node-file-path]");
  if (file) {
    event.preventDefault();
    openNodeFile(file.dataset.nodeFilePath, Number(file.dataset.nodeFileLine) || null).catch((error) => {
      if (error.name === "AbortError") return;
      $("#nodeFileMeta").textContent = "读取失败";
      $("#nodeFileLoading").textContent = error.message;
      toast(error.message);
    });
    return;
  }
  if (event.target.closest("button[data-load-older]")) {
    void loadOlderAgentTranscript();
  }
});
$("#sessionScan").addEventListener("click", () => scanLocalSessions().catch((error) => {
  $("#sessionScanState").textContent = `扫描失败：${error.message}`;
}));
$("#sessionSourceNode").addEventListener("change", () => {
  agent.sessionScanEpoch++;
  agent.sessionNodeId = null;
  agent.sessions = [];
  renderLocalSessions();
  $("#sessionScanState").textContent = "节点已切换，请重新扫描。";
});
for (const id of ["sessionSourceFilter", "sessionArchiveFilter", "sessionSearch"]) {
  $(`#${id}`).addEventListener("input", () => { agent.sessionVisibleLimit = 40; renderLocalSessions(); });
}
$("#sessionShowMore").addEventListener("click", () => { agent.sessionVisibleLimit += 40; renderLocalSessions(); });
$("#sessionImportCancel").addEventListener("click", () => agent.sessionImportController?.abort());
$("#localSessionList").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-session-index]");
  if (button) importLocalSession(Number(button.dataset.sessionIndex), button).catch((error) => toast(error.message));
});
$("#conversationForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  if (agent.sendPromise || agent.forkPromise || agent.threadActionPromise) return;
  const text = $("#conversationInput").value.trim();
  const attachments = [...agent.attachments];
  if (!text && !attachments.length) return;
  setConversationNotice();
  const progress = replyProgress.begin(agent.threadId, agent.turnId);
  agent.replySubmission = progress;
  renderReplyProgress();
  const operation = sendAgentMessage(text, attachments, progress);
  agent.sendPromise = operation;
  syncConversationSendUi();
  try {
    await operation;
    if ($("#conversationInput").value.trim() === text) {
      $("#conversationInput").value = "";
      resizeConversationInput();
    }
    agent.attachments = agent.attachments.filter((file) => !attachments.includes(file));
    renderComposerAttachments();
    $("#conversationHint").textContent = "可粘贴或拖入图片与文件 · 上传支持取消";
  } catch (error) {
    replyProgress.finish(progress);
    renderReplyProgress();
    setConversationNotice(error.name === "AbortError" ? "已取消上传，未发送消息；附件仍保留。" : error.message, error.name === "AbortError" ? "" : "error");
    $("#conversationHint").textContent = "附件仍保留，可重试";
  } finally {
    if (agent.replySubmission === progress) agent.replySubmission = null;
    if (agent.sendPromise === operation) agent.sendPromise = null;
    syncConversationSendUi();
  }
});
$("#conversationInput").addEventListener("keydown", (event) => {
  composerShiftEnter = event.key === "Enter" && event.shiftKey;
  if (event.key === "Enter" && !event.shiftKey && !event.isComposing && event.keyCode !== 229) {
    event.preventDefault();
    if (!event.repeat && !agent.sendPromise && !agent.forkPromise && !agent.threadActionPromise && $("#agentRuntimeNode").value) $("#conversationForm").requestSubmit();
  }
});
// Some Android keyboards use beforeinput for the IME action instead of Enter.
let composerShiftEnter = false;
$("#conversationInput").addEventListener("keyup", () => { composerShiftEnter = false; });
$("#conversationInput").addEventListener("beforeinput", (event) => {
  if (!["insertParagraph", "insertLineBreak"].includes(event.inputType) || event.isComposing || composerShiftEnter) return;
  event.preventDefault();
  if (!agent.sendPromise && !agent.forkPromise && !agent.threadActionPromise && $("#agentRuntimeNode").value) $("#conversationForm").requestSubmit();
});
$("#conversationInput").addEventListener("input", resizeConversationInput);
$("#conversationAttach").addEventListener("click", () => $("#conversationFileInput").click());
$("#conversationUploadCancel").addEventListener("click", () => {
  agent.uploadController?.abort();
  $("#conversationHint").textContent = "正在取消并清理当前上传…";
});
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
$("#nodeFileTextMore").addEventListener("click", () => loadMoreFilePreview().catch((error) => toast(error.message)));
$("#nodeFileDialog").addEventListener("close", resetNodeFileDialog);
$("#agentInterrupt").addEventListener("click", async () => {
  const threadId = agent.threadId;
  const turnId = agent.turnId;
  const key = JSON.stringify([threadId, turnId]);
  if (!threadId || !turnId || agent.interruptRequests.has(key)) return;
  agent.interruptRequests.add(key);
  syncConversationSendUi();
  try { await rpc("turn/interrupt", { threadId, turnId }); }
  catch (error) { toast(error.message); }
  finally { agent.interruptRequests.delete(key); syncConversationSendUi(); }
});

$("#conversationMenuToggle").addEventListener("click", (event) => openThreadMenu(agent.threadId, event.currentTarget));
$("#agentThreadList").addEventListener("contextmenu", (event) => {
  const row = event.target.closest("[data-thread-row]");
  if (!row) return;
  event.preventDefault();
  openThreadMenu(row.dataset.threadRow, row, { x: event.clientX, y: event.clientY });
});
$("#agentThreadList").addEventListener("keydown", (event) => {
  if (event.key !== "ContextMenu" && !(event.shiftKey && event.key === "F10")) return;
  const row = event.target.closest("[data-thread-row]");
  if (row) { event.preventDefault(); openThreadMenu(row.dataset.threadRow, row); }
});
$("#threadOptionsMenu").addEventListener("keydown", (event) => {
  const items = [...event.currentTarget.querySelectorAll('[role="menuitem"]')];
  const index = items.indexOf(document.activeElement);
  const next = { ArrowDown: (index + 1) % items.length, ArrowUp: (index + items.length - 1) % items.length, Home: 0, End: items.length - 1 }[event.key];
  if (next !== undefined) { event.preventDefault(); items[next].focus(); }
});
$("#threadFork").addEventListener("click", forkThreadFromMenu);
$("#threadRename").addEventListener("click", () => editThreadTitle().catch((error) => toast(error.message)));
$("#threadCopyId").addEventListener("click", async () => {
  try { await navigator.clipboard.writeText(agent.menuThreadId); $("#threadOptionsMenu").hidePopover(); toast("已复制对话 ID"); }
  catch { toast("复制失败，请允许剪贴板访问后重试。"); }
});
$("#threadOpenWindow").addEventListener("click", (event) => { openThreadWindow(event, event.currentTarget); $("#threadOptionsMenu").hidePopover(); });
$("#threadRenameCancel").addEventListener("click", () => $("#threadRenameDialog").close());
const threadMetadataChannel = typeof BroadcastChannel === "function" ? new BroadcastChannel("mira.thread.metadata") : null;
threadMetadataChannel?.addEventListener("message", (event) => {
  if (event.data?.action === "delete") removeThreadFromWindow(event.data.threadId, true);
  else if (["archive", "restore"].includes(event.data?.action)) {
    const thread = agent.threads.find(thread => thread.threadId === event.data.threadId);
    if (thread) thread.archived = event.data.action === "archive";
  }
  if (["agentView", "runtimeView"].includes(document.body.dataset.view)) void loadAgentThreads().catch(() => {});
});
$("#threadArchive").addEventListener("click", archiveThreadFromMenu);
$("#threadDelete").addEventListener("click", () => showDeleteThreadDialog().catch(error => toast(error.message)));
$("#agentArchiveToggle").addEventListener("click", () => {
  agent.showArchived = !agent.showArchived;
  $("#agentArchiveToggle").setAttribute("aria-pressed", String(agent.showArchived));
  $("#agentArchiveLabel").textContent = agent.showArchived ? "返回对话列表" : "归档对话";
  void loadAgentThreads().catch(error => toast(error.message));
});
$("#threadDeleteCancel").addEventListener("click", () => $("#threadDeleteDialog").close());
$("#threadDeleteForm").addEventListener("submit", async event => {
  event.preventDefault();
  const thread = agent.deleteTarget;
  if (!thread || agent.threadActionPromise || agent.sendPromise || agent.forkPromise) return;
  if (agent.activeTurns.has(thread.threadId)) { $("#threadDeleteError").textContent = "请先停止此对话的运行，再删除。"; return; }
  const operation = (async () => {
    await api(`/v1/codex/threads/${encodeURIComponent(thread.threadId)}?storeId=personal`, {
      method: "DELETE", body: JSON.stringify({ generation: thread.generation, itemCount: thread.itemCount, operationId: thread.operationId }),
    });
    removeThreadFromWindow(thread.threadId, true);
    threadMetadataChannel?.postMessage({ threadId: thread.threadId, action: "delete" });
    $("#threadDeleteDialog").close();
    await loadAgentThreads();
    toast("对话已永久删除");
  })();
  agent.threadActionPromise = operation; syncConversationSendUi();
  $("#threadDeleteConfirm").disabled = true;
  $("#threadDeleteConfirm").textContent = "正在删除…";
  try { await operation; } catch (error) { $("#threadDeleteError").textContent = error.message; }
  finally {
    agent.threadActionPromise = null; syncConversationSendUi();
    $("#threadDeleteConfirm").disabled = false; $("#threadDeleteConfirm").textContent = "永久删除";
  }
});
$("#threadRenameForm").addEventListener("submit", async (event) => {
  event.preventDefault();
  const rename = agent.rename;
  const save = $("#threadRenameSave");
  if (!rename || save.disabled) return;
  const name = $("#threadRenameInput").value.trim();
  if (rename.name !== name) { rename.name = name; rename.operationId = crypto.randomUUID(); }
  save.disabled = true;
  $("#threadRenameError").textContent = "";
  try {
    const thread = await api(`/v1/codex/threads/${encodeURIComponent(rename.threadId)}?storeId=personal`, {
      method: "PATCH", body: JSON.stringify({ name, expectedName: rename.expectedName, generation: rename.generation, operationId: rename.operationId }),
    });
    agent.threads = agent.threads.map((value) => value.threadId === thread.threadId ? thread : value);
    if (agent.threadId === thread.threadId) setConversationTitle(thread.title);
    renderAgentThreads();
    threadMetadataChannel?.postMessage({ threadId: thread.threadId });
    $("#threadRenameDialog").close();
    toast("标题已保存");
  } catch (error) { $("#threadRenameError").textContent = error.message; }
  finally { save.disabled = false; }
});
$("#agentNewProject").addEventListener("click", () => showProjectDialog().catch((error) => toast(error.message)));
$("#projectCancel").addEventListener("click", () => $("#projectDialog").close());
$("#projectNode").addEventListener("change", () => { $("#projectPath").value = dashboardNodes.get($("#projectNode").value)?.desiredAppServer?.defaultCwd || ""; });
$("#projectForm").addEventListener("submit", (event) => {
  event.preventDefault();
  if (agent.sendPromise) return;
  const nodeId = $("#projectNode").value;
  const cwd = $("#projectPath").value.trim();
  const windows = dashboardNodes.get(nodeId)?.platform === "windows";
  if (!nodeId || !(windows ? /^(?:[a-z]:[\\/]|\\\\[^\\]+\\[^\\]+)/i.test(cwd) : cwd.startsWith("/"))) {
    $("#projectError").textContent = "请选择运行机器，并填写该机器上的绝对路径。";
    return;
  }
  newAgentThread({ project: projectForThread({ runtimeNodeId: nodeId, cwd }) });
  $("#projectDialog").close();
  closeAgentThreadDrawerOnMobile();
  $("#conversationInput").focus();
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
      await restoreBrowserRoute();
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

syncThemeControl();
initializePwa();
void bootstrap();

// Shared, rebuildable presentation metadata for App Server items and stored
// rollout items. Never interpret arbitrary shell/JavaScript as an activity.
import { outputImages, imageJsonReplacer, imagePath } from "./trace-images.js";

export function itemType(item) {
  return String(item?.type ?? "").replaceAll("_", "").toLowerCase();
}

function text(value) {
  if (typeof value === "string") return value;
  if (Array.isArray(value)) return value.map(text).filter(Boolean).join("\n");
  if (!value || typeof value !== "object") return "";
  return text(value.text ?? value.content ?? value.output);
}

function compact(value, limit = 160) {
  const line = String(value ?? "").replace(/\s+/g, " ").trim();
  return line.length > limit ? `${line.slice(0, limit)}…` : line;
}

export function reasoningParts(item) {
  // Raw model reasoning is not a readable summary. Older records may expose
  // only summary_text; an absent summary must not create a placeholder card.
  const parts = item.summary ?? item.summary_text ?? item.text;
  return (Array.isArray(parts) ? parts : [parts]).map(text);
}

export function reasoningText(item) {
  return reasoningParts(item).filter(Boolean).join("\n\n").trim();
}

export function reasoningHeading(body) {
  const line = String(body ?? "").split("\n").find((line) => line.trim()) ?? "";
  return compact(line.replace(/^\s*#{1,6}\s*/, "").replace(/\*\*|__|`/g, ""), 140);
}

export function activityStatus(value, exitCode = null) {
  const status = String(value ?? "").replaceAll("_", "").toLowerCase();
  if (["failed", "error", "失败"].includes(status) || (typeof exitCode === "number" && exitCode !== 0)) return "failed";
  if (["declined", "denied", "拒绝"].includes(status)) return "declined";
  if (["interrupted", "cancelled", "canceled", "aborted", "中断"].includes(status)) return "interrupted";
  if (["inprogress", "running", "运行", "运行中", "等待处理"].includes(status)) return "running";
  return "completed";
}

export function formatActivityDuration(milliseconds) {
  if (!Number.isFinite(milliseconds) || milliseconds < 0) return "";
  if (milliseconds < 1000) return `${Math.round(milliseconds)} ms`;
  const seconds = milliseconds / 1000;
  return seconds < 60 ? `${Number(seconds.toFixed(1))} 秒` : `${Math.floor(seconds / 60)} 分 ${Math.floor(seconds % 60)} 秒`;
}

function durationMs(item) {
  const value = item.durationMs ?? item.duration_ms;
  if (Number.isFinite(value) && value >= 0) return value;
  // serde's std::time::Duration wire representation in canonical rollout data.
  const duration = item.duration;
  if (duration && Number.isFinite(duration.secs) && Number.isFinite(duration.nanos)) {
    return duration.secs * 1000 + duration.nanos / 1e6;
  }
  return null;
}

function commandActions(item, command) {
  const actions = item.commandActions ?? item.command_actions ?? item.parsed_cmd;
  if (!Array.isArray(actions) || !actions.length) return [{ kind: "run", label: compact(command) || "命令" }];
  return actions.map((value) => {
    const action = value && typeof value === "object" ? value : {};
    const type = itemType(action);
    if (type === "read") return { kind: "read", label: compact(action.path || action.name || "文件") };
    if (type === "listfiles") return { kind: "list", label: compact(action.path || item.cwd || "目录") };
    if (type === "search") return {
      kind: "search", label: [action.query ? `“${compact(action.query)}”` : "内容", action.path ? `（${compact(action.path)}）` : ""].join(""),
    };
    return { kind: "run", label: compact(action.command ?? action.cmd ?? command) || "命令" };
  });
}

function diffStats(diff) {
  let added = 0;
  let removed = 0;
  let inHunk = false;
  for (const line of diff.split("\n")) {
    if (line.startsWith("@@")) { inHunk = true; continue; }
    if (!inHunk && /^(---|\+\+\+) /.test(line)) continue;
    if (line.startsWith("+")) added += 1;
    if (line.startsWith("-")) removed += 1;
  }
  return { added, removed };
}

function contentLines(content) {
  return content ? content.split("\n").length - (content.endsWith("\n") ? 1 : 0) : 0;
}

function fileChanges(changes) {
  const entries = Array.isArray(changes) ? changes : Object.entries(changes ?? {}).map(([path, change]) => ({ ...change, path }));
  return entries.map((change) => {
    const kind = itemType({ type: change.kind?.type ?? change.kind ?? change.type });
    const action = { kind: ({ add: "create", delete: "delete" })[kind] ?? "edit", label: compact(change.path || "文件") };
    const move = change.kind?.movePath ?? change.movePath ?? change.move_path;
    if (move) action.label += ` → ${compact(move)}`;
    // Official FileUpdateChange.diff is full file content for add/delete,
    // and a unified diff only for updates (item_builders::format_file_change_diff).
    const wholeFile = ["add", "delete"].includes(kind);
    const diff = wholeFile ? undefined : change.diff ?? change.unified_diff;
    const content = change.content ?? (wholeFile ? change.diff : undefined);
    if (typeof diff === "string") Object.assign(action, diffStats(diff));
    else if (typeof content === "string") Object.assign(action, {
      added: kind === "add" ? contentLines(content) : 0,
      removed: kind === "delete" ? contentLines(content) : 0,
    });
    return { action, detail: [change.path, move ? `→ ${move}` : "", diff ?? content].filter((value) => value != null && value !== "").join("\n") };
  });
}

export function toolItemView(item) {
  if (!item || typeof item !== "object") return null;
  const type = itemType(item);
  const exitCode = item.exitCode ?? item.exit_code ?? null;
  const activity = { status: activityStatus(item.status, exitCode), durationMs: durationMs(item), exitCode, actions: [] };
  let title;
  let body;
  if (type === "imageview") {
    const path = imagePath(item.path);
    return { kind: "tool", title: "查看图片", body: path, markdown: false,
      images: path ? [{ path }] : [], activity: { ...activity, actions: [{ kind: "tool", label: "查看图片" }] } };
  } else if (type === "commandexecution") {
    title = "Shell";
    const command = Array.isArray(item.command) ? item.command.join(" ") : text(item.command);
    activity.actions = commandActions(item, command);
    body = [command, item.cwd ? `cwd: ${item.cwd}` : "",
      item.aggregatedOutput ?? item.aggregated_output ?? item.formatted_output ?? item.output ?? text([item.stdout, item.stderr]),
      exitCode == null ? "" : `exit code: ${exitCode}`].filter((value) => value !== "" && value != null).join("\n\n");
  } else if (type === "filechange") {
    title = "文件修改";
    const changes = fileChanges(item.changes);
    activity.actions = changes.map((change) => change.action);
    if (!activity.actions.length) activity.actions.push({ kind: "edit", label: "文件" });
    body = [changes.map((change) => change.detail).join("\n\n"), text([item.stdout, item.stderr])].filter(Boolean).join("\n\n");
  } else if (["mcptoolcall", "dynamictoolcall", "toolcall", "collabagenttoolcall", "collabtoolcall"].includes(type)) {
    const name = item.tool ?? item.name ?? type;
    const namespace = item.server ?? item.namespace;
    title = namespace ? `${namespace} · ${name}` : name;
    activity.actions = [{ kind: "tool", label: compact(title) }];
    body = JSON.stringify({ arguments: item.arguments ?? item.input ?? item.prompt,
      result: item.result ?? item.contentItems ?? item.content_items ?? item.output,
      error: item.error, success: item.success }, imageJsonReplacer, 2);
    if (item.success === false || item.error) activity.status = "failed";
  } else return null;
  const images = outputImages(item.result ?? item.contentItems ?? item.content_items ?? item.output);
  return { kind: "tool", title, body, activity, markdown: false, ...(images.length ? { images } : {}) };
}

// Legacy model-facing calls may have no materialized counterpart. Only use
// explicit arguments for known shell tools; do not guess what code-mode JS did.
export function responseToolView(payload, status = "completed") {
  let input = payload.arguments ?? payload.input ?? payload.command;
  if (typeof input === "string") {
    try { input = JSON.parse(input); } catch { /* raw custom tool input */ }
  }
  const name = String(payload.name ?? payload.tool ?? "").split(".").at(-1);
  if (["exec_command", "shell_command", "shell", "local_shell"].includes(name) || payload.type === "local_shell_call") {
    return toolItemView({ type: "commandExecution", status, command: input?.cmd ?? input?.command ?? payload.command,
      cwd: input?.workdir ?? input?.cwd, commandActions: payload.commandActions ?? payload.parsed_cmd });
  }
  if (name === "apply_patch" && typeof input === "string") {
    const changes = [];
    for (const line of input.split("\n")) {
      const match = line.match(/^\*\*\* (Add|Update|Delete) File: (.+)$/);
      if (match) changes.push({ path: match[2], kind: match[1].toLowerCase(), diff: "" });
      else if (changes.length && !line.startsWith("***")) changes.at(-1).diff += `${line}\n`;
    }
    if (changes.length) return toolItemView({ type: "fileChange", status, changes: changes.map((change) => {
      if (change.kind === "add") return { path: change.path, kind: "add",
        content: change.diff.split("\n").filter((line) => line.startsWith("+")).map((line) => line.slice(1)).join("\n") };
      if (change.kind === "delete") return { path: change.path, kind: "delete" };
      return change;
    }) });
  }
  return null;
}

const verbs = { read: "读取", search: "搜索", list: "列出", run: "执行", create: "创建", edit: "修改", delete: "删除", tool: "调用" };

export function activitySummary(activity) {
  if (!activity?.actions?.length) return "";
  const prefix = activity.status === "running" ? "正在" : activity.status === "completed" ? "已" : "";
  const suffix = { failed: " · 失败", declined: " · 已拒绝", interrupted: " · 已中断" }[activity.status] ?? "";
  const visible = activity.actions.slice(0, 3).map((action) => {
    const stats = Number.isFinite(action.added) ? ` +${action.added} −${action.removed}` : "";
    return `${prefix}${verbs[action.kind] ?? "调用"} ${action.label}${stats}`;
  });
  if (activity.actions.length > 3) visible.push(`另 ${activity.actions.length - 3} 项`);
  return visible.join(" · ") + suffix;
}

export function summarizeActivities(activities) {
  const counts = new Map();
  for (const activity of activities) {
    for (const action of activity?.actions ?? []) {
      const label = action.kind === "tool" ? action.label : verbs[action.kind] ?? "工具";
      counts.set(label, (counts.get(label) ?? 0) + 1);
    }
  }
  return [...counts].map(([label, count]) => `${label} × ${count}`).join(" · ");
}

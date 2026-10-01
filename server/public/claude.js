// Native Claude events -> shared, ephemeral transcript items. No Codex rollouts.
const compact = (value, limit = 160) => {
  const line = String(value ?? "").replace(/\s+/g, " ").trim();
  return line.length > limit ? `${line.slice(0, limit)}…` : line;
};
const nativeImage = b => b?.type === "image" && b.source?.type === "base64" && /^image\/(png|jpeg|gif|webp)$/.test(b.source.media_type)
  ? `data:${b.source.media_type};base64,${b.source.data}` : null;

// Claude Code's built-in tools, summarized like Codex's parsed shell actions.
// Paths inside the session directory are shown relative to it.
function toolActions(name, input = {}, cwd = "") {
  const path = value => {
    const text = String(value || "");
    return compact(cwd && text.startsWith(`${cwd}/`) ? text.slice(cwd.length + 1) : text) || "文件";
  };
  const mcp = /^mcp__(.+?)__(.+)$/.exec(name);
  switch (name) {
    case "Bash": return [{ kind: "run", label: compact(input.command) || "命令" }];
    case "Read": return [{ kind: "read", label: path(input.file_path) }];
    case "Edit": case "MultiEdit": return [{ kind: "edit", label: path(input.file_path) }];
    case "NotebookEdit": return [{ kind: "edit", label: path(input.notebook_path) }];
    case "Write": return [{ kind: "create", label: path(input.file_path) }];
    case "Grep": return [{ kind: "search", label: `“${compact(input.pattern)}”${input.path ? `（${path(input.path)}）` : ""}` }];
    case "Glob": return [{ kind: "search", label: `“${compact(input.pattern)}”${input.path ? `（${path(input.path)}）` : ""}` }];
    case "LS": return [{ kind: "list", label: path(input.path) }];
    case "WebFetch": return [{ kind: "read", label: compact(input.url) || "网页" }];
    case "WebSearch": return [{ kind: "search", label: `“${compact(input.query)}”` }];
    case "Agent": case "Task": return [{ kind: "agent", label: compact(input.description || input.subagent_type) || "子 Agent" }];
    case "TodoWrite": return [{ kind: "tool", label: "待办列表" }];
    default: return [{ kind: "tool", label: mcp ? `${mcp[1]} · ${mcp[2]}` : name || "工具" }];
  }
}

function patchStats(result) {
  if (result?.type === "create" && typeof result.content === "string") {
    return { added: result.content ? result.content.split("\n").length - (result.content.endsWith("\n") ? 1 : 0) : 0, removed: 0 };
  }
  const patch = result?.structuredPatch;
  if (!Array.isArray(patch)) return null;
  const lines = patch.flatMap(hunk => hunk?.lines || []);
  return { added: lines.filter(line => line.startsWith("+")).length, removed: lines.filter(line => line.startsWith("-")).length };
}

const streamScope = row => JSON.stringify([row.turnId ?? null, row.payload.parent_tool_use_id ?? null]);

function pruneCompletedStreams(rows) {
  const active = new Map(), messages = new Map();
  for (const row of [...rows.values()].sort((a, b) => a.seq - b.seq)) {
    const e = row.payload, scope = streamScope(row);
    const id = e.type === "assistant" ? e.message?.id : e.type === "stream_event" && e.event?.type === "message_start" ? e.event.message?.id : null;
    if (id) {
      const key = JSON.stringify([scope, id]);
      if (!messages.has(key)) messages.set(key, { rows: [], saved: false, stopped: false });
      active.set(scope, messages.get(key));
    }
    const message = active.get(scope);
    if (!message) continue;
    if (e.type === "assistant" && id) message.saved = true;
    if (e.type !== "stream_event") continue;
    message.rows.push(row.seq);
    if (e.event?.type === "message_stop") { message.stopped = true; active.delete(scope); }
  }
  // SDK assistant events complete individual blocks (thinking, text, tools).
  // Keep message identity and deltas until the entire message has stopped.
  for (const message of messages.values()) {
    if (message.saved && message.stopped) for (const seq of message.rows) rows.delete(seq);
  }
}

export function claudeTrace(rows, { child = false, activeTurn = null } = {}) {
  const items = new Map(), questions = new Map(), streams = new Map(), steers = new Map(), endedTurns = new Set();
  let interrupted = false, cwd = "";
  const put = (key, kind, title, body, turnId, extra = {}) => items.set(key, { key, kind, title, body, turnId, ...extra });
  function blocks(message, key, role, turnId, timestamp, toolResult) {
    const timing = timestamp ? { completedAt: timestamp, timingScope: "recorded" } : {};
    const content = typeof message?.content === "string" ? [{ type: "text", text: message.content }] : message?.content || [];
    for (const [index, b] of content.entries()) {
      const k = `${key}:${index}`;
      if (b.type === "text") put(k, role, role === "user" ? "你" : "Claude", b.text, turnId, timing);
      else if (b.type === "thinking") { if (b.thinking?.trim()) put(k, "reasoning", "思考", b.thinking, turnId, timing); }
      else if (b.type === "tool_use") {
        const activity = { status: "running", durationMs: null, actions: toolActions(b.name, b.input, cwd) };
        put(`tool:${b.id}`, "tool", b.name, JSON.stringify(b.input, null, 2), turnId, { ...timing, activity, startedAt: timestamp });
      } else if (b.type === "tool_result") {
        const prior = items.get(`tool:${b.tool_use_id}`);
        const text = typeof b.content === "string" ? b.content : JSON.stringify((b.content || []).filter(v => v.type !== "image"), null, 2);
        const images = Array.isArray(b.content) ? b.content.map(nativeImage).filter(Boolean) : [];
        const actions = (prior?.activity?.actions || [{ kind: "tool", label: "工具结果" }]).map(action => ({ ...action }));
        const stats = patchStats(toolResult);
        if (stats && actions.length === 1 && ["edit", "create"].includes(actions[0].kind)) {
          Object.assign(actions[0], stats, toolResult.type === "update" ? { kind: "edit" } : {});
        }
        const started = Date.parse(prior?.startedAt), finished = Date.parse(timestamp);
        const activity = { status: b.is_error ? "failed" : "completed", durationMs: finished >= started ? finished - started : null, actions };
        // Screenshots and other tool output images stay with their tool call.
        put(`tool:${b.tool_use_id}`, "tool", prior?.title || "工具结果", [prior?.body, text].filter(Boolean).join("\n\n"), turnId,
          { ...timing, status: activity.status, activity, startedAt: prior?.startedAt, nativeImages: [...prior?.nativeImages || [], ...images] });
      } else if (nativeImage(b)) put(k, role, "图片", "", turnId, { ...timing, nativeImage: nativeImage(b) });
    }
  }
  function userInput(key, e, turnId, extra = {}) {
    put(key, "user", "你", [e.text, ...(e.attachments || []).map(f => `附件：${f.name || f.path}`)].filter(Boolean).join("\n"), turnId, extra);
    blocks({ content: (e.message?.content || []).filter(b => b.type === "image") }, `${key}:image`, "user", turnId, e.timestamp);
  }
  // A message added while Claude runs appears where the model read it. Until
  // then it waits below the running turn; a stop or a failed turn drops it unread.
  function placeSteer(id, steerState) {
    const steer = steers.get(id);
    if (!steer || steer.steerState) return;
    steer.steerState = steerState;
    userInput(`steer:${id}`, steer, steer.turnId, { steerState });
  }
  for (const row of rows) {
    const e = row.payload, turnId = row.turnId, key = `claude:${e.uuid || row.seq}`, scope = streamScope(row);
    if (!child && e.parent_tool_use_id && ["assistant", "user", "stream_event"].includes(e.type)) continue;
    if (e.type === "mira_user") {
      interrupted = false;
      userInput(key, e, turnId);
    } else if (e.type === "mira_steer") steers.set(e.steerId, { ...e, turnId });
    // Mira sends a rejected steer again as the next turn.
    else if (e.type === "mira_steer_rejected") steers.delete(e.steerId);
    else if (e.type === "command_lifecycle" && e.state !== "queued") placeSteer(e.command_uuid, e.state === "cancelled" ? "cancelled" : "inserted");
    else if (e.type === "assistant") {
      // A transcript reload omits stream events. A saved block still identifies
      // the message for later deltas from that message's remaining blocks.
      if (e.message?.id) streams.set(scope, e.message.id);
      for (const k of items.keys()) if (k.startsWith(`stream:${scope}:${e.message?.id}:`)) items.delete(k);
      blocks(e.message, key, "assistant", turnId, e.timestamp);
    } else if (e.type === "user" && (child || e.message?.content?.some?.(b => b.type === "tool_result"))) blocks(e.message, key, "user", turnId, e.timestamp, e.tool_use_result ?? e.toolUseResult);
    else if (e.type === "stream_event") {
      const raw = e.event;
      if (raw?.type === "message_start") streams.set(scope, raw.message.id);
      const stream = streams.get(scope);
      if (stream && raw?.type === "content_block_delta" && raw.delta?.type === "text_delta") {
        const k = `stream:${scope}:${stream}:${raw.index}`;
        put(k, "assistant", "Claude", (items.get(k)?.body || "") + raw.delta.text, turnId);
      }
      if (raw?.type === "message_stop") streams.delete(scope);
    } else if (e.type === "mira_question") {
      questions.set(e.questionId, { ...e, turnId });
      put(`question:${e.questionId}`, "question", "等待你的回答", (e.questions || []).map(q => q.question).join("\n\n"), turnId,
        { questionId: e.questionId, questionState: "pending" });
    } else if (e.type === "mira_answer") {
      questions.delete(e.questionId);
      put(`question:${e.questionId}`, "question", "已回答", Object.entries(e.answers || {}).map(([question, answer]) => `${question}\n${answer}`).join("\n\n"), turnId,
        { questionId: e.questionId, questionState: "answered" });
    } else if (e.type === "mira_interrupt_requested") { interrupted = true; endedTurns.add(turnId); }
    else if (e.type === "mira_completed") endedTurns.add(turnId);
    else if (e.type === "mira_error") put(key, "error", "运行错误", e.message, turnId);
    else if (e.type === "system" && e.subtype === "mirror_error") put(key, "error", "历史未完整保存", "部分原生记录未能保存到 Mira。", turnId);
    else if (e.type === "result") {
      for (const id of e.parent_tool_use_id ? [] : e.user_message_uuids || []) placeSteer(id, "inserted");
      if (e.is_error && !interrupted) put(key, "error", "Claude 返回错误", (e.errors || [e.subtype]).join("\n"), turnId);
      const last = [...items.values()].findLast(item => item.turnId === turnId && item.kind === "assistant" && item.body);
      if (last) Object.assign(last, { turnElapsedMs: e.duration_ms, turnCostEstimate: row.costEstimate, turnCompletedAt: row.completedAt ?? e.timestamp });
    } else if (e.type === "system" && e.subtype === "init" && e.cwd) cwd = e.cwd.replace(/\/+$/, "");
    // Sub-agent task events are not listed here: the Agent tool call shows its
    // result, and the sidebar opens the sub-agent's own conversation.
  }
  // A tool without a result is still running, or was cut off when its turn ended.
  for (const item of items.values()) {
    if (item.activity?.status === "running" && item.turnId !== activeTurn) item.activity = { ...item.activity, status: "interrupted" };
  }
  for (const [id, steer] of steers) placeSteer(id, steer.turnId === activeTurn ? "queued" : "cancelled");
  for (const [id, question] of questions) {
    if (question.turnId === activeTurn && !endedTurns.has(question.turnId)) continue;
    questions.delete(id);
    const item = items.get(`question:${id}`);
    Object.assign(item, { title: "问题已结束", questionState: "cancelled", body: `${item.body}\n\n本轮已结束，此问题不再等待回答。` });
  }
  return { trace: [...items.values()], questions };
}

export class ClaudeRuntime {
  constructor(api) { this.api = api; this.turnRequests = new Map(); this.steerRequests = new Map(); this.reset(); }
  reset(id = null) { this.epoch = (this.epoch || 0) + 1; this.id = id; this.rows = new Map(); this.cursor = 0; this.earliest = null; this.restored = false; }
  // A browser-cached copy of the rows read so far. The next latest-page read
  // keeps the cached older rows only when that page overlaps them.
  snapshot() { return { rows: [...this.rows.values()], cursor: this.cursor, earliest: this.earliest }; }
  restore(id, snapshot) {
    this.reset(id);
    for (const row of snapshot.rows) this.rows.set(row.seq, row);
    this.cursor = snapshot.cursor; this.earliest = snapshot.earliest; this.restored = true;
  }
  trace(thread, activeTurn) {
    return claudeTrace([...this.rows.values()].sort((a,b) => a.seq-b.seq), { child: !!thread.subpath, activeTurn });
  }
  call(path, body) { return this.api(`/v1/claude/${path}`, body === undefined ? {} : { method: "POST", body: JSON.stringify(body) }); }
  async prepare(nodeId, progress = () => {}) {
    let state = await this.call(`runtimes/${nodeId}/prepare`, {});
    const deadline = Date.now() + 660_000;
    while (state.status === "preparing") {
      progress("正在准备 Claude SDK…");
      if (Date.now() > deadline) throw new Error("Claude SDK 准备超时，请稍后重试");
      await new Promise(resolve => setTimeout(resolve, 1000));
      state = await this.call(`runtimes/${nodeId}/status`, {});
    }
    if (state.status !== "ready") throw new Error(state.error || "Claude 未就绪");
  }
  async models(node, progress) {
    await this.prepare(node.nodeId, progress);
    const info = await this.call(`runtimes/${node.nodeId}/describe`, { nodeAccountId: node.nodeAccountId || "" });
    const provider = node.reportedAppServer?.provider || {}, catalog = info.models || [];
    const efforts = m => m.supportsEffort === false ? [] : (m.supportedEffortLevels || []).map(reasoningEffort => ({ reasoningEffort }));
    const models = catalog.map(m => ({ model: m.value || m.id, displayName: m.displayName || m.value, description: m.description, supportedReasoningEfforts: efforts(m) }));
    // The SDK lists aliases; an account's full model ID borrows the capabilities of the alias resolving to it.
    if (provider.model && !models.some(m => m.model === provider.model)) {
      const base = id => String(id || "").replace(/\[1m\]$/i, "");
      const match = catalog.find(m => m.resolvedModel === provider.model) || catalog.find(m => base(m.resolvedModel) === base(provider.model));
      models.unshift({ model: provider.model, displayName: match?.displayName || provider.model, description: "账号默认模型", supportedReasoningEfforts: match ? efforts(match) : [] });
    }
    return { defaultModel: provider.model || models[0]?.model, configuredReasoningEffort: provider.effort || null, models };
  }
  async history(thread, { older = false, poll = false } = {}) {
    if (this.id !== thread.threadId) this.reset(thread.threadId);
    const epoch = this.epoch;
    const query = new URLSearchParams({ subpath: thread.subpath || "" });
    if (poll) query.set("after", this.cursor); else { query.set("view", "transcript"); query.set("before", older ? this.earliest || 0 : 0); }
    const result = await this.call(`sessions/${thread.sessionId}/${thread.subpath ? "history" : "events"}?${query}`);
    if (epoch !== this.epoch) return null;
    let earliest = result.data.length ? result.earliest : null;
    if (this.restored && !poll && !older) {
      // Stream events after this page are re-read by the next poll.
      this.restored = false; this.cursor = 0;
      const cachedMax = Math.max(0, ...[...this.rows.values()].filter(row => row.payload?.type !== "stream_event").map(row => row.seq));
      if (earliest !== null && earliest <= cachedMax) {
        for (const seq of [...this.rows.keys()]) if (seq >= earliest) this.rows.delete(seq);
        earliest = this.earliest;
      } else this.rows.clear();
    }
    for (const row of result.data) this.rows.set(row.seq, row);
    if (!older) this.cursor = Math.max(this.cursor, result.cursor || 0);
    if (!poll) this.earliest = earliest;
    pruneCompletedStreams(this.rows);
    return { ...this.trace(thread, result.session?.activeTurn), session: result.session,
      nextCursor: this.earliest, changed: result.data.length > 0, more: poll && result.data.length > 0 };
  }
  send(thread, body) { return this.#retained(this.turnRequests, "turns", thread, body); }
  steer(thread, body) { return this.#retained(this.steerRequests, "steer", thread, body); }
  // A retry reuses the request ID, so the Server replays its verdict instead of
  // repeating the input. Validation and conflicts may be corrected; network
  // failures retain the exact input.
  async #retained(requests, operation, thread, body) {
    const id = thread.sessionId;
    let request = requests.get(id);
    if (!request) { request = { ...body, requestId: crypto.randomUUID() }; requests.set(id, request); }
    try {
      const result = await this.call(`sessions/${id}/${operation}`, request);
      requests.delete(id);
      return result;
    } catch (error) {
      if ([400, 409].includes(error.status)) requests.delete(id);
      throw error;
    }
  }
}
// Older Servers do not expose the derived acknowledgement boundary.
export function claudeHistoryAcknowledgementRequired(session) {
  return session?.persistence === "incomplete" && session.historyAcknowledgementRequired !== false;
}

// Native Claude events -> shared, ephemeral transcript items. No Codex rollouts.
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

export function claudeTrace(rows, { child = false } = {}) {
  const items = new Map(), questions = new Map(), streams = new Map();
  let interrupted = false;
  const put = (key, kind, title, body, turnId, extra = {}) => items.set(key, { key, kind, title, body, turnId, ...extra });
  function blocks(message, key, role, turnId, timestamp) {
    const timing = timestamp ? { completedAt: timestamp, timingScope: "recorded" } : {};
    const content = typeof message?.content === "string" ? [{ type: "text", text: message.content }] : message?.content || [];
    for (const [index, b] of content.entries()) {
      const k = `${key}:${index}`;
      if (b.type === "text") put(k, role, role === "user" ? "你" : "Claude", b.text, turnId, timing);
      else if (b.type === "thinking") put(k, "reasoning", "思考", b.thinking, turnId, timing);
      else if (b.type === "tool_use") put(`tool:${b.id}`, "tool", b.name, JSON.stringify(b.input, null, 2), turnId, timing);
      else if (b.type === "tool_result") {
        const prior = items.get(`tool:${b.tool_use_id}`);
        const text = typeof b.content === "string" ? b.content : JSON.stringify((b.content || []).filter(v => v.type !== "image"), null, 2);
        put(`tool:${b.tool_use_id}`, "tool", prior?.title || "工具结果", [prior?.body, text].filter(Boolean).join("\n\n"), turnId, { ...timing, status: b.is_error ? "failed" : "completed" });
        if (Array.isArray(b.content)) blocks({ content: b.content.filter(v => v.type === "image") }, `result:${k}`, role, turnId, timestamp);
      } else if (b.type === "image" && b.source?.type === "base64" && /^image\/(png|jpeg|gif|webp)$/.test(b.source.media_type)) {
        put(k, role, "图片", "", turnId, { ...timing, nativeImage: `data:${b.source.media_type};base64,${b.source.data}` });
      }
    }
  }
  for (const row of rows) {
    const e = row.payload, turnId = row.turnId, key = `claude:${e.uuid || row.seq}`, scope = streamScope(row);
    if (!child && e.parent_tool_use_id && ["assistant", "user", "stream_event"].includes(e.type)) continue;
    if (e.type === "mira_user") {
      interrupted = false;
      put(key, "user", "你", [e.text, ...(e.attachments || []).map(f => `附件：${f.name || f.path}`)].filter(Boolean).join("\n"), turnId);
      blocks({ content: (e.message?.content || []).filter(b => b.type === "image") }, `${key}:image`, "user", turnId, e.timestamp);
    } else if (e.type === "assistant") {
      // A transcript reload omits stream events. A saved block still identifies
      // the message for later deltas from that message's remaining blocks.
      if (e.message?.id) streams.set(scope, e.message.id);
      for (const k of items.keys()) if (k.startsWith(`stream:${scope}:${e.message?.id}:`)) items.delete(k);
      blocks(e.message, key, "assistant", turnId, e.timestamp);
    } else if (e.type === "user" && (child || e.message?.content?.some?.(b => b.type === "tool_result"))) blocks(e.message, key, "user", turnId, e.timestamp);
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
      put(`question:${e.questionId}`, "assistant", "需要你的选择", "", turnId, { questionId: e.questionId });
    } else if (e.type === "mira_answer") {
      questions.delete(e.questionId);
      put(`question:${e.questionId}`, "assistant", "已回答", Object.values(e.answers || {}).join(" · "), turnId);
    } else if (e.type === "mira_interrupt_requested") interrupted = true;
    else if (e.type === "mira_error") put(key, "error", "运行错误", e.message, turnId);
    else if (e.type === "system" && e.subtype === "mirror_error") put(key, "error", "历史未完整保存", "部分原生记录未能保存到 Mira。", turnId);
    else if (e.type === "result") {
      if (e.is_error && !interrupted) put(key, "error", "Claude 返回错误", (e.errors || [e.subtype]).join("\n"), turnId);
      const last = [...items.values()].findLast(item => item.turnId === turnId && item.kind === "assistant" && item.body);
      if (last) Object.assign(last, { turnElapsedMs: e.duration_ms, turnCostEstimate: row.costEstimate, turnCompletedAt: row.completedAt ?? e.timestamp });
    } else if (e.type === "system" && ["task_started", "task_progress", "task_notification"].includes(e.subtype)) {
      put(`task:${e.task_id || row.seq}`, "tool", "子任务", e.description || e.summary || e.status || "", turnId);
    }
  }
  return { trace: [...items.values()], questions };
}

export class ClaudeRuntime {
  constructor(api) { this.api = api; this.turnRequests = new Map(); this.reset(); }
  reset(id = null) { this.epoch = (this.epoch || 0) + 1; this.id = id; this.rows = new Map(); this.cursor = 0; this.earliest = null; }
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
    for (const row of result.data) this.rows.set(row.seq, row);
    if (!older) this.cursor = Math.max(this.cursor, result.cursor || 0);
    if (!poll) this.earliest = result.data.length ? result.earliest : null;
    pruneCompletedStreams(this.rows);
    return { ...claudeTrace([...this.rows.values()].sort((a,b) => a.seq-b.seq), { child: !!thread.subpath }), session: result.session,
      nextCursor: this.earliest, changed: result.data.length > 0, more: poll && result.data.length > 0 };
  }
  async send(thread, body) {
    const id = thread.sessionId;
    let request = this.turnRequests.get(id);
    if (!request) { request = { ...body, requestId: crypto.randomUUID() }; this.turnRequests.set(id, request); }
    try {
      const result = await this.call(`sessions/${id}/turns`, request);
      this.turnRequests.delete(id);
      return result;
    } catch (error) {
      // Validation before reservation may be corrected. Network failures retain exact input.
      if ([400, 409].includes(error.status)) this.turnRequests.delete(id);
      throw error;
    }
  }
}

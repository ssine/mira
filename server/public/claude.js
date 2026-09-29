// Native Claude events -> shared, ephemeral transcript items. No Codex rollouts.
export function claudeTrace(rows, { child = false } = {}) {
  const items = new Map(), questions = new Map();
  let stream = "", interrupted = false;
  const put = (key, kind, title, body, turnId, extra = {}) => items.set(key, { key, kind, title, body, turnId, ...extra });
  function blocks(message, key, role, turnId) {
    const content = typeof message?.content === "string" ? [{ type: "text", text: message.content }] : message?.content || [];
    for (const [index, b] of content.entries()) {
      const k = `${key}:${index}`;
      if (b.type === "text") put(k, role, role === "user" ? "你" : "Claude", b.text, turnId);
      else if (b.type === "thinking") put(k, "reasoning", "思考", b.thinking, turnId);
      else if (b.type === "tool_use") put(`tool:${b.id}`, "tool", b.name, JSON.stringify(b.input, null, 2), turnId);
      else if (b.type === "tool_result") {
        const prior = items.get(`tool:${b.tool_use_id}`);
        const text = typeof b.content === "string" ? b.content : JSON.stringify((b.content || []).filter(v => v.type !== "image"), null, 2);
        put(`tool:${b.tool_use_id}`, "tool", prior?.title || "工具结果", [prior?.body, text].filter(Boolean).join("\n\n"), turnId, { status: b.is_error ? "failed" : "completed" });
        if (Array.isArray(b.content)) blocks({ content: b.content.filter(v => v.type === "image") }, `result:${k}`, role, turnId);
      } else if (b.type === "image" && b.source?.type === "base64" && /^image\/(png|jpeg|gif|webp)$/.test(b.source.media_type)) {
        put(k, role, "图片", "", turnId, { nativeImage: `data:${b.source.media_type};base64,${b.source.data}` });
      }
    }
  }
  for (const row of rows) {
    const e = row.payload, turnId = row.turnId, key = `claude:${e.uuid || row.seq}`;
    if (!child && e.parent_tool_use_id && ["assistant", "user", "stream_event"].includes(e.type)) continue;
    if (e.type === "mira_user") {
      interrupted = false;
      put(key, "user", "你", [e.text, ...(e.attachments || []).map(f => `附件：${f.name || f.path}`)].filter(Boolean).join("\n"), turnId);
      blocks({ content: (e.message?.content || []).filter(b => b.type === "image") }, `${key}:image`, "user", turnId);
    } else if (e.type === "assistant") {
      for (const k of items.keys()) if (k.startsWith(`stream:${e.message?.id}:`)) items.delete(k);
      blocks(e.message, key, "assistant", turnId);
    } else if (e.type === "user" && (child || e.message?.content?.some?.(b => b.type === "tool_result"))) blocks(e.message, key, "user", turnId);
    else if (e.type === "stream_event") {
      const raw = e.event;
      if (raw?.type === "message_start") stream = raw.message.id;
      if (raw?.type === "content_block_delta" && raw.delta?.type === "text_delta") {
        const k = `stream:${stream}:${raw.index}`;
        put(k, "assistant", "Claude", (items.get(k)?.body || "") + raw.delta.text, turnId);
      }
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
      if (last) Object.assign(last, { turnElapsedMs: e.duration_ms, turnCostEstimate: row.costEstimate, turnCompletedAt: row.completedAt });
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
    return { defaultModel: node.reportedAppServer?.provider?.model || info.models?.[0]?.value,
      models: (info.models || []).map(m => ({ model: m.value || m.id, displayName: m.displayName || m.value, description: m.description,
        supportedReasoningEfforts: m.supportsEffort === false ? [] : (m.supportedEffortLevels || []).map(reasoningEffort => ({ reasoningEffort })) })) };
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
    // Completed messages supersede transient delta history, keeping polling memory bounded.
    const completed = new Set([...this.rows.values()].filter(r => r.payload.type === "assistant").map(r => r.payload.message?.id));
    let message = "";
    for (const [seq, row] of [...this.rows].sort((a,b) => a[0]-b[0])) {
      if (row.payload.type !== "stream_event") continue;
      if (row.payload.event?.type === "message_start") message = row.payload.event.message.id;
      if (completed.has(message)) this.rows.delete(seq);
    }
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

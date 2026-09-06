// Ephemeral UI state, never part of the persisted transcript. Entries are tied
// to a submission and then a thread/turn so late events cannot affect another chat.
export class ReplyProgress {
  entries = new Set();

  begin(threadId, previousTurnId = null, now = Date.now()) {
    const entry = { threadId, previousTurnId, turnId: null, startedAt: now, phase: "正在准备…" };
    this.entries.add(entry);
    return entry;
  }

  update(entry, values) {
    if (this.entries.has(entry)) Object.assign(entry, values);
  }

  finish(entry) { this.entries.delete(entry); }

  clear(except = null) {
    for (const entry of this.entries) if (entry !== except) this.finish(entry);
  }

  observe(method, params = {}) {
    const turnId = params.turnId ?? params.turn?.id;
    const threadId = params.threadId;
    for (const entry of this.entries) {
      if (!threadId || entry.threadId !== threadId || !turnId || turnId === entry.previousTurnId) continue;
      if (entry.turnId && entry.turnId !== turnId) continue;
      // A late item from an older completed turn must not bind a new send.
      // Bind through turn/started or the explicit turn/start RPC result only.
      if (!entry.turnId && method !== "turn/started") continue;
      entry.turnId = turnId;
      if (method === "turn/started") entry.phase = "Codex 正在处理，等待回复…";
      const prose = method === "item/agentMessage/delta" ? params.delta
        : ["item/started", "item/completed"].includes(method) && params.item?.type === "agentMessage" ? params.item.text : "";
      if ((typeof prose === "string" && prose.trim()) || method === "turn/completed" ||
          (method === "error" && params.willRetry !== true)) this.finish(entry);
      else if (method === "error" && params.willRetry === true) entry.phase = "连接重试中，等待回复…";
    }
  }

  current(threadId) {
    return [...this.entries].reverse().find((entry) => entry.threadId === threadId) ?? null;
  }
}

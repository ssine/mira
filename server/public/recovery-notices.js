export function encryptedContextError(message) {
  if (typeof message !== "string" || message.length > 32768) return false;
  return /"code"\s*:\s*"(?:invalid_encrypted_content|unknown_reasoning_pool)"/.test(message)
    || /(?:^|\n)The encrypted content(?: for item \S+)? could not be verified\./.test(message)
    || /(?:^|409(?: Conflict)?: )(?:encrypted history has no known compatibility pool|history belongs to different compatibility pools)/.test(message);
}

export function recoveryNoticeLabel(notice, enabled = true) {
  const base = "加密内容不匹配";
  const retries = notice?.retryCount > 0 ? `已自动重试 ${notice.retryCount} 次` : "";
  if (notice?.reason?.includes("连续失败 20 次")) return `${base} · ${retries || "连续失败 20 次"} · 自动重试已停止（连续失败 20 次）`;
  if (notice?.status === "stopped" && !notice.reason?.startsWith("本次自动重试未完成")) {
    return `${base}${retries ? ` · ${retries}` : ""} · 自动恢复已停止`;
  }
  if (notice?.status === "unconfirmed" && !retries) return `${base} · 恢复状态待确认`;
  if (retries) return `${base} · ${retries}${notice.status === "dispatching" ? "，正在继续" : ""}`;
  if (notice?.status === "applying") return `${base} · 正在自动处理`;
  if (notice?.resolved) return `${base} · 已处理`;
  return enabled ? base : `${base} · 自动恢复已关闭`;
}

// Reads only the visible failure turns, in bounded batches. The Server counts
// actual retries; browser refreshes and inferred error counts never increment it.
export class RecoveryNotices {
  constructor(trace, { api, current }) {
    Object.assign(this, { trace, api, current });
    this.epoch = 0;
    this.cache = new Map();
    this.missing = new Map();
  }
  reset() {
    this.epoch++;
    clearTimeout(this.timer);
    this.cache.clear();
    this.missing.clear();
    this.inflight = null;
    this.checkedAt = 0;
  }
  decorate(card) {
    if (this.scope !== JSON.stringify(this.current())) return;
    const value = this.cache.get(card.dataset.turnId);
    if (!value) return;
    const label = recoveryNoticeLabel(value.notice, value.enabled);
    card.querySelector(".compaction-label").textContent = label;
    card.querySelector(".compaction-notice").textContent = label;
  }
  async refresh() {
    const { threadId, generation } = this.current();
    const scope = JSON.stringify(this.current());
    if (scope !== this.scope) { this.reset(); this.scope = scope; }
    if (!threadId || this.inflight || Date.now() - (this.checkedAt || 0) < 1500) return;
    const cards = [...this.trace.querySelectorAll(".trace-card.recovery[data-turn-id]")];
    const turns = [...new Set(cards.map(card => card.dataset.turnId).filter(Boolean))];
    if (!turns.length) return;
    const epoch = this.epoch, active = {};
    this.inflight = active;
    let pending = false;
    try {
      for (let offset = 0; offset < turns.length; offset += 64) {
        const query = new URLSearchParams({ storeId: "personal" });
        for (const turn of turns.slice(offset, offset + 64)) query.append("turnId", turn);
        const result = await this.api(`/v1/codex/threads/${encodeURIComponent(threadId)}/automatic-input-recovery?${query}`);
        if (epoch !== this.epoch || this.current().threadId !== threadId || (generation != null && result.generation !== generation)) return;
        for (const turn of turns.slice(offset, offset + 64)) {
          const notice = result.turns?.[turn];
          this.cache.set(turn, { notice, enabled: result.enabled });
          if (!notice) this.missing.set(turn, (this.missing.get(turn) || 0) + 1);
          pending ||= ["applying", "dispatching"].includes(notice?.status) || (notice && !notice.status && result.enabled && !notice.resolved);
          pending ||= !notice && (this.missing.get(turn) || 0) < 3;
        }
      }
      for (const card of this.trace.querySelectorAll(".trace-card.recovery[data-turn-id]")) this.decorate(card);
    } catch {
      // Keep the last known count and original expandable error on read failure.
    } finally {
      if (this.inflight === active) {
        this.inflight = null;
        this.checkedAt = Date.now();
        clearTimeout(this.timer);
        if (pending) this.timer = setTimeout(() => void this.refresh(), 2000);
      }
    }
  }
}

// The Server owns recovery and retries. This control only saves a preference.
export class AutomaticRecovery {
  constructor(input, status, { api, notice }) {
    Object.assign(this, { input, status, api, notice });
    this.epoch = 0;
    input.addEventListener("change", () => void this.save());
  }
  endpoint(threadId = this.threadId) {
    return `/v1/codex/threads/${encodeURIComponent(threadId)}/automatic-input-recovery?storeId=personal`;
  }
  async select(threadId) {
    this.threadId = threadId;
    const epoch = ++this.epoch;
    this.input.disabled = true;
    this.input.checked = true;
    this.status.textContent = threadId ? "正在读取设置…" : "新对话默认开启";
    if (!threadId) return;
    try {
      const result = await this.api(this.endpoint(threadId));
      if (epoch !== this.epoch) return;
      this.render(result);
      this.input.disabled = false;
    } catch (error) {
      if (epoch === this.epoch) this.status.textContent = error.message;
    }
  }
  render(result) {
    this.saved = result;
    this.input.checked = result.enabled;
    this.status.textContent = !result.enabled ? "已关闭自动恢复，遇到不兼容上下文时可手动处理。"
      : result.status === "stopped" ? result.reason
      : result.status === "applying" ? "正在自动处理不兼容上下文…"
      : result.status === "dispatching" ? "已处理上下文，正在后台重试。"
      : "遇到加密上下文不兼容时自动处理并后台重试，保留原始历史，不新增用户消息。同一份历史连续失败 20 次后停止；有新进展后重新计数。关闭页面后仍然生效。";
  }
  async save() {
    if (this.input.disabled || !this.saved || !this.threadId) return;
    const epoch = this.epoch, threadId = this.threadId, previous = this.saved;
    this.input.disabled = true;
    try {
      const result = await this.api(this.endpoint(threadId), { method: "PUT", body: JSON.stringify({ enabled: this.input.checked, generation: previous.generation }) });
      if (epoch === this.epoch) this.render(result);
    } catch (error) {
      if (epoch === this.epoch) { this.render(previous); this.notice(error.message); }
    } finally {
      if (epoch === this.epoch) this.input.disabled = false;
    }
  }
}

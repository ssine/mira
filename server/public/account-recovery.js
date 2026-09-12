// Compatibility is opt-in after a provider error; never retry a turn here.
export class AccountRecovery {
  constructor(root, { api, beforeApply, afterApply, notice, confirm = window.confirm.bind(window) }) {
    Object.assign(this, { root, api, beforeApply, afterApply, notice, confirm });
    this.context = null; this.epoch = 0;
    this.decisions = new Map();
  }
  select(threadId, bindingId) {
    this.context = threadId && bindingId ? { threadId, bindingId } : null;
    this.epoch++; this.root.replaceChildren(); this.root.hidden = true;
    if (this.context) void this.refresh();
  }
  endpoint(context = this.context) {
    return `/v1/codex/threads/${encodeURIComponent(context.threadId)}/input-recovery?storeId=personal&nodeAccountId=${encodeURIComponent(context.bindingId)}`;
  }
  observe(params) {
    if (this.context?.threadId === params.threadId && this.context?.bindingId === params.nodeAccountId) void this.refresh();
  }
  observeTurn(threadId) {
    if (this.context?.threadId === threadId && !this.root.hidden) void this.refresh();
  }
  async refresh() {
    const context = this.context, epoch = ++this.epoch;
    if (!context) return;
    let plan;
    try { plan = await this.api(this.endpoint(context)); }
    catch (error) {
      if (epoch === this.epoch && this.context === context && error.code === "no_context_failure") {
        this.root.replaceChildren(); this.root.hidden = true;
      }
      return;
    }
    if (epoch !== this.epoch || this.context !== context) return;
    this.root.replaceChildren(); this.root.hidden = false;
    const text = document.createElement("span");
    text.textContent = plan.recoverable
      ? "此账号无法解密历史上下文。可以确认兼容处理后继续，原始历史会保留。"
      : `此账号无法解密历史上下文：${plan.reason}。可切回原账号继续。`;
    this.root.append(text);
    if (!plan.recoverable) return;
    const button = document.createElement("button"); button.type = "button"; button.className = "secondary";
    button.textContent = "处理不兼容上下文…";
    button.addEventListener("click", async () => {
      if (this.applying || this.context !== context) return;
      const effect = plan.policy === "rebuildContext"
        ? "下一次恢复将从原始消息重建上下文，省略旧的加密推理和压缩检查点；输入可能变长。"
        : "下一次恢复将省略旧的加密推理，保留普通消息和工具记录。";
      if (!this.confirm(`${effect}\n\nPostgreSQL 中的原始历史不会删除。确认后不会自动重试失败的请求，需要你再次发送消息。\n\n继续？`)) return;
      this.applying = true; button.disabled = true;
      const decisionKey = JSON.stringify([context.threadId, context.bindingId, plan.failureId, plan.generation, plan.itemCount]);
      if (!this.decisions.has(decisionKey)) {
        if (this.decisions.size >= 32) this.decisions.delete(this.decisions.keys().next().value);
        this.decisions.set(decisionKey, crypto.randomUUID());
      }
      try {
        await this.beforeApply(context);
        const result = await this.api(this.endpoint(context), { method: "POST", body: JSON.stringify({
          confirm: true, decisionId: this.decisions.get(decisionKey), failureId: plan.failureId,
          generation: plan.generation, itemCount: plan.itemCount,
        }) });
        if (this.context !== context) return;
        this.epoch++;
        this.root.replaceChildren(); this.root.hidden = true;
        await this.afterApply({ ...context, retiredRuntimeId: result.retiredRuntimeId });
        this.notice("兼容处理已确认，原始历史已保留。请发送消息继续。");
      } catch (error) {
        this.notice(error.message);
        if (this.context === context) void this.refresh();
      } finally { this.applying = false; button.disabled = false; }
    });
    this.root.append(button);
  }
}

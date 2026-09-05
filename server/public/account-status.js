const weeklyMinutes = 7 * 24 * 60;

export function weeklyQuota(result) {
  const snapshot = result?.rateLimitsByLimitId?.codex ?? result?.rateLimits;
  const codex = snapshot && (!snapshot.limitId || snapshot.limitId === "codex") ? snapshot : null;
  const window = [codex?.primary, codex?.secondary].find(value => value?.windowDurationMins === weeklyMinutes);
  const remaining = Number.isFinite(window?.usedPercent) ? Math.max(0, Math.min(100, 100 - window.usedPercent)) : null;
  const seconds = window?.resetsAt;
  const resetsAt = Number.isFinite(seconds) && seconds > 0 && seconds < 8.64e12 ? seconds * 1000 : null;
  const count = result?.rateLimitResetCredits?.availableCount;
  return { remaining, resetsAt, resetCount: Number.isSafeInteger(count) && count >= 0 ? count : null };
}

export function resetTime(timestamp, now = Date.now()) {
  if (!Number.isFinite(timestamp)) return "未提供";
  if (timestamp <= now) return "等待更新";
  return new Intl.DateTimeFormat("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false }).format(timestamp);
}

// A read-only subscription: it never starts/resumes a thread or changes the Node's runtime.
export class AccountSidebar {
  constructor(root, { intervalMs = 5 * 60_000, timeoutMs = 15_000 } = {}) {
    this.root = root;
    this.intervalMs = intervalMs;
    this.timeoutMs = timeoutMs;
    this.cache = new Map();
    root.querySelector("[data-account-refresh]").addEventListener("click", () => void this.refresh());
    this.render();
  }

  select(node, active) {
    const cacheKey = node?.nodeId ? JSON.stringify([node.nodeId, node.reportedAppServer?.codexHome, node.reportedAppServer?.codexPath]) : null;
    const key = active ? JSON.stringify([cacheKey, node?.status, node?.reportedAppServer?.status]) : "";
    if (key === this.key) return;
    this.key = key;
    this.stop();
    this.cacheKey = active ? cacheKey : null;
    this.node = active ? node : null;
    const cached = active && this.cache.get(cacheKey);
    this.account = cached?.account ?? null;
    this.limits = cached?.limits ?? null;
    this.message = !active ? "" : !node ? "请选择运行节点" : node.status !== "online" ? "运行节点离线"
      : node.reportedAppServer?.status !== "running" ? "Codex 尚未启动" : cached ? cached.message : "正在读取账户…";
    this.available = Boolean(active && node?.status === "online" && node?.reportedAppServer?.status === "running");
    this.render();
    if (this.available) {
      if (cached && Date.now() - cached.updatedAt < this.intervalMs) this.schedule();
      else void this.refresh();
    }
  }

  clear() {
    this.stop();
    this.cache.clear();
    this.key = this.cacheKey = this.node = this.account = this.limits = null;
    this.available = false;
    this.message = "";
    this.render();
  }

  stop() {
    clearTimeout(this.timer);
    clearTimeout(this.notificationTimer);
    this.session?.close();
    this.session = null;
    this.operation = null;
    this.refreshAgain = false;
    this.revision = (this.revision ?? 0) + 1;
  }

  connect() {
    if (this.session) return this.session;
    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    const socket = new WebSocket(`${scheme}//${location.host}/v1/nodes/${this.node.nodeId}/app-server?storeId=personal`, ["mira-client-v1"]);
    const pending = new Map();
    let id = 0, rejectOpen;
    const session = { socket, close: () => {
      clearTimeout(openTimer);
      rejectOpen?.(new Error("closed"));
      for (const request of pending.values()) request.reject(new Error("closed"));
      pending.clear();
      socket.close();
    }, call: (method, params) => new Promise((resolve, reject) => {
      if (socket.readyState !== WebSocket.OPEN) { reject(new Error("offline")); return; }
      const requestId = ++id;
      const finish = (callback, value) => { clearTimeout(timer); pending.delete(requestId); callback(value); };
      const timer = setTimeout(() => finish(reject, new Error("timeout")), this.timeoutMs);
      pending.set(requestId, { resolve: value => finish(resolve, value), reject: error => finish(reject, error) });
      socket.send(JSON.stringify({ id: requestId, method, ...(params ? { params } : {}) }));
    }) };
    const opened = new Promise((resolve, reject) => {
      rejectOpen = reject;
      socket.addEventListener("open", () => { clearTimeout(openTimer); resolve(); }, { once: true });
    });
    const openTimer = setTimeout(() => rejectOpen(new Error("timeout")), this.timeoutMs);
    session.ready = opened.then(async () => {
      await session.call("initialize", { clientInfo: { name: "mira_web_account", version: "1" }, capabilities: { experimentalApi: true } });
      socket.send(JSON.stringify({ method: "initialized" }));
    });
    socket.addEventListener("message", event => {
      if (this.session !== session) return;
      let message;
      try { message = JSON.parse(event.data); } catch { return; }
      const request = pending.get(message.id);
      if (request) { message.error ? request.reject(new Error(message.error.message)) : request.resolve(message.result); return; }
      if (message.id !== undefined) return;
      if (["account/updated", "account/rateLimits/updated"].includes(message.method)) {
        // Identity changes invalidate the cached account immediately. Frequent
        // quota notifications share the normal TTL instead of causing more RPCs.
        if (message.method === "account/updated") {
          this.revision++;
          this.cache.delete(this.cacheKey);
          this.account = null;
          this.limits = null;
          this.message = "账户信息已变更，正在更新…";
          this.render();
          clearTimeout(this.notificationTimer);
          this.notificationTimer = setTimeout(() => void this.refresh(), 300);
        } else {
          this.schedule();
        }
      }
    });
    const disconnected = () => {
      session.close();
      if (this.session !== session) return;
      this.session = null;
      this.revision++;
      this.message = this.account ? "连接已断开，显示上次结果" : "账户暂不可用，请稍后刷新";
      this.render();
      this.schedule(this.intervalMs);
    };
    socket.addEventListener("close", disconnected, { once: true });
    socket.addEventListener("error", disconnected, { once: true });
    this.session = session;
    return session;
  }

  schedule(retryDelay) {
    clearTimeout(this.timer);
    if (!this.available || this.operation) return;
    const cached = this.cache.get(this.cacheKey);
    const delay = retryDelay ?? (cached ? Math.max(0, cached.updatedAt + this.intervalMs - Date.now()) : this.intervalMs);
    if (this.available) this.timer = setTimeout(() => void this.refresh(), delay);
  }

  async refresh() {
    if (!this.available) return;
    if (this.operation) { this.refreshAgain = true; return; }
    clearTimeout(this.timer);
    const revision = this.revision;
    const session = this.connect();
    const operation = (async () => {
      await session.ready;
      const account = await session.call("account/read", { refreshToken: false });
      if (this.session !== session || this.revision !== revision) return;
      const previousEmail = this.account?.email;
      const previousType = this.account?.type;
      this.account = account.account ?? null;
      if (previousEmail !== this.account?.email || previousType !== this.account?.type) this.cache.delete(this.cacheKey);
      if (previousEmail !== this.account?.email || this.account?.type !== "chatgpt") this.limits = null;
      this.message = !this.account ? "此节点的 Codex 尚未登录" : this.account.type !== "chatgpt" ? "此登录方式不提供套餐额度" : "";
      this.render();
      if (this.account?.type !== "chatgpt") return;
      const limits = await session.call("account/rateLimits/read");
      if (this.session !== session || this.revision !== revision) return;
      this.limits = limits;
      this.message = "";
    })();
    this.operation = operation;
    this.render();
    try { await operation; }
    catch {
      if (this.session !== session || this.revision !== revision) return;
      this.message = this.account ? (this.limits ? "额度更新失败，显示上次结果" : "套餐额度暂不可用，请稍后刷新") : "账户暂不可用，请稍后刷新";
      if (session.socket.readyState !== WebSocket.OPEN) { session.close(); this.session = null; }
    } finally {
      if (this.operation === operation) {
        this.operation = null;
        if (this.session === session && this.revision === revision) {
          this.cache.set(this.cacheKey, { account: this.account, limits: this.limits, message: this.message, updatedAt: Date.now() });
        }
        this.render();
        this.schedule(this.session === session && this.revision === revision ? undefined : this.intervalMs);
        if (this.refreshAgain || this.revision !== revision && this.session === session) {
          this.refreshAgain = false;
          clearTimeout(this.notificationTimer);
          this.notificationTimer = setTimeout(() => void this.refresh(), 300);
        }
      }
    }
  }

  render() {
    const find = selector => this.root.querySelector(selector);
    const { remaining, resetsAt, resetCount } = weeklyQuota(this.limits);
    const email = this.account?.email || (this.account?.type === "apiKey" ? "API Key 登录" : "Codex 账户");
    find("[data-account-email]").textContent = email;
    find("[data-account-email]").title = email;
    const mode = this.node?.nodeMode === "wsl" ? " · WSL" : this.node?.platform === "windows" ? " · Windows" : "";
    const nodeLabel = this.node ? `${this.node.hostname}${mode}` : "当前运行节点";
    find("[data-account-node]").textContent = nodeLabel;
    find("[data-account-node]").title = nodeLabel;
    find("[data-account-plan]").textContent = this.account?.planType?.toUpperCase() ?? "";
    find("[data-account-summary-email]").textContent = email;
    find("[data-account-summary-email]").title = email;
    find("[data-account-summary-plan]").textContent = this.account?.planType?.toUpperCase() ?? "";
    const summary = remaining === null ? "查看账户与额度" : `本周剩余 ${Number(remaining.toFixed(1))}%`;
    const summaryNode = find("[data-account-summary-remaining]");
    summaryNode.textContent = this.node && !this.available ? (this.node.status !== "online" ? "运行节点离线" : "Codex 尚未启动") : summary;
    summaryNode.title = this.message || summary;
    find("[data-account-remaining]").textContent = remaining === null ? "未提供" : `${Number(remaining.toFixed(1))}%`;
    const meter = find("meter");
    meter.classList.toggle("hidden", remaining === null);
    meter.value = remaining ?? 0;
    find("[data-account-reset]").textContent = resetTime(resetsAt);
    find("[data-account-reset]").title = resetsAt ? `${new Date(resetsAt).toLocaleString()}（本地时间）` : "按本地时区显示";
    find("[data-account-credits]").textContent = resetCount === null ? "未提供" : `${resetCount} 次`;
    find("dl").classList.toggle("hidden", this.account?.type !== "chatgpt");
    find("[data-account-status]").textContent = this.message ?? "";
    find("[data-account-status]").classList.toggle("hidden", !this.message);
    find("[data-account-refresh]").disabled = !this.available || Boolean(this.operation);
    const cached = this.cache.get(this.cacheKey);
    const minutes = cached ? Math.max(0, Math.floor((Date.now() - cached.updatedAt) / 60_000)) : null;
    find("[data-account-updated]").textContent = `${minutes === null ? "" : minutes === 0 ? "刚刚更新 · " : `${minutes} 分钟前更新 · `}每 5 分钟刷新`;
    this.root.setAttribute("aria-busy", String(Boolean(this.operation)));
  }
}

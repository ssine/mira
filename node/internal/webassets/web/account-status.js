import { accountQuery, accountNode, accountGroups } from "./codex-accounts.js";
import { weeklyQuota } from "./account-quota.js";
import { AccountHistory } from "./account-history.js";
import { AccountSpend } from "./account-spend.js";
export { weeklyQuota } from "./account-quota.js";

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
    this.summaries = new Map(); this.summaryJobs = new Map();
    this.history = new AccountHistory(root.querySelector("[data-account-history]"));
    this.spend = new AccountSpend(root.querySelector("[data-account-spend]"));
    root.querySelector("[data-account-refresh]").addEventListener("click", () => {
      void this.refresh();
      if (this.selectedName && !root.querySelector("[data-account-spend]").classList.contains("hidden")) {
        this.summaries.delete(this.selectedName); this.loadSummaries();
        if (this.spend.range !== "7d") this.spend.select({ nodeId: this.selectedName }, null, true, true);
      }
    });
    this.render();
  }

  setNodes(nodes, active, legacyNode) {
    this.groups = accountGroups(nodes);
    this.active = active;
    if (!active) this.stopSummaries();
    const names = new Set(this.groups.map(group => group.name));
    for (const name of this.summaries.keys()) if (!names.has(name)) this.summaries.delete(name);
    for (const [name, controller] of this.summaryJobs) if (!names.has(name)) { controller.abort(); this.summaryJobs.delete(name); }
    const overview = this.groups.length > 0;
    this.root.querySelector("#agentAccountToggle").classList.toggle("hidden", overview);
    this.root.querySelector("[data-account-list]").classList.toggle("hidden", !overview);
    if (!overview) { this.select(legacyNode, active); return; }
    if (!this.groups.some(group => group.name === this.selectedName)) this.selectedName = this.groups[0].name;
    this.selectGroup(this.selectedName);
    this.renderOverview();
  }

  selectGroup(name) {
    const group = this.groups?.find(group => group.name === name);
    if (!group) return;
    this.selectedName = name;
    const { node, account } = group.members[0];
    this.select(accountNode(node, account.nodeAccountId), this.active);
    this.render();
  }

  groupQuota(group) {
    const account = group.members[0].account;
    return weeklyQuota(this.node?.nodeAccountId === account.nodeAccountId && this.limits ? this.limits : account.snapshot?.limits);
  }

  renderOverview() {
    const list = this.root.querySelector("[data-account-list]");
    if (!list || !this.groups?.length) return;
    const existing = new Map([...list.children].map(row => [row.dataset.accountName, row]));
    for (const group of this.groups) {
      let row = existing.get(group.name);
      if (!row) {
        row = document.createElement("button"); row.type = "button"; row.className = "sidebar-account-row";
        row.dataset.accountName = group.name; row.setAttribute("aria-haspopup", "dialog"); row.setAttribute("aria-controls", "agentAccountDetails");
        row.append(document.createElement("strong"), document.createElement("span"));
        list.append(row);
      }
      existing.delete(group.name);
      const { node } = group.members[0];
      const quota = this.groupQuota(group);
      row.firstChild.textContent = group.name;
      const summary = this.summaries.get(group.name), estimate = summary?.data?.estimate;
      const cost = Number.isFinite(estimate?.amount) ? `7 天 ${estimate.status === "partial" ? "≥ " : ""}$${estimate.amount.toFixed(2)}`
        : summary?.message || (summary ? "7 天暂无可估费用" : "7 天费用…");
      row.lastChild.textContent = quota.remaining === null ? cost : `剩余 ${Number(quota.remaining.toFixed(1))}%`;
      row.title = `${group.name} · ${[...new Set(group.members.map(value => value.node.displayName || value.node.hostname))].join("、")}${node.status !== "online" ? " · 上次记录" : ""}`;
      if (quota.remaining === null) row.title += " · 最近 7 天标准 API 价格估算，点击查看每日费用";
      if (quota.remaining === null && summary?.message) row.title += ` · ${summary.message}`;
      row.setAttribute("aria-label", `${group.name}，${row.lastChild.textContent}`);
    }
    for (const row of existing.values()) row.remove();
    this.loadSummaries();
  }

  stopSummaries() {
    for (const controller of this.summaryJobs.values()) controller.abort();
    this.summaryJobs.clear();
  }

  loadSummaries() {
    if (!this.active || navigator.onLine === false) return;
    for (const group of this.groups ?? []) {
      if (this.summaryJobs.size >= 2) break;
      if (this.groupQuota(group).remaining !== null || this.summaryJobs.has(group.name) ||
        (this.summaries.get(group.name)?.expiresAt ?? 0) > Date.now()) continue;
      const controller = new AbortController(); this.summaryJobs.set(group.name, controller);
      void this.loadSummary(group.name, controller);
    }
  }

  async loadSummary(name, controller) {
    const timer = setTimeout(() => controller.abort(), 35_000);
    try {
      const response = await fetch(this.spend.urlFor(name, "7d"), { signal: controller.signal });
      if (!response.ok) throw new Error("cost unavailable");
      const data = await response.json();
      if (this.summaryJobs.get(name) !== controller) return;
      const expiresAt = Date.now() + (Number.isFinite(data.estimate?.amount) ? this.intervalMs : 30_000);
      this.summaries.set(name, { data, expiresAt });
      this.spend.cacheSummary(name, data, expiresAt);
    } catch {
      if (this.summaryJobs.get(name) !== controller) return;
      this.summaries.set(name, { ...this.summaries.get(name), message: "费用暂不可用", expiresAt: Date.now() + 30_000 });
    } finally {
      clearTimeout(timer);
      if (this.summaryJobs.get(name) === controller) { this.summaryJobs.delete(name); this.renderOverview(); }
    }
  }

  select(node, active) {
    const cacheKey = node?.nodeId ? JSON.stringify([node.nodeId, node.nodeAccountId, node.accountRevision, node.reportedAppServer?.runtimeId, node.reportedAppServer?.codexHome, node.reportedAppServer?.codexPath, node.accountSnapshot]) : null;
    const key = active ? JSON.stringify([cacheKey, node?.status, node?.reportedAppServer?.status]) : "";
    if (key === this.key) return;
    this.key = key;
    this.stop();
    this.cacheKey = active ? cacheKey : null;
    this.node = active ? node : null;
    const cached = active && this.cache.get(cacheKey);
    this.account = cached?.account ?? (active ? node?.accountSnapshot?.account : null) ?? null;
    this.limits = cached?.limits ?? (active ? node?.accountSnapshot?.limits : null) ?? null;
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
    this.active = false;
    this.stop();
    this.cache.clear();
    this.stopSummaries(); this.summaries.clear();
    this.history.clear();
    this.spend.clear();
    this.groups = []; this.selectedName = null;
    this.root.querySelector("[data-account-list]").replaceChildren();
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
    const socket = new WebSocket(`${scheme}//${location.host}/v1/nodes/${this.node.nodeId}/app-server?storeId=personal${accountQuery(this.node.nodeAccountId)}`, ["mira-client-v1"]);
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
      const custom = this.node?.reportedAppServer?.provider?.id && this.node.reportedAppServer.provider.id !== "openai";
      this.account = custom ? { type: "providerConfig" } : account.account ?? null;
      if (previousEmail !== this.account?.email || previousType !== this.account?.type) this.cache.delete(this.cacheKey);
      if (previousEmail !== this.account?.email || this.account?.type !== "chatgpt") this.limits = null;
      this.message = !this.account ? "此账号尚未登录" : this.account.type === "providerConfig" ? "服务商配置已接管，未提供额度接口" : this.account.type !== "chatgpt" ? "此登录方式不提供套餐额度" : "";
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
          if (this.cache.size > 64) this.cache.delete(this.cache.keys().next().value);
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
    const email = this.node?.accountName || this.account?.email || (this.account?.type === "apiKey" ? "API Key 登录" : "Codex 账户");
    find("[data-account-email]").textContent = email;
    find("[data-account-email]").title = email;
    const mode = this.node?.nodeMode === "wsl" ? " · WSL" : this.node?.platform === "windows" ? " · Windows" : "";
    const group = this.groups?.find(group => group.name === this.selectedName);
    const nodeLabel = group ? [...new Set(group.members.map(value => value.node.displayName || value.node.hostname))].join(" · ") : this.node ? `${this.node.displayName?.trim() || this.node.hostname}${mode}` : "当前运行节点";
    find("[data-account-node]").textContent = nodeLabel;
    find("[data-account-node]").title = nodeLabel;
    find("[data-account-plan]").textContent = this.account?.planType?.toUpperCase() ?? "";
    find("[data-account-summary-email]").textContent = email;
    find("[data-account-summary-email]").title = email;
    find("[data-account-summary-plan]").textContent = this.account?.planType?.toUpperCase() ?? "";
    const remainingText = remaining === null ? "未提供" : `${Number(remaining.toFixed(1))}%`;
    const deadline = resetTime(resetsAt);
    const summary = remaining === null ? "查看账户与额度" : resetsAt === null ? `剩余 ${remainingText} · 时间未提供`
      : deadline === "等待更新" ? "已到重置时间，等待更新" : `${deadline} 前剩余 ${remainingText}`;
    const summaryNode = find("[data-account-summary-remaining]");
    summaryNode.textContent = this.node && !this.available ? (this.node.status !== "online" ? "运行节点离线" : "Codex 尚未启动") : summary;
    summaryNode.title = this.message || summary;
    const summaryCredits = find("[data-account-summary-credits]");
    summaryCredits.textContent = this.account?.type !== "chatgpt" ? "" : resetCount === null ? "重置未提供" : `重置 ${resetCount} 次`;
    summaryCredits.title = resetCount === null ? "剩余重置次数未提供" : `剩余重置次数：${resetCount} 次`;
    find("[data-account-remaining]").textContent = remainingText;
    const meter = find("meter");
    meter.classList.toggle("hidden", remaining === null);
    meter.value = remaining ?? 0;
    find("[data-account-reset]").textContent = deadline;
    find("[data-account-reset]").title = resetsAt ? `${new Date(resetsAt).toLocaleString()}（本地时间）` : "按本地时区显示";
    find("[data-account-credits]").textContent = resetCount === null ? "未提供" : `${resetCount} 次`;
    find("dl").classList.toggle("hidden", this.account?.type !== "chatgpt");
    find("[data-account-status]").textContent = this.message ?? "";
    find("[data-account-status]").classList.toggle("hidden", !this.message);
    find("[data-account-refresh]").disabled = (!this.available && !group) || Boolean(this.operation);
    this.root.setAttribute("aria-busy", String(Boolean(this.operation)));
    const spending = Boolean(group && remaining === null);
    find("[data-account-history]").classList.toggle("hidden", spending);
    find("[data-account-spend]").classList.toggle("hidden", !spending);
    this.history.select(this.node, this.account, Boolean(this.key) && !spending);
    this.spend.select(group ? { nodeId: group.name } : null, null, Boolean(this.key) && spending && find("#agentAccountDetails").matches(":popover-open"));
    this.renderOverview();
  }
}

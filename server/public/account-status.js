import { accountQuery, accountNode, accountGroups, recentAccountKeys } from "./codex-accounts.js";
import { weeklyQuota } from "./account-quota.js";
import { AccountHistory } from "./account-history.js";
import { AccountSpend, spendCacheLifetime, spendProjectionStatus } from "./account-spend.js";
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
    this.threads = []; this.expanded = false;
    this.history = new AccountHistory(root.querySelector("[data-account-history]"));
    this.spend = new AccountSpend(root.querySelector("[data-account-spend]"));
    root.querySelector("[data-account-refresh]").addEventListener("click", () => {
      void this.refresh();
      if (this.selectedName && !root.querySelector("[data-account-spend]").classList.contains("hidden")) {
        this.summaries.delete(this.selectedName); this.loadSummaries();
        if (this.spend.range !== "7d") this.spend.select({ nodeId: this.selectedName }, null, true, true);
      }
    });
    root.querySelector("[data-account-more]").addEventListener("click", () => { this.expanded = !this.expanded; this.renderOverview(); });
    this.render();
  }

  // Only the accounts of the two most recently active conversations stay unfolded.
  setThreads(threads) {
    this.threads = threads;
    if (this.groups?.length) this.renderOverview();
  }

  visibleGroups() {
    const groups = this.groups ?? [], recent = recentAccountKeys(groups, this.threads);
    const pinned = recent.length ? recent : groups.slice(0, 2).map(group => group.key);
    // Pinned rows keep the name order so switching conversations does not reshuffle them.
    const ordered = [...groups.filter(group => pinned.includes(group.key)), ...groups.filter(group => !pinned.includes(group.key))];
    return { ordered, folded: ordered.length - pinned.length, visible: this.expanded ? ordered : ordered.slice(0, pinned.length) };
  }

  setNodes(nodes, active, legacyNode, { summariesActive = active } = {}) {
    this.groups = accountGroups(nodes);
    this.active = active;
    // Server-side cost reads can finish while the mobile drawer is closed.
    // Live Node account subscriptions still follow the drawer's active state.
    this.summariesActive = summariesActive;
    if (!summariesActive) this.stopSummaries();
    const names = new Set(this.groups.map(group => group.key));
    for (const name of this.summaries.keys()) if (!names.has(name)) this.summaries.delete(name);
    for (const [name, controller] of this.summaryJobs) if (!names.has(name)) { controller.abort(); this.summaryJobs.delete(name); }
    const overview = this.groups.length > 0;
    const emptyCatalog = !overview && nodes.some(node => (node.codexAccounts ?? []).length > 0);
    this.root.classList.toggle("hidden", emptyCatalog);
    this.root.querySelector("#agentAccountToggle").classList.toggle("hidden", overview);
    this.root.querySelector("[data-account-list]").classList.toggle("hidden", !overview);
    if (!overview) {
      this.selectedName = null; this.root.querySelector("[data-account-more]").hidden = true;
      this.select(emptyCatalog ? null : legacyNode, active && !emptyCatalog);
      if (emptyCatalog && this.root.querySelector("#agentAccountDetails").matches(":popover-open")) this.root.querySelector("#agentAccountDetails").hidePopover();
      return;
    }
    if (!this.groups.some(group => group.key === this.selectedName)) this.selectedName = this.groups[0].key;
    this.selectGroup(this.selectedName);
    this.renderOverview();
  }

  selectGroup(name) {
    const group = this.groups?.find(group => group.key === name);
    if (!group) return;
    this.selectedName = name;
    const { node, account } = group.members[0];
    this.select(accountNode(node, account.nodeAccountId), this.active);
    this.render();
  }

  groupQuota(group) {
    if (group.engine === "claude") return weeklyQuota(null);
    const account = group.members[0].account;
    return weeklyQuota(this.node?.nodeAccountId === account.nodeAccountId && this.limits ? this.limits : account.snapshot?.limits);
  }

  renderOverview() {
    const list = this.root.querySelector("[data-account-list]");
    if (!list || !this.groups?.length) return;
    const existing = new Map([...list.children].map(row => [row.dataset.accountName, row]));
    const { ordered, visible, folded } = this.visibleGroups();
    for (const [index, group] of ordered.entries()) {
      let row = existing.get(group.key);
      if (!row) {
        row = document.createElement("button"); row.type = "button"; row.className = "sidebar-account-row";
        row.dataset.accountName = group.key; row.setAttribute("aria-haspopup", "dialog"); row.setAttribute("aria-controls", "agentAccountDetails");
        row.append(document.createElement("strong"), document.createElement("span"));
      }
      if (list.children[index] !== row) list.insertBefore(row, list.children[index] ?? null);
      row.hidden = !visible.includes(group);
      existing.delete(group.key);
      const { node } = group.members[0];
      const quota = this.groupQuota(group);
      row.firstChild.textContent = `${group.engine === "claude" ? "Claude" : "Codex"} · ${group.name}`;
      row.dataset.accountEngine = group.engine;
      const summary = this.summaries.get(group.key), estimate = summary?.data?.estimate;
      const cost = Number.isFinite(estimate?.amount) ? `7 天 ${estimate.status === "partial" ? "≥ " : ""}$${estimate.amount.toFixed(2)}`
        : spendProjectionStatus(summary?.data) || summary?.message || (summary ? "7 天暂无可估费用" : "7 天费用…");
      row.lastChild.textContent = quota.remaining === null ? cost : `剩余 ${Number(quota.remaining.toFixed(1))}%`;
      if (quota.remaining === null && Number.isFinite(estimate?.amount) && spendProjectionStatus(summary?.data)) row.lastChild.textContent += " · 汇总中";
      if (quota.remaining === null && summary?.data?.cache?.stale) row.lastChild.textContent += " · 上次统计";
      row.title = `${group.engine === "claude" ? "Claude" : "Codex"} · ${group.name} · ${[...new Set(group.members.map(value => value.node.displayName || value.node.hostname))].join("、")}${node.status !== "online" ? " · 上次记录" : ""}`;
      if (quota.remaining === null) row.title += " · 最近 7 天标准 API 价格估算，点击查看每日费用";
      if (quota.remaining === null && summary?.message) row.title += ` · ${summary.message}`;
      row.setAttribute("aria-label", `${group.engine === "claude" ? "Claude" : "Codex"} · ${group.name}，${row.lastChild.textContent}`);
    }
    for (const row of existing.values()) row.remove();
    const more = this.root.querySelector("[data-account-more]");
    more.hidden = folded === 0;
    more.textContent = this.expanded ? "收起账号" : `展开其余 ${folded} 个账号`;
    more.setAttribute("aria-expanded", String(this.expanded));
    this.loadSummaries();
  }

  stopSummaries() {
    clearTimeout(this.summaryTimer);
    for (const controller of this.summaryJobs.values()) controller.abort();
    this.summaryJobs.clear();
  }

  loadSummaries() {
    clearTimeout(this.summaryTimer);
    if (!this.summariesActive || navigator.onLine === false) return;
    const groups = this.groups?.length ? this.visibleGroups().visible : [];
    for (const group of groups) {
      if (this.summaryJobs.size >= 2) break;
      if (this.groupQuota(group).remaining !== null || this.summaryJobs.has(group.key) ||
        (this.summaries.get(group.key)?.expiresAt ?? 0) > Date.now()) continue;
      const controller = new AbortController(); this.summaryJobs.set(group.key, controller);
      void this.loadSummary(group.key, controller);
    }
    const next = groups.filter(group => this.groupQuota(group).remaining === null && !this.summaryJobs.has(group.key))
      .map(group => this.summaries.get(group.key)?.expiresAt).filter(expiresAt => expiresAt > Date.now());
    if (next.length) this.summaryTimer = setTimeout(() => this.loadSummaries(), Math.max(1, Math.min(...next) - Date.now()));
  }

  async loadSummary(name, controller) {
    const timer = setTimeout(() => controller.abort(), 65_000);
    try {
      const response = await fetch(this.spend.urlFor(name, "7d"), { signal: controller.signal });
      if (!response.ok) throw new Error("cost unavailable");
      const data = await response.json();
      if (this.summaryJobs.get(name) !== controller) return;
      const expiresAt = Date.now() + spendCacheLifetime(data, Number.isFinite(data.estimate?.amount) ? this.intervalMs : 30_000);
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
    if (key === this.key) { this.node = active ? node : null; this.renderMemory(); return; }
    this.key = key;
    this.stop();
    this.cacheKey = active ? cacheKey : null;
    this.node = active ? node : null;
    const cached = active && this.cache.get(cacheKey);
    this.account = cached?.account ?? (active ? node?.accountSnapshot?.account : null) ?? null;
    this.limits = cached?.limits ?? (active ? node?.accountSnapshot?.limits : null) ?? null;
    this.message = !active ? "" : !node ? "请选择运行节点" : node.status !== "online" ? "运行节点离线"
      : node.reportedAppServer?.status !== "running" ? "Codex 尚未启动" : cached ? cached.message : "正在读取账户…";
    this.available = Boolean(active && node?.engine !== "claude" && node?.status === "online" && node?.reportedAppServer?.status === "running");
    if (node?.engine === "claude") this.message = "Claude SDK 价格估算；网关实际扣费可能不同。";
    this.render();
    if (this.available) {
      if (cached && Date.now() - cached.updatedAt < this.intervalMs) this.schedule();
      else void this.refresh();
    }
  }

  clear() {
    this.active = this.summariesActive = false;
    this.stop();
    this.cache.clear();
    this.stopSummaries(); this.summaries.clear();
    this.history.clear();
    this.spend.clear();
    this.groups = []; this.selectedName = null;
    this.root.querySelector("[data-account-list]").replaceChildren();
    this.root.querySelector("[data-account-more]").hidden = true;
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

  renderMemory() {
    const target = this.root.querySelector("[data-account-memory]");
    if (!target) return;
    const group = this.groups?.find(group => group.key === this.selectedName);
    const members = group?.members ?? (this.node ? [{ node: this.node, account: { reportedAppServer: this.node.reportedAppServer } }] : []);
    const el = (tag, text, className) => {
      const value = document.createElement(tag); value.textContent = text; if (className) value.className = className; return value;
    };
    const bytes = value => value >= 1024 ** 3 ? `${(value / 1024 ** 3).toFixed(2)} GiB` : `${(value / 1024 ** 2).toFixed(0)} MiB`;
    const fragment = document.createDocumentFragment();
    if (group?.engine === "claude") { target.replaceChildren(); return; }
    fragment.append(el("strong", "Codex 内存 · 节点共享"));
    const seen = new Set();
    for (const { node, account } of members) {
      if (seen.has(node.nodeId)) continue;
      seen.add(node.nodeId);
      const row = el("div", "", "account-memory-row");
      row.append(el("span", node.displayName || node.hostname || "运行节点", "account-memory-node"));
      const memory = account.reportedAppServer?.memoryResidency;
      const valid = Number.isFinite(memory?.budgetBytes) && memory.budgetBytes > 0 &&
        Number.isFinite(memory?.residentBytes) && memory.residentBytes >= 0 && memory.status !== "unavailable";
      const stale = node.status !== "online" || account.reportedAppServer?.status !== "running" || !memory?.sampledAt || Date.now() - memory.sampledAt > 60_000;
      if (!valid) {
        row.append(el("span", node.status !== "online" ? "节点离线 · 内存数据不可用" : "尚无可用内存采样", "muted"));
        fragment.append(row); continue;
      }
      const used = memory.residentBytes, budget = memory.budgetBytes;
      const own = Number.isFinite(memory.processBytes) && memory.processBytes >= 0 ? Math.min(used, memory.processBytes) : null;
      const description = `${bytes(used)} / ${bytes(budget)} · ${Math.round(used / budget * 100)}%${stale ? " · 上次采样" : ""}`;
      row.append(el("span", description, "account-memory-value"));
      const bar = el("div", "", "account-memory-bar");
      bar.setAttribute("role", "img"); bar.setAttribute("aria-label", `已用内存 / 共享软预算：${description}`);
      bar.title = description;
      const segment = (amount, className, label) => {
        const part = el("span", "", className);
        part.style.width = `${amount / Math.max(used, budget) * 100}%`;
        part.title = `${label}：${bytes(amount)}`; bar.append(part);
      };
      if (own !== null) segment(own, "account-memory-own", "当前账号进程");
      segment(used - (own ?? 0), "account-memory-other", own === null ? "节点全部账号进程" : "其余账号进程");
      row.append(bar);
      const detail = own === null ? "节点总占用；当前节点版本未提供账号拆分" : `蓝色 · 当前账号 ${bytes(own)}　灰色 · 其余 ${bytes(used - own)}`;
      row.append(el("small", detail, "muted"));
      if (memory.sampledAt) row.title = `采样时间：${new Date(memory.sampledAt).toLocaleString()}`;
      fragment.append(row);
    }
    if (!members.length) fragment.append(el("p", "选择账号后显示内存采样", "muted"));
    fragment.append(el("p", "包含历史缓存、已加载会话和进程开销。预算由节点上的账号共享，可暂时超出；暂不支持按会话拆分。", "account-memory-note"));
    target.replaceChildren(fragment);
  }

  render() {
    const find = selector => this.root.querySelector(selector);
    const { remaining, resetsAt, resetCount } = weeklyQuota(this.limits);
    const email = this.node?.accountName || this.account?.email || (this.account?.type === "apiKey" ? "API Key 登录" : "Codex 账户");
    find("[data-account-email]").textContent = email;
    find("[data-account-email]").title = email;
    const mode = this.node?.nodeMode === "wsl" ? " · WSL" : this.node?.platform === "windows" ? " · Windows" : "";
    const group = this.groups?.find(group => group.key === this.selectedName);
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
    summaryNode.textContent = this.node?.engine === "claude" ? "Claude API · 查看估算费用" : this.node && !this.available ? (this.node.status !== "online" ? "运行节点离线" : "Codex 尚未启动") : summary;
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
    this.spend.select(group ? { nodeId: group.key } : null, null, Boolean(this.key) && spending && find("#agentAccountDetails").matches(":popover-open"));
    this.renderOverview();
    this.renderMemory();
  }
}

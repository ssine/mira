import { weeklyQuota } from "./account-quota.js";

export function accountNode(node, bindingId = "") {
  if (!node) return null;
  const account = (node.codexAccounts ?? []).find(value => bindingId ? value.nodeAccountId === bindingId : value.isDefault);
  if (!account) return bindingId ? null : node;
  return { ...node, nodeAccountId: account.nodeAccountId, accountName: account.name, accountRevision: account.credentialRevision ?? account.revision,
    desiredAppServer: account.desiredAppServer, reportedAppServer: account.reportedAppServer, accountSnapshot: account.snapshot };
}

// Names are the user's grouping key. A balance is an observation, never a sum
// of percentages from copies of an account on different Nodes.
export function accountGroups(nodes) {
  const groups = new Map();
  for (const node of nodes) {
    if (node.approvalStatus && node.approvalStatus !== "approved") continue;
    for (const account of node.codexAccounts ?? []) {
      const name = account.name?.trim();
      if (!name) continue;
      if (!groups.has(name)) groups.set(name, { name, members: [] });
      groups.get(name).members.push({ node, account });
    }
  }
  for (const group of groups.values()) {
    group.members.sort((a, b) => {
      const known = value => Number(weeklyQuota(value.account.snapshot?.limits).remaining !== null);
      return known(b) - known(a) || (Date.parse(b.account.observedAt) || 0) - (Date.parse(a.account.observedAt) || 0) ||
        Number(b.node.status === "online" && b.account.reportedAppServer?.status === "running") - Number(a.node.status === "online" && a.account.reportedAppServer?.status === "running") ||
        a.account.nodeAccountId.localeCompare(b.account.nodeAccountId);
    });
  }
  return [...groups.values()].sort((a, b) => a.name.localeCompare(b.name, "zh-CN"));
}

export function accountQuery(bindingId) { return bindingId ? `&nodeAccountId=${encodeURIComponent(bindingId)}` : ""; }

function el(tag, text, className) { const node = document.createElement(tag); if (text) node.textContent = text; if (className) node.className = className; return node; }
function lines(value) { return value.split(/[\n,]/u).map(item => item.trim()).filter(Boolean); }
const stateLabel = { running: "已启动", starting: "启动中", stopped: "已停止", unsupported: "不支持" };

export class CodexAccounts {
  constructor(root, { api, refreshNodes, notice }) {
    this.root = root; this.api = api; this.refreshNodes = refreshNodes; this.notice = notice; this.nodes = [];
    this.dialog = document.querySelector("#codexAccountDialog");
    this.form = document.querySelector("#codexAccountForm");
    this.filter = root.querySelector("[data-accounts-node]");
    root.querySelector("[data-accounts-refresh]").addEventListener("click", () => this.refresh().catch(error => notice(error.message)));
    root.querySelector("[data-accounts-new]").addEventListener("click", () => this.edit());
    this.filter.addEventListener("change", () => this.render());
    this.form.addEventListener("submit", event => { event.preventDefault(); void this.run(() => this.save()); });
    this.form.elements.mode.addEventListener("change", () => this.modeChanged());
    this.dialog.querySelector("[data-account-close]").addEventListener("click", () => this.dialog.close());
    this.dialog.addEventListener("close", () => { this.clearSecrets(); void this.cancelLogin(); });
    for (const action of ["start", "stop", "login", "logout", "cancel-login"]) {
      this.dialog.querySelector(`[data-account-${action}]`).addEventListener("click", () => void this.run(() => this.action(action)));
    }
  }

  setNodes(nodes) {
    this.nodes = nodes.filter(node => node.capabilities?.appServer);
    const previous = this.filter.value;
    this.filter.replaceChildren(new Option("全部节点", ""), ...this.nodes.map(node => new Option(node.displayName || node.hostname, node.nodeId)));
    this.filter.value = this.nodes.some(node => node.nodeId === previous) ? previous : "";
    this.render();
  }

  async refresh() { const response = await this.api("/v1/nodes"); this.setNodes(response.data ?? []); await this.refreshNodes?.(); }

  focusNode(nodeId) { this.filter.value = nodeId; this.render(); this.root.scrollIntoView({ block: "start" }); }

  render() {
    const list = this.root.querySelector("[data-accounts-list]"); list.replaceChildren();
    for (const node of this.nodes) {
      if (this.filter.value && this.filter.value !== node.nodeId) continue;
      for (const account of node.codexAccounts ?? []) {
        const row = el("button", "", "codex-account-row"); row.type = "button";
        const identity = el("span"); identity.append(el("strong", account.name), el("small", `${node.displayName || node.hostname} · ${account.reportedAppServer?.provider?.name || account.reportedAppServer?.provider?.id || account.provider}`));
        const snapshot = account.snapshot, quota = weeklyQuota(snapshot?.limits);
        const custom = account.reportedAppServer?.provider?.id && account.reportedAppServer.provider.id !== "openai";
        const usage = custom ? "服务商未提供额度接口" : quota.remaining === null ? "尚无额度数据" : `周额度剩余 ${Number(quota.remaining.toFixed(1))}%`;
        const state = node.status !== "online" ? "节点离线" : stateLabel[account.reportedAppServer?.status] || "等待节点确认";
        const status = el("span"); status.append(el("span", usage), el("small", snapshot?.account?.email || state));
        row.append(identity, status, el("span", state, "muted")); row.addEventListener("click", () => this.edit(node, account)); list.append(row);
      }
    }
    if (!list.childElementCount) list.append(el("p", "此节点还没有可管理的 Codex 账号。", "muted"));
  }

  edit(node = null, account = null) {
    this.current = account ? { node, account } : null; this.form.reset(); this.clearSecrets();
    this.dialog.querySelector("[data-account-error]").textContent = "";
    this.dialog.querySelector("[data-account-login-info]").replaceChildren();
    this.form.elements.node.replaceChildren(...this.nodes.filter(value => value.capabilities?.codexAccountsV1).map(value => new Option(value.displayName || value.hostname, value.nodeId)));
    this.form.elements.node.value = node?.nodeId || this.filter.value || this.form.elements.node.value;
    this.form.elements.node.disabled = Boolean(account);
    this.form.elements.name.value = account?.name || "";
    this.form.elements.mode.value = account?.isDefault || account?.desiredAppServer?.codexHome ? "adopt" : account?.authType === "providerConfig" ? "custom" : "chatgpt";
    this.form.elements.mode.disabled = Boolean(account);
    this.form.elements.home.value = account?.desiredAppServer?.codexHome || account?.reportedAppServer?.codexHome || "";
    this.form.elements.home.disabled = Boolean(account);
    this.form.elements.environmentFiles.value = (account?.desiredAppServer?.environmentFiles ?? []).join("\n");
    this.form.elements.inheritEnv.value = (account?.desiredAppServer?.inheritEnv ?? []).join(", ");
    this.form.elements.baseUrl.value = account?.reportedAppServer?.provider?.baseUrl || "";
    this.dialog.querySelector("[data-account-actions]").hidden = !account;
    this.dialog.querySelector("[data-account-title]").textContent = account ? "管理 Codex 账号" : "添加 Codex 账号";
    const provider = account?.reportedAppServer?.provider;
    this.dialog.querySelector("[data-account-profile]").textContent = account ? [stateLabel[account.reportedAppServer?.status] || "等待节点确认", provider?.id, provider?.credentialSource && `凭据来源：${provider.credentialSource}`, provider?.missingEnvironment?.length ? `缺少变量：${provider.missingEnvironment.join(", ")}` : "", account.reportedAppServer?.lastError].filter(Boolean).join(" · ") : "";
    this.modeChanged(); this.dialog.showModal();
  }

  modeChanged() {
    const mode = this.form.elements.mode.value;
    this.dialog.querySelector("[data-account-home]").hidden = mode !== "adopt";
    this.dialog.querySelector("[data-account-provider]").hidden = mode !== "custom";
    this.form.elements.home.required = mode === "adopt";
    this.form.elements.baseUrl.required = mode === "custom";
    const provider = this.current?.account.reportedAppServer?.provider?.id;
    const custom = mode === "custom" || (provider && provider !== "openai");
    for (const action of ["login", "logout", "cancel-login"]) this.dialog.querySelector(`[data-account-${action}]`).hidden = custom;
    this.dialog.querySelector("[data-account-clear-key]").hidden = mode !== "custom";
    this.dialog.querySelector("[data-account-key-hint]").textContent = mode === "custom" ? "密钥只写入所选节点。留空保留已有密钥。" : mode === "adopt" ? "现有 config.toml 与凭据继续由原文件管理；环境设置可在下方补充。" : "使用 ChatGPT 登录，或填写 OpenAI API Key 后点击「登录」。";
  }

  clearSecrets() { this.form.elements.apiKey.value = ""; this.form.elements.environment.value = ""; }

  async run(operation) {
    if (this.busy) return; this.busy = true;
    const buttons = [...this.dialog.querySelectorAll("button")]; for (const button of buttons) button.disabled = true;
    this.dialog.querySelector("[data-account-error]").textContent = "";
    try { await operation(); } catch (error) { this.clearSecrets(); this.dialog.querySelector("[data-account-error]").textContent = error.message; }
    finally { this.busy = false; for (const button of buttons) button.disabled = false; }
  }

  endpoint(action) { return `/v1/nodes/${this.current.node.nodeId}/codex-accounts/${this.current.account.nodeAccountId}${action ? `/${action}` : ""}`; }

  async waitForAccount(status) {
    const deadline = Date.now() + 40_000;
    while (Date.now() < deadline) {
      const node = await this.api(`/v1/nodes/${this.current.node.nodeId}`);
      const account = node.codexAccounts?.find(value => value.nodeAccountId === this.current.account.nodeAccountId);
      if (account) { this.current = { node, account }; if (account.reportedAppServer?.status === status) return; }
      await new Promise(resolve => setTimeout(resolve, 750));
    }
    throw new Error("节点尚未确认操作；稍后刷新账号状态再继续。");
  }

  async save() {
    const fields = this.form.elements, mode = fields.mode.value;
    const body = { environmentFiles: lines(fields.environmentFiles.value), inheritEnv: lines(fields.inheritEnv.value) };
    if (fields.environment.value.trim()) {
      try { body.environment = JSON.parse(fields.environment.value); } catch { throw new Error("环境变量请填写 JSON 对象，例如 {\"PROVIDER_KEY\":\"...\"}。"); }
    } else if (fields.clearEnvironment.checked) body.environment = {};
    if (mode === "custom") {
      const currentProvider = this.current?.account.reportedAppServer?.provider;
      if (!currentProvider?.baseUrl || currentProvider.baseUrl !== fields.baseUrl.value.trim()) body.provider = { id: currentProvider?.id || "custom", name: currentProvider?.name || fields.name.value.trim(), baseUrl: fields.baseUrl.value.trim() };
      if (fields.apiKey.value) body.apiKey = fields.apiKey.value;
      else if (fields.clearKey.checked) body.apiKey = "";
    }
    if (this.current) for (const key of ["environmentFiles", "inheritEnv"]) {
      if (JSON.stringify(body[key]) === JSON.stringify(this.current.account.desiredAppServer?.[key] ?? [])) delete body[key];
    }
    if (!this.current) {
      const nodeId = fields.node.value;
      const created = await this.api(`/v1/nodes/${nodeId}/codex-accounts`, { method: "POST", body: JSON.stringify({ name: fields.name.value.trim(), provider: mode === "chatgpt" ? "openai" : "custom", authType: mode === "chatgpt" ? "chatgpt" : "providerConfig", ...(mode === "adopt" ? { codexHome: fields.home.value.trim() } : {}) }) });
      this.current = { node: { nodeId }, account: created };
      fields.node.disabled = fields.mode.disabled = fields.home.disabled = true;
      this.dialog.querySelector("[data-account-actions]").hidden = false;
      await this.waitForAccount("stopped");
    } else {
      await this.api(this.endpoint(""), { method: "PATCH", body: JSON.stringify({ name: fields.name.value.trim() }) });
    }
    try { if (Object.keys(body).length) await this.api(this.endpoint("configure"), { method: "POST", body: JSON.stringify(body) }); }
    finally { this.clearSecrets(); }
    await this.refresh();
    this.dialog.querySelector("[data-account-actions]").hidden = false;
    this.dialog.querySelector("[data-account-profile]").textContent = "配置已保存。启动账号后可查看额度或登录。";
    this.form.elements.node.disabled = this.form.elements.mode.disabled = this.form.elements.home.disabled = true;
  }

  async action(action) {
    if (action === "cancel-login") { await this.cancelLogin(); return; }
    if (!this.current) return;
    if (action === "start" || action === "stop") {
      await this.api(`/v1/codex/runtimes/${this.current.node.nodeId}/${action}`, { method: "POST", body: JSON.stringify({ storeId: "personal", nodeAccountId: this.current.account.nodeAccountId }) });
      this.dialog.querySelector("[data-account-profile]").textContent = action === "start" ? "已请求启动；首次下载运行包可能需要几分钟。" : "已请求停止，执行中的任务结束后生效。";
      await this.refresh(); return;
    }
    if (action === "login") {
      const body = this.form.elements.apiKey.value ? { apiKey: this.form.elements.apiKey.value } : {};
      let result; try { result = await this.api(this.endpoint("login"), { method: "POST", body: JSON.stringify(body) }); } finally { this.clearSecrets(); }
      if (result.status === "completed") { this.notice("API Key 已保存到节点"); await this.refresh(); return; }
      this.login = { endpoint: this.endpoint("login-status"), cancel: this.endpoint("login-cancel"), id: result.loginSessionId };
      const info = this.dialog.querySelector("[data-account-login-info]"); info.replaceChildren(el("p", `在浏览器完成登录，验证码：${result.userCode || ""}`));
      try { const url = new URL(result.verificationUrl); if (url.protocol === "https:") { const link = el("a", "打开登录页面 ↗"); link.href = url.href; link.target = "_blank"; link.rel = "noopener noreferrer"; info.append(link); } } catch { /* Keep the code visible. */ }
      this.pollLogin(); return;
    }
    if (action === "logout") {
      if (!confirm("退出此 Codex 账号？对话历史会保留。")) return;
      await this.api(this.endpoint("logout"), { method: "POST", body: "{}" }); await this.refresh(); this.notice("账号已退出");
    }
  }

  pollLogin() {
    clearTimeout(this.loginTimer); const login = this.login; if (!login) return;
    this.loginTimer = setTimeout(async () => {
      try {
        const result = await this.api(`${login.endpoint}?sessionId=${encodeURIComponent(login.id)}`); if (this.login !== login) return;
        if (result.status === "pending") { this.pollLogin(); return; }
        this.login = null; this.dialog.querySelector("[data-account-login-info]").textContent = result.status === "completed" ? "登录完成" : "登录未完成，可重新发起。"; await this.refresh();
      } catch { if (this.login === login) this.pollLogin(); }
    }, 2000);
  }

  async cancelLogin() {
    clearTimeout(this.loginTimer); const login = this.login; this.login = null;
    if (login) { try { await this.api(login.cancel, { method: "POST", body: JSON.stringify({ sessionId: login.id }) }); } catch { this.notice("取消登录尚未确认，请刷新账号状态。"); } }
    this.dialog.querySelector("[data-account-login-info]").replaceChildren();
  }
}

// Persistent public port routes. Application credentials stay with the service.
export class SitesPanel {
  constructor(root, api, toast) {
    this.root = root; this.api = api; this.toast = toast; this.sites = new Map(); this.cursor = "";
    root.querySelector("form").addEventListener("submit", event => { event.preventDefault(); void this.create().catch(error => toast(error.message)); });
    root.querySelector("[data-sites-more]").addEventListener("click", () => void this.load(false).catch(error => toast(error.message)));
    root.querySelector("[data-sites-list]").addEventListener("click", event => {
      const button = event.target.closest("button[data-site-action]");
      if (button) void this.change(button).catch(error => toast(error.message));
    });
  }
  async refresh(nodes) {
    this.nodeNames = new Map(nodes.map(node => [node.nodeId, node.aliases?.[0] || node.nodeKey]));
    const select = this.root.querySelector("[name=nodeId]"); const selected = select.value;
    select.replaceChildren();
    for (const node of nodes.filter(node => node.approvalStatus === "approved" && node.capabilities?.portSitesV1)) {
      const option = document.createElement("option"); option.value = node.nodeId; option.textContent = node.aliases?.[0] || node.hostname || node.nodeKey; select.append(option);
    }
    if ([...select.options].some(option => option.value === selected)) select.value = selected;
    this.root.querySelector("[type=submit]").disabled = !select.options.length;
    await this.load(true);
  }
  async load(reset) {
    const result = await this.api(`/v1/sites?limit=50&after=${encodeURIComponent(reset ? "" : this.cursor)}`);
    if (reset) this.sites.clear();
    for (const site of result.data || []) this.sites.set(site.siteId, site);
    this.cursor = result.nextCursor || "";
    this.root.querySelector("[data-sites-more]").hidden = !result.hasMore;
    const rows = this.root.querySelector("[data-sites-list]"); rows.replaceChildren();
    for (const site of this.sites.values()) {
      const row = document.createElement("tr");
      const name = document.createElement("td"); const link = document.createElement("a"); link.textContent = site.name; link.href = site.url || "#"; link.target = "_blank"; link.rel = "noopener noreferrer"; name.append(link); row.append(name);
      for (const value of [site.url || "未配置站点域名", `${site.scheme} · ${this.nodeNames?.get(site.nodeId) || site.nodeId} : ${site.port}`, ({available:"可连接",disabled:"已停用",offline:"节点离线",revoked:"节点已撤销",upgrade_required:"需更新节点"})[site.phase] || site.phase]) {
        const cell = document.createElement("td"); cell.textContent = value; row.append(cell);
      }
      const actions = document.createElement("td");
      for (const [action, text] of [["toggle",site.enabled ? "停用" : "启用"],["delete","删除"]]) {
        const button = document.createElement("button"); button.type="button"; button.className="secondary"; button.dataset.siteAction=action; button.dataset.siteId=site.siteId; button.textContent=text; actions.append(button);
      }
      row.append(actions); rows.append(row);
    }
    if (!this.sites.size) { const row=document.createElement("tr"); const cell=document.createElement("td");cell.colSpan=5;cell.textContent="尚未注册站点";row.append(cell);rows.append(row); }
  }
  async create() {
    const form=this.root.querySelector("form");const values=new FormData(form);const button=form.querySelector("[type=submit]");button.disabled=true;
    try { await this.api("/v1/sites",{method:"POST",body:JSON.stringify({name:values.get("name"),nodeId:values.get("nodeId"),port:Number(values.get("port")),scheme:values.get("scheme")})}); await this.load(true); this.toast("站点已注册"); }
    finally {button.disabled=false;}
  }
  async change(button) {
    const site=this.sites.get(button.dataset.siteId); if (!site) return;
    if (button.dataset.siteAction === "delete" && !window.confirm(`删除 ${site.name}？域名将停止转发，名称仍会保留。`)) return;
    button.disabled=true;
    try { await this.api(`/v1/sites/${site.siteId}`,{method:button.dataset.siteAction === "delete" ? "DELETE" : "PATCH",body:JSON.stringify({expectedRevision:site.revision,...(button.dataset.siteAction === "toggle" ? {enabled:!site.enabled} : {})})}); await this.load(true); }
    finally {button.disabled=false;}
  }
}

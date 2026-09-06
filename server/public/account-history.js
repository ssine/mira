const ns = "http://www.w3.org/2000/svg";
const percent = value => `${Number(value.toFixed(1))}%`;
const timeLabel = value => new Date(value).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false });

export function quotaSegments(points, intervalMs) {
  const segments = [];
  let segment;
  for (const point of points) {
    if (!Number.isFinite(point.remaining)) { segment = null; continue; }
    if (!segment || point.at - segment.at(-1).at > intervalMs * 2.5) {
      segment = []; segments.push(segment);
    }
    segment.push(point);
  }
  return segments;
}

export class AccountHistory {
  constructor(root) {
    this.root = root;
    this.range = "7d";
    this.cache = new Map();
    root.querySelector("select").addEventListener("change", event => {
      this.range = event.target.value;
      this.select(this.node, this.account, true);
    });
    this.svg = root.querySelector("svg");
    this.svg.addEventListener("pointermove", event => {
      const rect = this.svg.getBoundingClientRect();
      this.inspect((event.clientX - rect.left) / rect.width * 600);
    });
    this.svg.addEventListener("pointerdown", event => {
      const rect = this.svg.getBoundingClientRect();
      this.inspect((event.clientX - rect.left) / rect.width * 600);
    });
    this.svg.addEventListener("pointerleave", event => {
      if (event.pointerType !== "touch") this.hidePoint();
    });
    this.svg.addEventListener("focus", () => {
      if (this.valid?.length) this.showPoint(this.valid[this.pointIndex ?? this.valid.length - 1]);
    });
    this.svg.addEventListener("blur", () => this.hidePoint());
    this.svg.addEventListener("keydown", event => {
      if (event.key === "Escape") this.hidePoint();
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key) || !this.valid?.length) return;
      event.preventDefault();
      this.pointIndex = event.key === "Home" ? 0 : event.key === "End" ? this.valid.length - 1 :
        Math.max(0, Math.min(this.valid.length - 1, (this.pointIndex ?? this.valid.length - 1) + (event.key === "ArrowLeft" ? -1 : 1)));
      this.showPoint(this.valid[this.pointIndex]);
    });
  }

  clear() {
    this.controller?.abort(); this.controller = null;
    this.key = this.node = this.account = this.data = null;
    this.cache.clear(); this.render();
  }

  select(node, account, active) {
    this.node = node; this.account = account;
    const key = active && node?.nodeId ? JSON.stringify([node.nodeId, node.reportedAppServer?.codexHome,
      node.reportedAppServer?.codexPath, account?.type, account?.email, this.range]) : null;
    if (this.key !== key) {
      this.controller?.abort(); this.controller = null; this.key = key;
      this.data = this.cache.get(key)?.data ?? null;
      this.message = "";
      this.render();
    }
    if (!key || this.controller || (this.cache.get(key)?.expiresAt ?? 0) > Date.now()) return;
    void this.load(key);
  }

  async load(key) {
    const controller = new AbortController();
    this.controller = controller;
    const timer = setTimeout(() => controller.abort(), 15_000);
    try {
      const response = await fetch(`/v1/nodes/${encodeURIComponent(this.node.nodeId)}/account-history?range=${this.range}`, { signal: controller.signal });
      if (!response.ok) throw new Error("history unavailable");
      const data = await response.json();
      if (this.key !== key) return;
      this.data = data; this.message = "";
      this.cache.set(key, { data, expiresAt: Date.now() + 5 * 60_000 });
      if (this.cache.size > 24) this.cache.delete(this.cache.keys().next().value);
    } catch {
      if (this.key !== key) return;
      this.message = "历史记录暂不可用";
      this.cache.set(key, { data: this.data, expiresAt: Date.now() + 30_000 });
    } finally {
      clearTimeout(timer);
      if (this.controller === controller) { this.controller = null; this.render(); }
    }
  }

  render() {
    const data = this.data;
    const sameAccount = this.account?.type === "chatgpt" && this.account.email === data?.account?.email;
    // An offline Node can still show its last recorded account, explicitly named.
    const offline = this.node?.status === "offline" && !this.account;
    const points = (sameAccount || offline) && Array.isArray(data?.points) ? data.points : [];
    const segments = quotaSegments(points, data?.intervalMs ?? 300_000);
    this.valid = segments.flat(); this.pointIndex = null;
    this.svg.replaceChildren();
    this.marker = this.tooltip = null;
    this.svg.setAttribute("aria-label", `额度历史，${this.valid.length} 次采样。左右方向键查看采样点。`);
    const empty = this.root.querySelector("[data-history-empty]");
    empty.hidden = this.valid.length > 0;
    empty.textContent = this.message || (!this.key ? "选择运行节点后查看额度历史" : this.account?.type && this.account.type !== "chatgpt" ? "此登录方式不提供套餐额度" :
      data?.account && this.account?.email && !sameAccount ? "账号已切换，等待首次采样" : "这个时间段暂无记录，采样后会显示在这里");
    this.svg.classList.toggle("hidden", !this.valid.length);
    this.root.querySelector("[data-history-note]").textContent = `${offline && data?.account?.email ? `${data.account.email} · ` : ""}每 5 分钟记录 · 空档表示未采集到额度`;
    if (!this.valid.length) return;
    const el = (tag, attrs, text) => {
      const node = document.createElementNS(ns, tag);
      for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, value);
      if (text !== undefined) node.textContent = text;
      this.svg.append(node); return node;
    };
    this.x = at => 42 + (at - data.from) / (data.to - data.from) * 540;
    this.y = remaining => 170 - remaining * 1.5;
    for (const value of [0, 50, 100]) {
      el("line", { x1: 42, x2: 582, y1: this.y(value), y2: this.y(value), class: "quota-grid" });
      el("text", { x: 34, y: this.y(value) + 4, "text-anchor": "end" }, `${value}%`);
    }
    for (const fraction of [0, .5, 1]) {
      const at = data.from + fraction * (data.to - data.from);
      const label = new Date(at).toLocaleString("zh-CN", this.range === "24h" ? { hour: "2-digit", minute: "2-digit", hour12: false } : { month: "2-digit", day: "2-digit" });
      el("text", { x: 42 + fraction * 540, y: 197, class: "quota-time-tick", "text-anchor": fraction === 0 ? "start" : fraction === 1 ? "end" : "middle" }, label);
    }
    for (const segment of segments) {
      const first = segment[0];
      if (segment.length === 1) el("circle", { cx: this.x(first.at), cy: this.y(first.remaining), r: 3, class: "quota-dot" });
      else el("path", { d: `M${this.x(first.at)},${this.y(first.remaining)}${segment.slice(1).map(point => `H${this.x(point.at)}V${this.y(point.remaining)}`).join("")}`, class: "quota-line" });
    }
    this.marker = el("circle", { r: 4, class: "quota-marker hidden" });
    this.tooltip = el("g", { class: "quota-tooltip hidden", "aria-hidden": "true" });
    this.tooltipTime = el("text", { x: 12, y: 23 });
    this.tooltipValue = el("text", { x: 12, y: 47, class: "quota-tooltip-value" });
    this.tooltip.append(el("rect", { width: 200, height: 60, rx: 5 }), this.tooltipTime, this.tooltipValue);
  }

  inspect(x) {
    if (!this.valid?.length) return;
    this.pointIndex = this.valid.reduce((best, point, index) => Math.abs(this.x(point.at) - x) < Math.abs(this.x(this.valid[best].at) - x) ? index : best, 0);
    this.showPoint(this.valid[this.pointIndex]);
  }

  showPoint(point) {
    const x = this.x(point.at), y = this.y(point.remaining);
    this.marker.setAttribute("cx", x); this.marker.setAttribute("cy", y);
    this.marker.classList.remove("hidden");
    // Flip the label at the plot edges so it stays beside the active point.
    const left = Math.max(8, x + 212 > 592 ? x - 212 : x + 12);
    const top = y - 72 < 8 ? y + 12 : y - 72;
    this.tooltip.setAttribute("transform", `translate(${left},${top})`);
    this.tooltipTime.textContent = timeLabel(point.at);
    this.tooltipValue.textContent = `剩余 ${percent(point.remaining)}`;
    this.tooltip.classList.remove("hidden");
    this.svg.setAttribute("aria-label", `额度历史，${this.valid.length} 次采样。${timeLabel(point.at)}，剩余 ${percent(point.remaining)}。左右方向键查看采样点。`);
  }

  hidePoint() {
    this.marker?.classList.add("hidden");
    this.tooltip?.classList.add("hidden");
  }
}

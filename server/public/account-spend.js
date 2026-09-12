import { AccountHistory } from "./account-history.js";

const money = value => `$${value.toFixed(2)}`;
const ns = "http://www.w3.org/2000/svg";

export function spendingSeries(data) {
  return (data?.days ?? []).map(day => {
    let amount = 0;
    const points = [{ at: day.at, remaining: 0, date: day.date, status: day.status }];
    for (const point of (data.points ?? []).filter(point => point.date === day.date).sort((a, b) => a.at - b.at)) {
      if (!Number.isFinite(point.amount)) continue;
      amount += point.amount;
      points.push({ ...point, remaining: amount });
    }
    points.push({ at: Math.min(day.end, data.to), remaining: amount, date: day.date, status: day.status });
    return { ...day, points };
  });
}

// Shares history cancellation/cache and keyboard/touch inspection with quotas.
export class AccountSpend extends AccountHistory {
  constructor(root) { super(root); this.timeoutMs = 35_000; }

  historyURL() {
    return this.urlFor(this.node.nodeId, this.range);
  }

  urlFor(name, range) {
    return `/v1/codex/accounts/cost-history?${new URLSearchParams({ name, range, timezone: Intl.DateTimeFormat().resolvedOptions().timeZone })}`;
  }

  cacheSummary(name, data, expiresAt) {
    const key = this.historyKey({ nodeId: name }, null, "7d");
    this.cache.set(key, { data, expiresAt });
    if (this.cache.size > 24) this.cache.delete(this.cache.keys().next().value);
    if (this.key === key) { this.data = data; this.message = ""; this.render(); }
  }

  render() {
    const data = this.data;
    const series = spendingSeries(data).filter(day => Number.isFinite(day.amount));
    this.valid = series.flatMap(day => day.points); this.pointIndex = null;
    this.svg.replaceChildren(); this.marker = this.tooltip = null;
    this.svg.classList.toggle("hidden", !this.valid.length);
    const empty = this.root.querySelector("[data-history-empty]");
    empty.hidden = this.valid.length > 0;
    empty.textContent = this.message || (this.controller ? "正在读取费用…" : "这个时间段暂无可归属、可定价的用量记录");
    this.root.querySelector("[data-history-legend]").hidden = !this.valid.length;
    this.svg.setAttribute("aria-label", "每日估算费用柱状图与日内累计曲线，左右方向键查看费用。");
    if (!this.valid.length) return;
    const el = (tag, attrs, text) => {
      const node = document.createElementNS(ns, tag);
      for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, value);
      if (text !== undefined) node.textContent = text;
      this.svg.append(node); return node;
    };
    const max = Math.max(.01, ...series.map(day => day.amount));
    const end = Math.max(data.from + 1, data.days.at(-1).end);
    this.x = at => 64 + (at - data.from) / (end - data.from) * 518;
    this.y = amount => 170 - amount / max * 145;
    for (const fraction of [0, .5, 1]) {
      const y = this.y(max * fraction);
      el("line", { x1: 64, x2: 582, y1: y, y2: y, class: "quota-grid" });
      el("text", { x: 57, y: y + 5, "text-anchor": "end" }, new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", notation: "compact", maximumFractionDigits: 2 }).format(max * fraction));
    }
    // All bars and curves use the same dollar scale and local calendar days.
    for (const day of series) {
      const width = this.x(day.end) - this.x(day.at), inset = Math.min(4, width * .15);
      const bar = el("rect", { x: this.x(day.at) + inset, y: this.y(day.amount), width: Math.max(1, width - 2 * inset), height: 170 - this.y(day.amount), class: "spend-bar" });
      const title = document.createElementNS(ns, "title");
      title.textContent = `${day.date} · ${money(day.amount)}${day.status === "partial" ? "（部分估算）" : ""}`; bar.append(title);
      el("path", { d: day.points.map((point, i) => `${i ? "L" : "M"}${this.x(point.at)},${this.y(point.remaining)}`).join(""), class: "quota-line spend-line" });
    }
    const indices = [...new Set([0, Math.floor((data.days.length - 1) / 2), data.days.length - 1])];
    for (const index of indices) {
      const day = data.days[index];
      el("text", { x: this.x((day.at + day.end) / 2), y: 197, "text-anchor": "middle" }, day.date.slice(5));
    }
    this.marker = el("circle", { r: 4, class: "quota-marker hidden" });
    this.tooltip = el("g", { class: "quota-tooltip hidden", "aria-hidden": "true" });
    this.tooltipTime = el("text", { x: 12, y: 23 });
    this.tooltipValue = el("text", { x: 12, y: 47, class: "quota-tooltip-value" });
    this.tooltip.append(el("rect", { width: 240, height: 60, rx: 5 }), this.tooltipTime, this.tooltipValue);
  }

  showPoint(point) {
    super.showPoint(point);
    const x = this.x(point.at), y = this.y(point.remaining);
    this.tooltip.setAttribute("transform", `translate(${Math.max(8, x + 252 > 592 ? x - 252 : x + 12)},${y < 80 ? y + 12 : y - 72})`);
    this.tooltipValue.textContent = `累计 ${money(point.remaining)}${point.status === "partial" ? " · 部分" : ""}`;
    this.svg.setAttribute("aria-label", `${point.date}，日内累计估算费用 ${money(point.remaining)}。左右方向键查看费用。`);
  }
}

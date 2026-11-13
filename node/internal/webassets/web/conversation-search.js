// Full-text search over conversation titles and user/assistant prose. While a
// query is present the results replace the sidebar's conversation list.

const debounceMs = 250;

const relativeTime = value => {
  if (!value) return "";
  const date = new Date(value);
  const days = Math.floor((Date.now() - date.getTime()) / 86400000);
  if (days < 1) return date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false });
  if (days < 365) return date.toLocaleDateString("zh-CN", { month: "numeric", day: "numeric" });
  return date.toLocaleDateString("zh-CN", { year: "numeric", month: "numeric", day: "numeric" });
};

// Highlight each term case-insensitively without parsing HTML.
export function highlightNodes(text, terms) {
  const needles = [...new Set(terms.map(term => term.toLowerCase()).filter(Boolean))].sort((a, b) => b.length - a.length);
  const lower = text.toLowerCase();
  const nodes = [];
  let cursor = 0;
  while (cursor < text.length) {
    let found = -1, length = 0;
    for (const needle of needles) {
      const index = lower.indexOf(needle, cursor);
      if (index >= 0 && (found < 0 || index < found)) { found = index; length = needle.length; }
    }
    if (found < 0) break;
    if (found > cursor) nodes.push(document.createTextNode(text.slice(cursor, found)));
    const mark = document.createElement("mark");
    mark.textContent = text.slice(found, found + length);
    nodes.push(mark);
    cursor = found + length;
  }
  if (cursor < text.length) nodes.push(document.createTextNode(text.slice(cursor)));
  return nodes;
}

export class ConversationSearch {
  constructor({ input, results, list, api, onOpen }) {
    Object.assign(this, { input, results, list, api, onOpen });
    this.timer = null;
    this.controller = null;
    this.query = "";
    input.addEventListener("input", () => this.schedule());
    input.addEventListener("search", () => this.schedule(0));
    input.addEventListener("keydown", event => {
      if (event.key === "Escape" && input.value) { event.preventDefault(); event.stopPropagation(); this.clear(); }
      if (event.key === "ArrowDown") { this.results.querySelector("button")?.focus(); event.preventDefault(); }
    });
    results.addEventListener("click", event => {
      const target = event.target.closest("[data-search-thread]");
      if (!target) return;
      const seq = target.dataset.searchSeq ? Number(target.dataset.searchSeq) : null;
      this.onOpen?.({ engine: target.dataset.searchEngine, threadId: target.dataset.searchThread, itemSeq: seq, role: target.dataset.searchRole || null });
    });
    results.addEventListener("keydown", event => {
      if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
      const buttons = [...results.querySelectorAll("button")];
      const index = buttons.indexOf(document.activeElement);
      if (event.key === "ArrowUp" && index <= 0) this.input.focus();
      else buttons[Math.max(0, Math.min(buttons.length - 1, index + (event.key === "ArrowDown" ? 1 : -1)))]?.focus();
      event.preventDefault();
    });
  }

  get active() { return this.input.value.trim() !== ""; }

  clear() {
    this.input.value = "";
    this.schedule(0);
  }

  schedule(delay = debounceMs) {
    clearTimeout(this.timer);
    this.timer = setTimeout(() => void this.run(), delay);
  }

  async run() {
    const query = this.input.value.trim();
    this.controller?.abort();
    this.controller = null;
    this.query = query;
    if (!query) {
      this.results.hidden = true;
      this.results.replaceChildren();
      this.list.hidden = false;
      return;
    }
    this.list.hidden = true;
    this.results.hidden = false;
    this.results.setAttribute("aria-busy", "true");
    if (!this.results.childElementCount) this.status("正在搜索…");
    const controller = this.controller = new AbortController();
    try {
      const response = await this.api(`/v1/conversations/search?${new URLSearchParams({ storeId: "personal", q: query })}`, { signal: controller.signal });
      if (controller !== this.controller) return;
      this.render(response);
    } catch (error) {
      if (controller !== this.controller || error.name === "AbortError") return;
      this.status(`搜索失败：${error.message}`);
    } finally {
      if (controller === this.controller) this.results.removeAttribute("aria-busy");
    }
  }

  status(text) {
    const note = document.createElement("p");
    note.className = "search-status";
    note.setAttribute("role", "status");
    note.textContent = text;
    this.results.replaceChildren(note);
  }

  render(response) {
    const terms = response.terms ?? [];
    const items = response.results ?? [];
    if (!items.length) { this.status("没有找到匹配的对话"); return; }
    const header = document.createElement("p");
    header.className = "search-status";
    header.setAttribute("role", "status");
    header.textContent = `${items.length}${response.truncated ? "+" : ""} 个对话`;
    const list = document.createElement("ul");
    list.className = "search-result-list";
    for (const item of items) {
      const entry = document.createElement("li");
      entry.className = "search-result";
      const open = document.createElement("button");
      open.type = "button";
      open.className = "search-result-title";
      open.dataset.searchThread = item.threadId;
      open.dataset.searchEngine = item.engine;
      if (!item.titleMatch && item.hits?.[0]) {
        open.dataset.searchSeq = String(item.hits[0].itemSeq);
        open.dataset.searchRole = item.hits[0].role;
      }
      const title = document.createElement("span");
      title.className = "search-result-name";
      title.append(...highlightNodes(item.title || "未命名对话", terms));
      const meta = document.createElement("span");
      meta.className = "search-result-meta";
      const parts = [];
      if (item.engine === "claude") parts.push("Claude");
      if (item.parentThreadId) parts.push(`子 Agent · ${item.rootTitle || "未命名对话"}`);
      if (item.archived) parts.push("已归档");
      if (item.hitCount > 1) parts.push(`${item.hitCount} 处`);
      const when = relativeTime(item.latestAt);
      if (when) parts.push(when);
      meta.textContent = parts.join(" · ");
      open.append(title, meta);
      entry.append(open);
      for (const hit of item.hits ?? []) {
        const snippet = document.createElement("button");
        snippet.type = "button";
        snippet.className = "search-result-hit";
        snippet.dataset.searchThread = item.threadId;
        snippet.dataset.searchEngine = item.engine;
        snippet.dataset.searchSeq = String(hit.itemSeq);
        snippet.dataset.searchRole = hit.role;
        const role = document.createElement("span");
        role.className = "search-result-role";
        role.textContent = hit.role === "user" ? "你" : "回复";
        const text = document.createElement("span");
        text.append(...highlightNodes(hit.snippet, terms));
        snippet.append(role, text);
        snippet.setAttribute("aria-label", `${role.textContent}：${hit.snippet}`);
        entry.append(snippet);
      }
      list.append(entry);
    }
    this.results.replaceChildren(header, list);
  }
}

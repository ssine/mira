// Pagination owns ordered server cursors independently of periodic head reads.
// Filter changes invalidate in-flight responses; loading another page never
// discards rows already read, including when a server cursor has expired.
export class ThreadPager {
  constructor(fetchPage, accept, changed = () => {}) {
    this.fetchPage = fetchPage;
    this.accept = accept;
    this.changed = changed;
    this.reset(false);
  }
  reset(archived) {
    this.epoch = (this.epoch ?? 0) + 1;
    this.archived = archived;
    this.enabled = false;
    this.projects = [];
    this.pages = new Map();
  }
  state(view, key = "") {
    const id = JSON.stringify([view, key]);
    if (!this.pages.has(id)) this.pages.set(id, { cursor: null, started: false, done: false, loading: false, checkedAt: 0 });
    return this.pages.get(id);
  }
  load(view, key = "", head = false) {
    const state = this.state(view, key);
    const slot = head ? "headPromise" : "promise";
    if (state[slot]) return state[slot];
    const epoch = this.epoch;
    if (!head) { state.loading = true; state.error = null; }
    let needsRender = !head || state.checkedAt === 0;
    const operation = (async () => {
      try {
        const query = new URLSearchParams({ storeId: "personal", view, limit: "50", archived: this.archived ? "1" : "0" });
        if (view === "children") query.set("parentThreadId", key);
        else if (key) query.set("projectKey", key);
        if (head) query.set("head", "1");
        else if (state.cursor) query.set("cursor", state.cursor);
        let page;
        try { page = await this.fetchPage(query); }
        catch (error) {
          if (error.code !== "thread_cursor_expired" || !query.has("cursor") || epoch !== this.epoch) throw error;
          query.delete("cursor");
          page = await this.fetchPage(query);
        }
        if (epoch !== this.epoch) return;
        this.enabled = page.paged === true;
        if (Array.isArray(page.projects)) {
          needsRender ||= JSON.stringify(this.projects) !== JSON.stringify(page.projects);
          this.projects = page.projects;
        }
        if (!head) {
          state.cursor = page.nextCursor;
          state.started = true;
          state.done = !page.nextCursor;
        }
        state.total = page.total;
        state.checkedAt = Date.now();
        needsRender = this.accept(page) || needsRender;
        return page;
      } catch (error) {
        if (epoch === this.epoch && !head) state.error = error.message;
        throw error;
      } finally {
        if (!head) state.loading = false;
        state[slot] = null;
        if (epoch === this.epoch && needsRender) this.changed();
      }
    })();
    state[slot] = operation;
    if (!head) this.changed();
    return operation;
  }
}

export function mergeThreadPages(previous, incoming, removed = []) {
  const rows = new Map(previous.map(row => [row.threadId, row]));
  for (const id of removed) rows.delete(id);
  for (const row of incoming) {
    const before = rows.get(row.threadId);
    if (before && (before.generation > row.generation || before.generation === row.generation && before.itemCount > row.itemCount)) continue;
    rows.set(row.threadId, { ...before, ...row });
  }
  return [...rows.values()];
}

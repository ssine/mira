// Browser-local copies of Server reads, shown first and then revalidated.
// PostgreSQL stays authoritative: every entry is disposable, bound to one Server
// version, bounded in total size and evicted least recently used first.
const databaseName = "mira-client-cache";
const sizeOf = value => JSON.stringify(value)?.length ?? 0;
const resolve = operation => {
  if (typeof operation.value !== "function") return operation.value;
  try { return operation.value() ?? undefined; } catch { return undefined; }
};

export class ClientCache {
  constructor({ budgetBytes = 96 * 1024 * 1024, entryBytes = 16 * 1024 * 1024, maxEntries = 400, delayMs = 1000 } = {}) {
    Object.assign(this, { budgetBytes, entryBytes, maxEntries, delayMs });
    this.version = null;
    this.database = null;
    this.pending = new Map();
    this.timer = null;
    this.pruning = null;
  }

  // Projections may change between releases; never reuse another version's copies.
  setVersion(version) { this.version = String(version || ""); }

  open() {
    if (typeof indexedDB === "undefined") return Promise.reject(new Error("IndexedDB unavailable"));
    this.database ??= new Promise((resolve, reject) => {
      const request = indexedDB.open(databaseName, 1);
      request.onupgradeneeded = () => {
        const database = request.result;
        database.createObjectStore("entries");
        database.createObjectStore("meta", { keyPath: "key" }).createIndex("usedAt", "usedAt");
        database.createObjectStore("snapshots");
      };
      request.onerror = () => reject(request.error);
      request.onblocked = () => reject(new Error("缓存正在被其他页面升级"));
      request.onsuccess = () => {
        const database = request.result;
        database.onversionchange = () => { database.close(); this.database = null; };
        database.onclose = () => { this.database = null; };
        resolve(database);
      };
    }).catch(error => { this.database = null; throw error; });
    return this.database;
  }

  async #transaction(stores, mode, work) {
    const database = await this.open();
    return new Promise((resolve, reject) => {
      const transaction = database.transaction(stores, mode);
      let result;
      transaction.oncomplete = () => resolve(result);
      transaction.onabort = transaction.onerror = () => reject(transaction.error);
      result = work(transaction);
    });
  }

  // Large per-conversation values (transcripts). Reading marks the entry as used.
  async read(key) {
    if (this.pending.has(`entry:${key}`)) {
      const value = resolve(this.pending.get(`entry:${key}`));
      if (value !== undefined) return value ?? undefined;
    }
    try {
      const record = await this.#transaction(["entries", "meta"], "readwrite", transaction => {
        const holder = {};
        const request = transaction.objectStore("entries").get(key);
        request.onsuccess = () => {
          holder.record = request.result;
          if (holder.record?.version !== this.version) return;
          const meta = transaction.objectStore("meta");
          const metaRequest = meta.get(key);
          metaRequest.onsuccess = () => { if (metaRequest.result) meta.put({ ...metaRequest.result, usedAt: Date.now() }); };
        };
        return holder;
      });
      return record.record?.version === this.version ? record.record.value : undefined;
    } catch { return undefined; }
  }

  // A function value is evaluated when the write is flushed, so callers can
  // queue "the state at rest" and decline (undefined) if it became inconsistent.
  write(key, value) { this.#queue(`entry:${key}`, { kind: "entry", key, value }); }
  remove(key) { this.#queue(`entry:${key}`, { kind: "entry", key, value: null }); }

  // Small whole-view values (sidebar, nodes, residency observations).
  async readSnapshot(name) {
    if (this.pending.has(`snapshot:${name}`)) return this.pending.get(`snapshot:${name}`).value;
    try {
      const record = await this.#transaction(["snapshots"], "readonly", transaction => {
        const holder = {};
        const request = transaction.objectStore("snapshots").get(name);
        request.onsuccess = () => { holder.record = request.result; };
        return holder;
      });
      return record.record?.version === this.version ? record.record.value : undefined;
    } catch { return undefined; }
  }

  writeSnapshot(name, value) { this.#queue(`snapshot:${name}`, { kind: "snapshot", key: name, value }); }

  #queue(id, operation) {
    if (!this.version) return;
    this.pending.set(id, operation);
    this.timer ??= setTimeout(() => { this.timer = null; void this.flush(); }, this.delayMs);
  }

  // Writes are coalesced per key. Serialization happens here, off the read path.
  async flush() {
    clearTimeout(this.timer);
    this.timer = null;
    if (!this.pending.size) return;
    const operations = [...this.pending.values()].map(operation => ({ ...operation, value: resolve(operation) }));
    this.pending.clear();
    const version = this.version, usedAt = Date.now();
    let grew = false;
    try {
      await this.#transaction(["entries", "meta", "snapshots"], "readwrite", transaction => {
        for (const { kind, key, value } of operations) {
          if (kind === "snapshot") {
            if (value === undefined) transaction.objectStore("snapshots").delete(key);
            else transaction.objectStore("snapshots").put({ version, value }, key);
            continue;
          }
          if (value === undefined) continue;
          const bytes = value === null ? 0 : sizeOf(value);
          if (value === null || bytes > this.entryBytes) {
            transaction.objectStore("entries").delete(key);
            transaction.objectStore("meta").delete(key);
            continue;
          }
          transaction.objectStore("entries").put({ version, value }, key);
          transaction.objectStore("meta").put({ key, bytes, usedAt });
          grew = true;
        }
      });
    } catch { /* Storage is optional: quota or private browsing only loses copies. */ }
    if (grew) await this.prune();
  }

  async prune() {
    this.pruning ??= (async () => {
      try {
        const rows = await this.#transaction(["meta"], "readonly", transaction => {
          const holder = { rows: [] };
          const request = transaction.objectStore("meta").index("usedAt").openCursor();
          request.onsuccess = () => {
            const cursor = request.result;
            if (!cursor) return;
            holder.rows.push(cursor.value);
            cursor.continue();
          };
          return holder;
        });
        let total = rows.rows.reduce((sum, row) => sum + row.bytes, 0), count = rows.rows.length;
        const evicted = [];
        for (const row of rows.rows) {
          if (total <= this.budgetBytes && count <= this.maxEntries) break;
          evicted.push(row.key); total -= row.bytes; count--;
        }
        if (evicted.length) await this.#transaction(["entries", "meta"], "readwrite", transaction => {
          for (const key of evicted) { transaction.objectStore("entries").delete(key); transaction.objectStore("meta").delete(key); }
        });
      } catch { /* A later write retries eviction. */ }
      finally { this.pruning = null; }
    })();
    return this.pruning;
  }

  async clear() {
    this.pending.clear();
    clearTimeout(this.timer);
    this.timer = null;
    try {
      await this.#transaction(["entries", "meta", "snapshots"], "readwrite", transaction => {
        for (const store of ["entries", "meta", "snapshots"]) transaction.objectStore(store).clear();
      });
    } catch { /* Nothing cached. */ }
  }
}

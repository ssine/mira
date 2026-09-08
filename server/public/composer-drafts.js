// Unsent browser-local drafts only. Canonical conversation history stays on Server.
export class ComposerDrafts {
  constructor() {
    this.pending = new Map();
    this.writes = new Map();
    this.database = null;
    this.fileIDs = new WeakMap();
  }

  open() {
    if (!this.database) {
      this.database = new Promise((resolve, reject) => {
        const request = indexedDB.open("mira-composer-drafts", 1);
        let blocked = false;
        request.onupgradeneeded = () => {
          request.result.createObjectStore("drafts");
          request.result.createObjectStore("files");
        };
        request.onerror = () => reject(request.error);
        request.onblocked = () => { blocked = true; reject(new Error("草稿存储正在被其他页面占用")); };
        request.onsuccess = () => {
          const database = request.result;
          if (blocked) { database.close(); return; }
          database.onversionchange = () => { database.close(); this.database = null; };
          database.onclose = () => { this.database = null; };
          resolve(database);
        };
      }).catch(error => { this.database = null; throw error; });
    }
    return this.database;
  }

  async read(key) {
    await this.writes.get(key)?.catch(() => {});
    if (this.pending.has(key)) return this.pending.get(key);
    const database = await this.open();
    return new Promise((resolve, reject) => {
      const transaction = database.transaction(["drafts", "files"], "readonly");
      const request = transaction.objectStore("drafts").get(key);
      const files = transaction.objectStore("files").get(key);
      transaction.oncomplete = () => {
        const value = request.result;
        if (value && typeof value.text === "string") value.files = files.result ?? [];
        resolve(this.pending.has(key) ? this.pending.get(key) : value);
      };
      transaction.onabort = () => reject(transaction.error);
    });
  }

  write(key, value, options = {}) {
    const operation = this.persist(key, value, options);
    const keys = [key, options.removeKey].filter(Boolean);
    for (const entry of keys) this.writes.set(entry, operation);
    const finish = () => {
      for (const entry of keys) if (this.writes.get(entry) === operation) this.writes.delete(entry);
    };
    void operation.then(finish, finish);
    return operation;
  }

  async persist(key, value, { removeKey } = {}) {
    // Retain unsaved edits in memory when storage is unavailable or full.
    this.pending.set(key, value);
    if (removeKey && removeKey !== key) this.pending.set(removeKey, undefined);
    const database = await this.open();
    await new Promise((resolve, reject) => {
      const transaction = database.transaction(["drafts", "files"], "readwrite");
      const store = transaction.objectStore("drafts");
      const files = transaction.objectStore("files");
      if (value === undefined) { store.delete(key); files.delete(key); }
      else if (value.files) {
        const { files: attachments, ...metadata } = value;
        const attachmentVersion = JSON.stringify(attachments.map(file => {
          if (!this.fileIDs.has(file)) this.fileIDs.set(file, crypto.randomUUID());
          return this.fileIDs.get(file);
        }));
        // Compare inside the transaction, including writes from other tabs.
        // Unchanged files need not be serialized for every text keystroke.
        const previous = store.get(key);
        previous.onsuccess = () => {
          if (previous.result?.attachmentVersion !== attachmentVersion) files.put(attachments, key);
        };
        store.put({ ...metadata, attachmentVersion }, key);
      } else store.put(value, key);
      if (removeKey && removeKey !== key) { store.delete(removeKey); files.delete(removeKey); }
      transaction.oncomplete = resolve;
      transaction.onabort = () => reject(transaction.error);
    });
    if (this.pending.get(key) === value) this.pending.delete(key);
    if (removeKey && this.pending.has(removeKey) && this.pending.get(removeKey) === undefined) this.pending.delete(removeKey);
  }
}

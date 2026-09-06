const cache = new Map();

// A short read-only connection: inspecting a picker never creates/resumes a
// conversation or sends a model request. Cache only the model fields, not config.
export async function readModelCatalog(nodeId, cwd, { refresh = false } = {}) {
  const key = JSON.stringify([nodeId, cwd]);
  const existing = cache.get(key);
  if (existing?.job) return existing.job;
  if (!refresh && existing?.expires > Date.now()) return existing.value;
  const entry = {};
  const job = (async () => {
    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    const socket = new WebSocket(`${scheme}//${location.host}/v1/nodes/${nodeId}/app-server?storeId=personal`, ["mira-client-v1"]);
    const pending = new Map();
    let requestId = 0, rejectOpen;
    const fail = error => { rejectOpen?.(error); for (const request of pending.values()) request.reject(error); };
    const timer = setTimeout(() => { fail(new Error("读取模型超时，点击刷新重试")); socket.close(); }, 15_000);
    socket.addEventListener("close", () => fail(new Error("模型连接已关闭")));
    socket.addEventListener("error", () => fail(new Error("无法读取节点模型")));
    socket.addEventListener("message", event => {
      let message; try { message = JSON.parse(event.data); } catch { return; }
      const request = pending.get(message.id);
      if (!request) return;
      pending.delete(message.id);
      message.error ? request.reject(new Error(message.error.message || "无法读取模型")) : request.resolve(message.result);
    });
    const call = (method, params) => new Promise((resolve, reject) => {
      if (socket.readyState !== WebSocket.OPEN) { reject(new Error("模型连接已关闭")); return; }
      const id = ++requestId;
      pending.set(id, { resolve, reject });
      socket.send(JSON.stringify({ id, method, params }));
    });
    try {
      await new Promise((resolve, reject) => { rejectOpen = reject; socket.addEventListener("open", resolve, { once: true }); });
      await call("initialize", { clientInfo: { name: "mira_web_models", version: "1" }, capabilities: { experimentalApi: true } });
      socket.send(JSON.stringify({ method: "initialized" }));
      const [{ config }, first] = await Promise.all([
        call("config/read", { includeLayers: false, ...(cwd ? { cwd } : {}) }),
        call("model/list", { limit: 100 }),
      ]);
      if (!config || !Array.isArray(first?.data)) throw new Error("节点未提供模型配置");
      const models = new Map(), seen = new Set();
      let page = first;
      while (page) {
        for (const model of page.data ?? []) {
          if (typeof model.model === "string" && model.model && !model.hidden) models.set(model.model, { model: model.model, isDefault: model.isDefault === true });
        }
        if (!page.nextCursor) break;
        if (seen.has(page.nextCursor)) throw new Error("模型列表分页异常");
        seen.add(page.nextCursor);
        page = await call("model/list", { limit: 100, cursor: page.nextCursor });
      }
      const defaultModel = typeof config.model === "string" && config.model || [...models.values()].find(model => model.isDefault)?.model || null;
      if (defaultModel && !models.has(defaultModel)) models.set(defaultModel, { model: defaultModel });
      return { models: [...models.values()], defaultModel };
    } finally { clearTimeout(timer); socket.close(); }
  })();
  entry.job = job;
  cache.set(key, entry);
  try { entry.value = await job; entry.expires = Date.now() + 300_000; return entry.value; }
  finally {
    delete entry.job;
    if (!entry.value && cache.get(key) === entry) cache.delete(key);
    while (cache.size > 40) cache.delete(cache.keys().next().value);
  }
}

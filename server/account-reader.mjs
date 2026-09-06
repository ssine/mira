import crypto from "node:crypto";

// Dedicated read-only tunnels never enter the conversation/tool broker. App Server
// can broadcast thread notifications to every connection, including this one.
export class AccountReader {
  constructor(send, { timeoutMs = 20_000 } = {}) {
    this.send = send;
    this.timeoutMs = timeoutMs;
    this.sessions = new Map();
  }

  handle(nodeId, message) {
    const session = this.sessions.get(message.sessionId);
    if (!session || session.nodeId !== nodeId) return false;
    if (["appserver.error", "appserver.closed"].includes(message.type)) session.close();
    if (message.type === "appserver.message") {
      let value;
      try { value = JSON.parse(message.payload); } catch { session.close(); return true; }
      if (value.method === "account/updated") session.changed = true;
      const pending = session.pending.get(value.id);
      if (pending && value.method === undefined) {
        session.pending.delete(value.id);
        value.error ? pending.reject(new Error("account RPC failed")) : pending.resolve(value.result);
      }
    }
    return true;
  }

  close(nodeId) {
    for (const session of this.sessions.values()) if (!nodeId || session.nodeId === nodeId) session.close();
  }

  async read(nodeId, { signal } = {}) {
    if (signal?.aborted) throw new Error("account sampling stopped");
    const sessionId = crypto.randomUUID();
    const session = { nodeId, pending: new Map(), changed: false, closed: false };
    const send = message => {
      if (session.closed || !this.send(nodeId, { ...message, sessionId })) throw new Error("account channel offline");
    };
    session.close = () => {
      if (session.closed) return;
      session.closed = true;
      clearTimeout(timer);
      this.sessions.delete(sessionId);
      this.send(nodeId, { type: "appserver.close", sessionId });
      for (const pending of session.pending.values()) pending.reject(new Error("account channel closed"));
      session.pending.clear();
    };
    let id = 0;
    const call = (method, params) => new Promise((resolve, reject) => {
      const requestId = ++id;
      session.pending.set(requestId, { resolve, reject });
      try { send({ type: "appserver.message", payload: JSON.stringify({ id: requestId, method, params }) }); }
      catch (error) { session.pending.delete(requestId); reject(error); }
    });
    const timer = setTimeout(session.close, this.timeoutMs);
    signal?.addEventListener("abort", session.close, { once: true });
    this.sessions.set(sessionId, session);
    try {
      send({ type: "appserver.open" });
      await call("initialize", { clientInfo: { name: "mira_account_history", version: "1" }, capabilities: { experimentalApi: true } });
      send({ type: "appserver.message", payload: JSON.stringify({ method: "initialized" }) });
      const { account } = await call("account/read", { refreshToken: false });
      session.changed = false;
      const limits = account?.type === "chatgpt" ? await call("account/rateLimits/read", {}) : null;
      // Do not pair a quota response with an identity replaced during the read.
      const verified = await call("account/read", { refreshToken: false });
      if (session.changed || JSON.stringify(account) !== JSON.stringify(verified.account)) throw new Error("account changed during sample");
      return { account, limits };
    } finally {
      signal?.removeEventListener("abort", session.close);
      session.close();
    }
  }
}

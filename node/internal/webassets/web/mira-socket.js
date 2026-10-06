// A WebSocket-shaped adapter over Mira's bounded HTTPS frame protocol.
// Reconnect remains owned by the caller; this adapter never resubmits RPCs.
let csrf = () => null;
export function configureMiraTransport(options = {}) { csrf = options.csrf ?? csrf; }
const encoder = new TextEncoder();
const decoder = new TextDecoder();
const MAX_FRAME = 16 * 1024 * 1024;
const failures = new Map();

export class MiraSocket extends EventTarget {
  constructor(url, protocols) {
    super();
    this.url = url; this.protocols = typeof protocols === "string" ? [protocols] : protocols; this.readyState = 0;
    this.abort = new AbortController(); this.received = 0; this.sent = 0;
    this.outgoing = Promise.resolve(); this.pendingBytes = 0; this.pendingFrames = 0; this.protocol = "";
    let preference = "auto";
    try { preference = new URL(location.href).searchParams.get("miraTransport") ?? globalThis.sessionStorage?.getItem("mira.transport") ?? "auto"; } catch {}
    this.preference = preference;
    if (preference === "https" || (preference === "auto" && (failures.get(new URL(url).origin) ?? 0) >= 2)) void this.openHTTPS();
    else this.openWebSocket();
  }
  emit(type, event) { this.dispatchEvent(event); this[`on${type}`]?.(event); }
  openWebSocket() {
    let opened = false;
    const native = new WebSocket(this.url, this.protocols); this.native = native;
    const fallback = () => {
      if (this.native !== native || this.readyState !== 0) return;
      this.native = null; clearTimeout(timer); native.close();
      if (this.preference === "websocket") this.finish(1006, "WebSocket unavailable");
      else { failures.set(new URL(this.url).origin, (failures.get(new URL(this.url).origin) ?? 0) + 1); void this.openHTTPS(); }
    };
    const timer = setTimeout(fallback, 3000); this.probeTimer = timer;
    native.addEventListener("open", () => { if (this.native !== native || this.readyState !== 0) return; clearTimeout(timer); opened = true; this.protocol = native.protocol; this.readyState = 1; this.emit("open", new Event("open")); });
    native.addEventListener("message", (event) => { if (this.native === native) this.emit("message", new MessageEvent("message", { data: event.data })); });
    native.addEventListener("error", () => { if (!opened) fallback(); });
    native.addEventListener("close", (event) => {
      if (this.native !== native) return;
      if (!opened && this.readyState === 0) { fallback(); return; }
      failures.set(new URL(this.url).origin, this.readyState === 2 || event.code === 1000 ? 0 : (failures.get(new URL(this.url).origin) ?? 0) + 1);
      this.finish(event.code ?? 1006, event.reason ?? "");
    });
  }
  async request(method, path, body, retry = false, limit = MAX_FRAME + 64 * 13) {
    const until = Date.now() + 55000;
    for (;;) {
      const attempt = new AbortController();
      const cancel = () => attempt.abort(this.abort.signal.reason);
      this.abort.signal.addEventListener("abort", cancel, { once: true });
      if (this.abort.signal.aborted) cancel();
      const timeout = setTimeout(() => attempt.abort(), Math.min(30000, Math.max(1, until - Date.now())));
      try {
        const response = await fetch(path, { method, credentials: "same-origin", signal: attempt.signal, redirect: "error",
          headers: { "X-Mira-Protocol": this.protocols[0], ...(method !== "GET" ? { "X-Mira-Csrf": csrf() ?? "", "Content-Type": "application/octet-stream" } : {}) }, body });
        if (!response.ok) {
          const error = new Error(`HTTPS transport returned HTTP ${response.status}`);
          error.terminal = response.status < 500 && ![408, 429].includes(response.status);
          throw error;
        }
        // Consume the complete response inside the retry boundary. A lost body
        // retries the identical send frame or receive cursor.
        const reader = response.body?.getReader(); const chunks = []; let size = 0;
        if (reader) {
          try {
            for (;;) {
              const { done, value } = await reader.read(); if (done) break;
              size += value.byteLength;
              if (size > limit) { const error = new Error("Oversized HTTPS response"); error.terminal = true; throw error; }
              chunks.push(value);
            }
          } finally { await reader.cancel().catch(() => {}); }
        }
        const result = new Uint8Array(size); let offset = 0;
        for (const chunk of chunks) { result.set(chunk, offset); offset += chunk.byteLength; }
        return result;
      } catch (error) {
        if (!retry || error.terminal || this.abort.signal.aborted || Date.now() >= until) throw error;
        await new Promise((resolve) => setTimeout(resolve, 200));
      } finally {
        clearTimeout(timeout); this.abort.signal.removeEventListener("abort", cancel);
      }
    }
  }
  async openHTTPS() {
    try {
      const url = new URL(this.url); url.protocol = url.protocol === "wss:" ? "https:" : "http:"; url.searchParams.set("transport", "https");
      const response = await this.request("POST", url.href, undefined, false, 4096);
      const session = JSON.parse(decoder.decode(response));
      if (!/^[a-f0-9]{32}$/.test(session.id) || session.protocol !== this.protocols[0]) throw new Error("Invalid HTTPS session");
      this.endpoint = `/v1/transports/${session.id}`;
      if (this.readyState === 3) { void this.closeHTTPS(); return; }
      this.readyState = 1; this.protocol = session.protocol; this.emit("open", new Event("open"));
      await this.receive();
    } catch (error) {
      if (this.readyState !== 3) { this.emit("error", new Event("error")); this.finish(1006, error.message); }
    }
  }
  async receive() {
    while (this.readyState === 1) {
      const data = await this.request("GET", `${this.endpoint}/receive?after=${this.received}`, undefined, true);
      if (data.length > MAX_FRAME + 64 * 13) throw new Error("Oversized HTTPS batch");
      const view = new DataView(data.buffer); let offset = 0; const frames = [];
      while (offset < data.length) {
        if (data.length - offset < 13 || frames.length >= 64) throw new Error("Invalid HTTPS batch");
        const kind = view.getUint8(offset), seq = Number(view.getBigUint64(offset + 1)), length = view.getUint32(offset + 9); offset += 13;
        if (![1, 2, 8].includes(kind) || seq !== this.received + frames.length + 1 || length > MAX_FRAME || offset + length > data.length) throw new Error("Invalid HTTPS frame");
        frames.push({ kind, seq, payload: data.slice(offset, offset + length) }); offset += length;
      }
      for (const { kind, seq, payload } of frames) {
        this.received = seq;
        if (kind === 8) { this.finish(1000, "Remote closed"); return; }
        if (this.readyState !== 1) return;
        this.emit("message", new MessageEvent("message", { data: kind === 1 ? decoder.decode(payload) : payload.buffer }));
      }
    }
  }
  send(data) {
    if (this.readyState !== 1) throw new Error("Mira socket is not open");
    if (this.native) { this.native.send(data); return; }
    const payload = typeof data === "string" ? encoder.encode(data) : new Uint8Array(data);
    if (payload.length > MAX_FRAME || this.pendingBytes + payload.length > MAX_FRAME || this.pendingFrames >= 64) throw new Error("Mira transport queue is full");
    const frame = new Uint8Array(13 + payload.length), header = new DataView(frame.buffer);
    header.setUint8(0, typeof data === "string" ? 1 : 2); header.setBigUint64(1, BigInt(++this.sent)); header.setUint32(9, payload.length); frame.set(payload, 13);
    this.pendingBytes += payload.length; this.pendingFrames++;
    this.outgoing = this.outgoing.then(async () => {
      if (this.readyState !== 1) return;
      try { await this.request("POST", `${this.endpoint}/send`, frame, true); }
      finally { this.pendingBytes -= payload.length; this.pendingFrames--; }
    }).catch((error) => { this.finish(1006, error.message); });
  }
  async closeHTTPS() {
    if (!this.endpoint) return;
    try { await fetch(`${this.endpoint}/close`, { method: "POST", credentials: "same-origin", redirect: "error", headers: { "X-Mira-Csrf": csrf() ?? "" }, keepalive: true }); } catch {}
  }
  close(code = 1000, reason = "") {
    if (this.readyState === 3) return;
    if (this.native) { clearTimeout(this.probeTimer); this.readyState = 2; this.native.close(code, reason); return; }
    void this.closeHTTPS(); this.finish(code, reason);
  }
  finish(code, reason) {
    if (this.readyState === 3) return;
    this.readyState = 3; clearTimeout(this.probeTimer); this.abort.abort();
    if (!this.native) void this.closeHTTPS();
    this.emit("close", Object.assign(new Event("close"), { code, reason, wasClean: code === 1000 }));
  }
  get bufferedAmount() { return this.native?.bufferedAmount ?? this.pendingBytes; }
}

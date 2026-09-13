import { accountQuery } from "./codex-accounts.js";

// Stopping a persisted turn must work in a reader that never resumed it.
// Use its recorded runtime, independently of the composer's current selection.
export async function interruptThread({ nodeId, nodeAccountId, threadId, turnId, timeoutMs = 30_000 }) {
  if (!nodeId || !threadId || !turnId) throw new Error("无法确定本轮的运行机器，请刷新会话后重试。");
  const scheme = location.protocol === "https:" ? "wss:" : "ws:";
  const socket = new WebSocket(`${scheme}//${location.host}/v1/nodes/${encodeURIComponent(nodeId)}/app-server?storeId=personal${accountQuery(nodeAccountId)}`, ["mira-client-v1"]);
  const pending = new Map();
  let nextId = 0, failure, rejectOpen;
  const fail = error => {
    failure ??= error;
    rejectOpen?.(failure);
    for (const request of pending.values()) request.reject(failure);
    pending.clear();
  };
  const call = (method, params) => new Promise((resolve, reject) => {
    if (failure || socket.readyState !== WebSocket.OPEN) { reject(failure ?? new Error("停止连接已断开，请重试。")); return; }
    const id = ++nextId;
    pending.set(id, { resolve, reject });
    socket.send(JSON.stringify({ id, method, params }));
  });
  socket.addEventListener("message", event => {
    let message; try { message = JSON.parse(event.data); } catch { return; }
    if (!Object.hasOwn(message, "result") && !Object.hasOwn(message, "error")) return;
    const request = pending.get(message.id);
    if (!request) return;
    pending.delete(message.id);
    message.error ? request.reject(new Error(message.error.message || "停止失败，请重试。")) : request.resolve(message.result);
  });
  socket.addEventListener("close", () => fail(new Error("停止连接已断开，请重试。")));
  socket.addEventListener("error", () => fail(new Error("无法连接此对话的运行机器，请重试。")));
  const deadline = setTimeout(() => fail(new Error("停止请求超时，请刷新状态后重试。")), timeoutMs);
  try {
    await new Promise((resolve, reject) => { rejectOpen = reject; socket.addEventListener("open", resolve, { once: true }); });
    await call("initialize", { clientInfo: { name: "mira_web_interrupt", version: "1" }, capabilities: { experimentalApi: true } });
    socket.send(JSON.stringify({ method: "initialized" }));
    const result = await call("thread/turns/list", { threadId, limit: 1, sortDirection: "desc", itemsView: "notLoaded" });
    const turn = result.data?.[0];
    if (!turn || turn.id !== turnId) throw new Error("会话轮次已变化，请刷新状态后重试。");
    if (["completed", "failed", "interrupted"].includes(turn.status)) return { interrupted: false };
    if (turn.status !== "inProgress") throw new Error("无法确认本轮运行状态，请稍后重试。");
    await call("turn/interrupt", { threadId, turnId });
    return { interrupted: true };
  } finally {
    clearTimeout(deadline);
    socket.close();
  }
}

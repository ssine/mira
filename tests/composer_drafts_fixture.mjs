// Disposable API/App Server fixture; serves the real Web sources and IndexedDB code.
import http from "node:http";
import fs from "node:fs/promises";
import { fileURLToPath } from "node:url";
const { WebSocketServer } = await import("ws").catch(() => import("../server/node_modules/ws/wrapper.mjs"));

export async function startDraftFixture({ port = 0, forkTest = false } = {}) {
  const nodeId = "00000000-0000-4000-8000-000000000001";
  const ids = ["00000000-0000-4000-8000-0000000000a1", "00000000-0000-4000-8000-0000000000b2"];
  const node = { nodeId, hostname: "Draft fixture", platform: "linux", status: "online", approvalStatus: "approved", capabilities: { appServer: true, files: true }, reportedAppServer: { status: "running" }, desiredAppServer: { defaultCwd: "/work" } };
  const summary = (threadId, cwd = "/work") => ({ threadId, cwd, title: `Draft ${threadId.slice(-2)}`, runtimeNodeId: nodeId, generation: 1, itemCount: 1, model: "fixture", activity: { state: "idle" }, updatedAt: new Date().toISOString() });
  let rows = ids.map(id => summary(id));
  let fail = false, delay = 0, uploadDelay = 0;
  const uploads = [];
  let forkPending = null;
  const forkChild = "00000000-0000-4000-8000-0000000000f1", forkUpload = "00000000-0000-4000-8000-0000000000f2";
  const forkEvents = [];

  const assetSource = await fs.readFile(new URL("../node/internal/webassets/webassets.go", import.meta.url), "utf8");
  const csp = assetSource.match(/const contentSecurityPolicy = "([^"]+)"/)[1];
  const server = http.createServer(async (request, response) => {
    const url = new URL(request.url, "http://localhost"), path = url.pathname;
    const json = value => { response.setHeader("Content-Type", "application/json"); response.end(JSON.stringify(value)); };
    let body = ""; for await (const chunk of request) body += chunk;
    if (forkTest && path === "/__test/fork") {
      if (request.method === "GET") return json({ pending: Boolean(forkPending), events: forkEvents });
      const command = JSON.parse(body);
      if (!forkPending) { response.statusCode = 409; return json({ error: "no pending fork" }); }
      const { socket, request: rpc } = forkPending;
      if (command.action === "success") {
        if (!rows.some(row => row.threadId === forkChild)) rows.push(summary(forkChild));
        socket.send(JSON.stringify({ id: rpc.id, result: { thread: { id: forkChild } } })); forkPending = null;
      } else {
        socket.send(JSON.stringify({ method: "mira/thread/fork/progress", params: {
          requestId: command.wrongRequest ? "unrelated" : rpc.id, threadId: forkChild, uploadId: forkUpload,
          phase: command.phase ?? "uploading", completedBytes: command.completedBytes ?? 41943040, totalBytes: 104857600,
        } }));
      }
      return json({});
    }
    if (forkTest && path === `/v2/stores/personal/history-uploads/${forkUpload}` && request.method === "DELETE") {
      forkEvents.push("cancel");
      if (!rows.some(row => row.threadId === forkChild)) rows.push(summary(forkChild));
      json({ status: "cancelled" });
      setTimeout(() => { if (forkPending) { forkPending.socket.send(JSON.stringify({ id: forkPending.request.id, error: { message: "upload cancelled" } })); forkPending = null; } }, 100);
      return;
    }
    if (path === "/__test/control") { ({ fail = false, delay = 0, uploadDelay = 0 } = JSON.parse(body)); return json({}); }
    if (path === "/__test/uploads") return json(uploads);
    if (path === "/healthz") return json({ version: "1.0.4", adminConfigured: true });
    if (path === "/v1/admin/session") return json({ csrfToken: "fixture" });
    if (path === "/v1/nodes") return json({ data: [node] });
    if (path === `/v1/nodes/${nodeId}`) return json(node);
    if (path.endsWith("/invoke")) {
      const { params } = JSON.parse(body);
      if (params.action === "write") { uploads.push(params); await new Promise(resolve => setTimeout(resolve, uploadDelay)); }
      return json({ result: {} });
    }
    if (path === "/v1/codex/threads") return json({ data: rows });
    const row = rows.find(row => path.includes(row.threadId));
    if (request.method === "DELETE" && row) { forkEvents.push(`delete:${row.threadId}`); rows = rows.filter(value => value !== row); return json({}); }
    if (path.endsWith("/transcript")) return json({ generation: 1, itemCount: 1, trace: [{ key: "history", kind: "assistant", body: "Persisted fixture history", turnId: "previous" }] });
    if (row) return json(row);
    if (path.startsWith("/v1/")) return json({ data: [] });
    try {
      const resource = path === "/" ? "index.html" : path.slice(1);
      if (resource.includes("..")) throw Error("Invalid path");
      const file = path === "/__test/scenarios.mjs" ? new URL("./composer_drafts_scenarios.mjs", import.meta.url) : new URL(resource.startsWith("vendor/") ? `../node/internal/webassets/web/${resource}` : `../server/public/${resource}`, import.meta.url);
      const type = resource.endsWith(".css") ? "text/css" : /\.(?:js|mjs)$/.test(resource) ? "text/javascript" : resource.endsWith(".html") ? "text/html" : "application/octet-stream";
      response.setHeader("Content-Type", type);
      response.setHeader("Content-Security-Policy", csp);
      response.setHeader("Cache-Control", "no-store");
      response.end(await fs.readFile(file));
    } catch { response.statusCode = 404; response.end(); }
  });
  const sockets = new WebSocketServer({ server });
  sockets.on("connection", socket => socket.on("message", async data => {
    const request = JSON.parse(data); if (request.id === undefined) return;
    const reply = result => socket.readyState === 1 && socket.send(JSON.stringify({ id: request.id, result }));
    const error = message => socket.readyState === 1 && socket.send(JSON.stringify({ id: request.id, error: { message } }));
    if (request.method === "initialize") {
      socket.client = request.params.clientInfo.name;
      return reply({});
    }
    if (socket.client === "mira_web_title") return error("Title generation disabled in fixture");
    if (request.method === "config/read") return reply({ config: { model: "fixture" } });
    if (request.method === "model/list") return reply({ data: [{ model: "fixture", displayName: "Fixture", isDefault: true, supportedReasoningEfforts: [] }], nextCursor: null });
    if (request.method === "thread/resume") { forkEvents.push(`resume:${request.params.threadId}`); }
    if (request.method === "thread/resume") return reply({ thread: { id: request.params.threadId, status: { type: "idle" } }, cwd: "/work", model: "fixture" });
    if (request.method === "thread/loaded/list") return reply({ data: rows.map(row => row.threadId) });
    if (forkTest && request.method === "thread/fork") { forkPending = { socket, request }; return; }
    if (request.method === "thread/start") {
      const row = summary("00000000-0000-4000-8000-0000000000c3", request.params.cwd);
      if (!rows.some(value => value.threadId === row.threadId)) rows.push(row);
      return reply({ thread: { id: row.threadId }, cwd: row.cwd, model: "fixture" });
    }
    if (request.method === "turn/start") {
      const shouldFail = fail;
      await new Promise(resolve => setTimeout(resolve, delay));
      return shouldFail ? error("Fixture send failed") : reply({ turn: { id: "fixture-turn", status: "completed" } });
    }
    return reply({ account: null, data: [] });
  }));
  await new Promise(resolve => server.listen(port, "127.0.0.1", resolve));
  return { origin: `http://127.0.0.1:${server.address().port}`, close: async () => { for (const socket of sockets.clients) socket.terminate(); await new Promise(resolve => server.close(resolve)); } };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const fixture = await startDraftFixture({ port: Number(process.argv[2] ?? 0), forkTest: process.argv.includes("--fork") });
  console.log(fixture.origin);
}

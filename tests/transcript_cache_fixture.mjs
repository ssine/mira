// A large disposable cache exercises the real Web reader without production data.
import http from "node:http";
import fs from "node:fs/promises";

const threadId = "00000000-0000-4000-8000-0000000000a1";
let items = Array.from({ length: 2400 }, (_, index) => ({
  key: `history-${index + 1}`, sourceItemSeq: index + 1, turnId: "turn",
  kind: index === 2399 ? "assistant" : index % 10 === 0 ? "reasoning" : "tool",
  title: "工具", status: "完成",
  body: index === 2399 ? "Latest reply\n\n" + "Reading surface.\n\n".repeat(150)
    : index % 10 === 0 ? `Thinking ${index}\n\n**Cached reasoning**` : `Output ${index}\n${"saved output ".repeat(40)}`,
}));
let generation = 1, holdTail = true, version = 1, replacement = false;
const pending = [], requests = [];
const row = () => ({ threadId, title: "Large cached conversation", cwd: "/work", generation, itemCount: items.length,
  listRoot: true, activity: { state: "idle" } });
const reply = (response, value) => response.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify(value));
const server = http.createServer(async (request, response) => {
  const url = new URL(request.url, "http://localhost"), path = url.pathname;
  if (path === "/__test/control") {
    let raw = ""; for await (const chunk of request) raw += chunk;
    if (raw) {
      const changes = JSON.parse(raw);
      holdTail = changes.holdTail ?? holdTail; generation = changes.generation ?? generation;
      if (changes.generation) replacement = true;
      if (changes.append) {
        version++;
        items.push({ key: `history-${items.length + 1}`, sourceItemSeq: items.length + 1,
          kind: "assistant", turnId: "next-turn", body: "New latest reply" });
      }
      if (changes.layout === "prose") {
        generation = 2; replacement = false;
        version = 1;
        items = Array.from({ length: 300 }, (_, index) => ({ key: `prose-${index}`, sourceItemSeq: index + 1,
          kind: "assistant", turnId: `turn-${index}`, body: `Paragraph ${index}\n\n${"Readable prose. ".repeat(100)}` }));
      }
      if (!holdTail) for (const finish of pending.splice(0)) finish();
    }
    return reply(response, { requests, pending: pending.length, generation });
  }
  if (path === "/__test/cache") return reply(response, { engine: "codex", generation, items, cursor: null,
    total: items.length, tailVersion: JSON.stringify([generation, version, items.length]), activityCount: items.length });
  if (path === "/__test/setup") return response.writeHead(200, { "Content-Type": "text/html" }).end("<!doctype html><title>Cache setup</title>");
  if (path === "/healthz") return reply(response, { version: "cache-test", adminConfigured: true });
  if (path === "/v1/admin/session") return reply(response, { csrfToken: "fixture" });
  if (path === "/v1/nodes") return reply(response, { data: [] });
  if (path === "/v1/codex/threads") return reply(response, { paged: true, data: [row()], projects: [], total: 1, nextCursor: null });
  if (path === `/v1/codex/threads/${threadId}/transcript`) {
    requests.push(Object.fromEntries(url.searchParams));
    const finish = () => reply(response, { generation, itemCount: items.length, storeVersion: version,
      trace: replacement ? [{ key: "replacement", kind: "assistant", body: "Replacement generation" }] : items.slice(-60),
      nextCursor: replacement ? null : `t2:${generation}:${items.length - 59}:${items.length}` });
    if (holdTail) pending.push(finish); else finish();
    return;
  }
  if (path === `/v1/codex/threads/${threadId}`) return reply(response, row());
  if (path.startsWith("/v1/")) return reply(response, { data: [] });
  try {
    const resource = path === "/" ? "index.html" : path.slice(1);
    if (resource.includes("..")) throw Error("Invalid path");
    const file = resource === "app.js" && process.env.MIRA_TEST_APP_SOURCE ? process.env.MIRA_TEST_APP_SOURCE
      : new URL(resource.startsWith("vendor/") ? `../node/internal/webassets/web/${resource}` : `../server/public/${resource}`, import.meta.url);
    let source = await fs.readFile(file);
    if (resource === "app.js") source = source.toString().replace("async function loadAgentTranscript", `
      const originalRenderTranscript = renderTranscript;
      globalThis.__testNotify = handleAgentNotification;
      globalThis.__testViewport = () => {
        const renderer = traceViewport();
        const group = document.querySelector(".tool-group");
        const first = group?._miraEntries.get("history-1");
        return { watched: renderer.rows.size, margin: renderer.margin,
          visible: [...renderer.rows.values()].filter(row => row.visible).length,
          first: first && { watched: renderer.rows.has(first.row), visible: renderer.rows.get(first.row)?.visible,
            cardVisible: first.card?._miraViewportVisible, children: first.row?.childElementCount,
            sameRow: first.card?.parentNode === first.row, top: first.row?.getBoundingClientRect().top } };
      };
      renderTranscript = (...args) => {
        const started = performance.now();
        try { return originalRenderTranscript(...args); }
        finally { document.documentElement.dataset.renderMs = String(performance.now() - started); }
      };
      async function loadAgentTranscript`);
    response.writeHead(200, { "Content-Type": resource.endsWith(".js") ? "text/javascript" : resource.endsWith(".css") ? "text/css" : "text/html", "Cache-Control": "no-store" }).end(source);
  } catch { response.writeHead(404).end(); }
});
server.listen(0, "127.0.0.1", () => console.log(`http://127.0.0.1:${server.address().port}`));

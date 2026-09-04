// Real-browser rendering test, isolated from a running Mira Server/Node or DB.
// Usage: node tests/trace_activity_browser.mjs [playwright-module] [browser-channel] [screenshot]
// Optional MIRA_TRACE_SCREENSHOT saves the final narrow-screen view.
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import http from "node:http";
import { projectCodexTranscript } from "../server/codex-transcript.mjs";

const { chromium } = await import(process.argv[2] ?? "playwright");
const root = new URL("../server/", import.meta.url);
const assets = new Map([
  ["/", ["public/index.html", "text/html"]],
  ["/app.js", ["public/app.js", "text/javascript"]],
  ["/trace-activity.js", ["public/trace-activity.js", "text/javascript"]],
  ["/styles.css", ["public/styles.css", "text/css"]],
  ["/vendor/xterm.js", ["node_modules/@xterm/xterm/lib/xterm.mjs", "text/javascript"]],
  ["/vendor/xterm-addon-fit.js", ["node_modules/@xterm/addon-fit/lib/addon-fit.mjs", "text/javascript"]],
  ["/vendor/xterm.css", ["node_modules/@xterm/xterm/css/xterm.css", "text/css"]],
  ["/vendor/marked.js", ["node_modules/marked/lib/marked.esm.js", "text/javascript"]],
  ["/vendor/dompurify.js", ["node_modules/dompurify/dist/purify.es.mjs", "text/javascript"]],
]);
const server = http.createServer(async (request, response) => {
  const asset = assets.get(request.url);
  if (!asset) { response.writeHead(404); response.end(); return; }
  try {
    let body = await fs.readFile(new URL(asset[0], root), "utf8");
    if (request.url === "/app.js") body = body.replace("void bootstrap();", `
      show("agentView");
      window.traceHarness = { agent, notify: handleAgentNotification, renderTranscript, renderThread,
        upsertTrace, clear: () => clear($("#conversationTrace")) };
    `);
    response.writeHead(200, { "content-type": asset[1], "cache-control": "no-store" });
    response.end(body);
  } catch (error) { response.writeHead(500); response.end(String(error)); }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
let browser;
try {
  browser = await chromium.launch({ headless: true, ...(process.argv[3] ? { channel: process.argv[3] } : {}) });
  const page = await browser.newPage({ viewport: { width: 1440, height: 1100 } });
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(`http://127.0.0.1:${server.address().port}/`);
  await page.waitForFunction(() => !!window.traceHarness);
  await page.evaluate(() => {
    const h = window.traceHarness;
    h.clear();
    h.agent.threadId = "thread-a";
    h.notify({ method: "turn/started", params: { threadId: "thread-a", turn: { id: "turn-1" } } });
  });
  async function notify(method, params) {
    await page.evaluate(({ method, params }) => window.traceHarness.notify({ method, params }),
      { method, params: { threadId: "thread-a", turnId: "turn-1", ...params } });
  }
  const read = { id: "cmd-1", type: "commandExecution", command: "cat config.go", cwd: "/project",
    commandActions: [{ type: "read", name: "config.go", path: "/project/config.go" }] };
  await notify("item/started", { item: { type: "reasoning", id: "reason-1", summary: [], content: ["raw"] } });
  await notify("item/reasoning/textDelta", { itemId: "reason-1", delta: "raw text" });
  assert.equal(await page.locator(".trace-card").count(), 0, "empty or raw reasoning must not appear");
  await notify("item/reasoning/summaryTextDelta", { itemId: "reason-1", summaryIndex: 0, delta: "**Inspecting configuration**" });
  await notify("item/reasoning/summaryTextDelta", { itemId: "reason-1", summaryIndex: 1, delta: "Checking the native test setup." });
  assert.equal(await page.locator(".reasoning .trace-kind").textContent(), "Inspecting configuration");

  await notify("item/started", { item: { type: "reasoning", id: "reason-resume", summary: ["**Resumed summary**", "Existing text"] } });
  await notify("item/reasoning/summaryTextDelta", { itemId: "reason-resume", summaryIndex: 1, delta: " plus streamed text" });
  assert.equal(await page.locator(".reasoning .trace-body").last().evaluate((node) => node._miraSource),
    "**Resumed summary**\n\nExisting text plus streamed text", "streaming must preserve already materialized summary parts");
  await page.locator(".trace-card.reasoning").last().evaluate((node) => node.remove());
  assert.equal(await page.locator(".reasoning .trace-status").textContent(), "");
  assert.equal(await page.locator(".reasoning .trace-detail").getAttribute("open"), null);
  assert.match(await page.locator(".reasoning .trace-body").evaluate((node) => node._miraSource), /\*\*\n\nChecking/);
  await notify("item/completed", { item: { type: "reasoning", id: "reason-1", summary: [] } });
  assert.equal(await page.locator(".reasoning .trace-kind").textContent(), "Inspecting configuration");

  await notify("item/started", { item: { ...read, status: "inProgress" } });
  assert.equal(await page.locator(".tool-group").getAttribute("open"), null);
  assert.match(await page.locator(".tool-group-latest").textContent(), /正在读取 \/project\/config.go/);
  await notify("item/commandExecution/outputDelta", { itemId: "cmd-1", delta: "package main\n" });
  assert.equal(await page.locator(".tool-group .trace-card").count(), 1);
  const done = { ...read, status: "completed", aggregatedOutput: "package main\n", exitCode: 0, durationMs: 15200 };
  await notify("item/completed", { item: done });
  assert.match(await page.locator(".tool-group-latest").textContent(), /已读取 .*15.2 秒/);
  assert.equal(await page.locator(".tool-group-counts").textContent(), "读取 × 1");
  await page.locator(".tool-group-summary").click();
  assert.equal(await page.locator(".tool .trace-detail").getAttribute("open"), null);
  await page.locator(".tool .trace-head").click();
  assert.match(await page.locator(".tool .trace-body").innerText(), /package main/);
  await page.evaluate(() => {
    window.copied = [];
    Object.defineProperty(navigator, "clipboard", { value: { writeText: async (text) => window.copied.push(text) }, configurable: true });
  });
  await page.locator(".tool .trace-copy").click();
  assert.equal(await page.locator(".tool .trace-detail").getAttribute("open"), "", "copy must not toggle details");
  assert.match(await page.evaluate(() => window.copied[0]), /cat config.go/);

  const search = { id: "cmd-2", type: "commandExecution", command: "rg sshRelay server.mjs", status: "completed",
    commandActions: [{ type: "search", query: "sshRelay", path: "server.mjs" }], aggregatedOutput: "found", exitCode: 0 };
  const file = { id: "patch-1", type: "fileChange", status: "completed", changes: [
    { path: "native_test.go", kind: { type: "add" }, diff: "package main\n\n// Test\n" },
  ] };
  await notify("item/completed", { item: search });
  await notify("item/completed", { item: file });
  assert.match(await page.locator(".tool-group-counts").textContent(), /读取 × 1 · 搜索 × 1 · 创建 × 1/);
  assert.match(await page.locator(".tool-group-latest").textContent(), /已创建 native_test.go \+3 −0/);
  const before = await page.locator(".tool .trace-kind").allTextContents();
  const history = projectCodexTranscript([done, search, file].map((item) => ({ type: "event_msg",
    payload: { type: "item_completed", turn_id: "turn-1", item } })));
  await page.evaluate((history) => {
    const h = window.traceHarness;
    h.agent.transcriptItems = history;
    h.renderTranscript(null);
  }, history);
  assert.deepEqual(await page.locator(".tool .trace-kind").allTextContents(), before, "history must preserve live descriptions");
  assert.equal(await page.locator(".tool-group").getAttribute("open"), "", "refresh preserves expanded groups");
  assert.equal(await page.locator(".tool .trace-detail").first().getAttribute("open"), "", "refresh preserves expanded items");
  await notify("item/completed", { item: done });
  assert.equal(await page.locator(".trace-card.tool").count(), 3, "live replay must not duplicate historical items");

  await page.evaluate(() => { window.traceHarness.agent.threadId = "thread-b"; window.traceHarness.clear(); });
  await notify("item/commandExecution/outputDelta", { itemId: "cmd-1", delta: "must not leak" });
  assert.equal(await page.locator(".trace-card").count(), 0, "events must not leak across threads");
  await page.evaluate(() => { window.traceHarness.agent.threadId = "thread-a"; });
  await notify("item/completed", { item: done });
  await notify("item/completed", { turnId: "turn-2", item: { ...done, aggregatedOutput: "a different turn" } });
  assert.equal(await page.locator(".trace-card.tool").count(), 2, "reused item IDs must be scoped to their turn");
  assert.equal(await page.locator(".tool-group").count(), 2, "tools from different turns must not merge into one group");

  await page.evaluate(() => {
    const h = window.traceHarness;
    h.clear();
    for (let i = 0; i < 50; i += 1) h.upsertTrace(`message-${i}`, "assistant", "Codex", `Message ${i}\n\nA short update.`, "");
    const trace = document.querySelector("#conversationTrace");
    trace.scrollTop = 0;
    h.upsertTrace("next-message", "assistant", "Codex", "Streaming update", "");
  });
  assert.equal(await page.locator("#conversationTrace").evaluate((node) => node.scrollTop), 0, "new activity must respect scrolling into history");
  await page.evaluate(() => {
    const h = window.traceHarness;
    const trace = document.querySelector("#conversationTrace");
    trace.scrollTop = trace.scrollHeight;
    h.upsertTrace("bottom-message", "assistant", "Codex", "Follow newest update", "");
  });
  assert.ok(await page.locator("#conversationTrace").evaluate((node) => node.scrollHeight - node.clientHeight - node.scrollTop < 2));

  await page.setViewportSize({ width: 390, height: 844 });
  await page.evaluate((history) => {
    const h = window.traceHarness;
    h.clear();
    h.upsertTrace("phase", "reasoning", "推理摘要", "**Planning native end-to-end tests**\n\nChecking Windows and Android.", "");
    for (const item of history) h.upsertTrace(item.key, item.kind, item.title, item.body, item.status, { activity: item.activity });
  }, history);
  await page.locator(".tool-group-summary").scrollIntoViewIfNeeded();
  const bounds = await page.locator(".tool-group").boundingBox();
  assert.ok(bounds.width <= 390 && bounds.x >= 0 && bounds.x + bounds.width <= 391, "tool group overflows mobile viewport");
  assert.ok(await page.locator(".tool-group").evaluate((node) => node.scrollWidth <= node.clientWidth + 1));
  await page.locator(".tool-group-summary").click();
  await page.locator(".tool .trace-head").last().focus();
  await page.keyboard.press("Enter");
  assert.equal(await page.locator(".tool .trace-detail").last().getAttribute("open"), "", "details must be keyboard accessible");
  await notify("item/completed", { item: { ...read, id: "injection", commandActions: [
    { type: "read", path: '<img src=x onerror="window.injected=true">' },
  ], status: "failed" } });
  assert.equal(await page.locator(".tool .trace-kind img").count(), 0, "activity labels must render as text, not HTML");
  assert.equal(await page.evaluate(() => window.injected), undefined);
  assert.equal(await page.locator(".activity-failed").count(), 1);
  assert.deepEqual(errors, [], "browser must have no uncaught errors");
  const screenshot = process.env.MIRA_TRACE_SCREENSHOT ?? process.argv[4];
  if (screenshot) await page.locator(".conversation-card").screenshot({ path: screenshot });
  console.log("PASS: real-browser live/history activity, summaries, grouping, copy, identity isolation, scrolling, narrow layout and keyboard checks");
} finally {
  await browser?.close();
  await new Promise((resolve) => server.close(resolve));
}

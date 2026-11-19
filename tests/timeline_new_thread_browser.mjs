// A pinned timeline follows a conversation created in this window: the first
// message's thread/start reply makes it the timeline's subject, and the
// header toggle can open it without a reload or a detour to another thread.
import assert from "node:assert/strict";
import crypto from "node:crypto";
const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const nodeId = crypto.randomUUID(), threadId = crypto.randomUUID();
const node = { nodeId, hostname: "Timeline test", platform: "linux", status: "online", approvalStatus: "approved", capabilities: { appServer: true },
  reportedAppServer: { status: "running" }, desiredAppServer: { defaultCwd: "/work" } };
const rows = [], reads = [];
const now = Date.now();
const timeline = { engine: "codex", rootThreadId: threadId, partial: false, truncated: false, delta: false, cursor: `x1.${threadId}:1:2`, lanes: [
  { threadId, parentThreadId: null, title: "New timeline", depth: 0,
    turns: [{ id: "turn-1", start: now - 20_000, end: now - 5_000, status: "completed" }], tools: [],
    messages: [{ at: now - 20_000, seq: 1, role: "user", preview: "Hello" }] },
] };

const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await context.addInitScript(() => { try { localStorage.setItem("mira.timeline.pinned", "0"); } catch { /* No storage. */ } });
  await context.route("**/v1/nodes", route => route.fulfill({ json: { data: [node] } }));
  await context.route("**/v1/nodes?*", route => route.fulfill({ json: { data: [node] } }));
  await context.route(`**/v1/nodes/${nodeId}`, route => route.fulfill({ json: node }));
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data: rows } }));
  await context.route(/\/v1\/codex\/threads\/[^?]+\?/, route => {
    const url = new URL(route.request().url()), path = url.pathname;
    if (path.endsWith("/timeline")) { reads.push(path); return route.fulfill({ json: timeline }); }
    if (path.endsWith("/transcript")) return route.fulfill({ json: { generation: 1, itemCount: 0, trace: [], nextCursor: null } });
    return route.fulfill({ json: rows[0] ?? {} });
  });
  await context.routeWebSocket(/\/app-server\?storeId=personal$/, socket => {
    let client;
    socket.onMessage(data => {
      const request = JSON.parse(data); if (request.id === undefined) return;
      if (request.method === "initialize") client = request.params.clientInfo.name;
      const reply = result => socket.send(JSON.stringify({ id: request.id, result }));
      if (client === "mira_web_account") return reply(request.method === "account/read" ? { account: null } : {});
      if (client === "mira_web_models") return reply(request.method === "model/list" ? { data: [], nextCursor: null } : request.method === "config/read" ? { config: {} } : {});
      if (client !== "mira_web") return socket.send(JSON.stringify({ id: request.id, error: { message: "Disabled in this fixture" } }));
      if (request.method === "initialize") reply({});
      else if (request.method === "thread/start") {
        rows.push({ threadId, title: "New timeline", cwd: "/work", runtimeNodeId: nodeId, generation: 1, itemCount: 2, activity: { state: "idle" }, tokenUsage: null });
        reply({ thread: { id: threadId }, cwd: "/work" });
      } else if (request.method === "turn/start") reply({ turn: { id: "turn-1", status: "completed" } });
      else if (request.method === "thread/loaded/list") reply({ data: [threadId] });
      else reply({});
    });
  });
  const page = await context.newPage(), errors = [];
  page.setDefaultTimeout(15_000);
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?view=agent`);
  await page.locator("#conversationInput").fill("Hello");
  await page.locator("#conversationSend").click();

  // The new conversation exposes the toggle; opening it reads and shows its
  // timeline in this same window.
  const toggle = page.locator("#conversationTimelineToggle");
  await toggle.waitFor({ state: "visible" });
  await page.waitForFunction(() => new URL(location.href).searchParams.get("thread"));
  await toggle.click();
  const panel = page.locator("#conversationTimeline");
  await panel.waitFor({ state: "visible" });
  await panel.locator(".timeline-chart").waitFor();
  assert.ok(reads.length >= 1 && reads.every(path => path.includes(threadId)), `the new thread's timeline is read: ${JSON.stringify(reads)}`);
  assert.equal(await toggle.getAttribute("aria-pressed"), "true");
  assert.deepEqual(errors, []);
  console.log("timeline new thread browser test passed");
} finally {
  await browser.close();
}

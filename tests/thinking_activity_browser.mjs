import assert from "node:assert/strict";
import crypto from "node:crypto";
const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const nodeId = crypto.randomUUID(), threadId = crypto.randomUUID();
const node = { nodeId, hostname: "Thinking fixture", platform: "linux", status: "online", approvalStatus: "approved",
  capabilities: { appServer: true }, reportedAppServer: { status: "running" }, desiredAppServer: { defaultCwd: "/work" } };
const thread = { threadId, title: "Streaming thoughts", cwd: "/work", model: "test-model", runtimeNodeId: nodeId,
  generation: 1, itemCount: 0, activity: { state: "idle" } };
let stream, turnId, acknowledge;
const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({ viewport: { width: 1200, height: 900 } });
  await context.route(/\/v1\/nodes(?:\?.*)?$/, route => route.fulfill({ json: { data: [node] } }));
  await context.route(`**/v1/nodes/${nodeId}`, route => route.fulfill({ json: node }));
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data: [thread] } }));
  await context.route(/\/v1\/codex\/threads\/[^?]+\?/, route => route.fulfill({ json:
    new URL(route.request().url()).pathname.endsWith("/transcript")
      ? { generation: 1, itemCount: 0, trace: [], nextCursor: null } : thread }));
  await context.routeWebSocket(/\/app-server\?/, socket => {
    let client;
    socket.onMessage(raw => {
      const request = JSON.parse(raw); if (request.id === undefined) return;
      const reply = result => socket.send(JSON.stringify({ id: request.id, result }));
      if (request.method === "initialize") { client = request.params.clientInfo.name; reply({}); return; }
      if (client === "mira_web_models") {
        reply(request.method === "config/read" ? { config: { model: "test-model" } }
          : { data: [{ model: "test-model", displayName: "Test", isDefault: true }], nextCursor: null }); return;
      }
      if (client === "mira_web_account") { reply(request.method === "account/read" ? { account: null } : {}); return; }
      if (request.method === "thread/resume") reply({ thread: { id: threadId, status: { type: "idle" } }, cwd: "/work", model: "test-model" });
      else if (request.method === "thread/loaded/list") reply({ data: [threadId] });
      else if (request.method === "turn/start") {
        stream = socket; turnId = crypto.randomUUID();
        thread.activity = { state: "running", turnId };
        // Hold the RPC acknowledgement as well as the reasoning completion:
        // submission feedback must not mask a live reasoning notification.
        acknowledge = () => reply({ turn: { id: turnId, status: "inProgress" } });
        socket.send(JSON.stringify({ method: "turn/started", params: { threadId, turn: { id: turnId } } }));
      } else reply({});
    });
  });
  const page = await context.newPage(), errors = [];
  page.on("pageerror", error => errors.push(error.message)); page.setDefaultTimeout(12_000);
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?thread=${threadId}`);
  const input = page.locator("#conversationInput"), notice = page.locator("#conversationActivity");
  const send = (method, params = {}) => stream.send(JSON.stringify({ method, params: { threadId, turnId, ...params } }));
  const begin = async () => {
    await input.fill("Think about this"); await page.locator("#conversationSend").click();
    await page.waitForFunction(() => !document.querySelector("#conversationStatus").classList.contains("hidden"));
    await assertEventually(() => typeof acknowledge === "function");
  };
  await begin();
  send("item/started", { item: { id: "thought", type: "reasoning", summary: [], content: [] } });
  send("item/reasoning/textDelta", { itemId: "thought", contentIndex: 0, delta: "" });
  assert.equal(await notice.isVisible(), false, "an empty item does not claim prefill has finished");
  send("item/reasoning/textDelta", { itemId: "thought", contentIndex: 0, delta: "First thought" });
  await notice.waitFor({ state: "visible" });
  assert.match(await notice.textContent(), /正在思考/);
  assert.equal(await page.locator(".trace-card.assistant").count(), 0, "status appears before any answer or completed thought");
  acknowledge(); acknowledge = null;
  await input.fill("Draft for the next message");
  await page.waitForTimeout(1200); // Include a normal status-timer tick with no new tokens.
  assert.equal(await notice.isVisible(), true, "thinking remains visible during a stream gap and with a draft");
  assert.match(await notice.textContent(), /正在思考/);
  assert.equal(await page.locator(".tool-group[open]").count(), 0, "collapsed thoughts need no interaction to show status");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.emulateMedia({ reducedMotion: "reduce" });
  assert.equal(await notice.isVisible(), true, "mobile and reduced motion retain the text label");
  send("item/started", { item: { id: "tool", type: "dynamicToolCall", tool: "test", arguments: {} } });
  await notice.waitFor({ state: "hidden" });
  send("item/completed", { item: { id: "tool", type: "dynamicToolCall", tool: "test", status: "completed", contentItems: [] } });
  send("item/reasoning/summaryTextDelta", { itemId: "thought-2", summaryIndex: 0, delta: "Thinking again" });
  await notice.waitFor({ state: "visible" });
  send("item/agentMessage/delta", { itemId: "answer", delta: "Answer" });
  await notice.waitFor({ state: "hidden" });
  send("item/reasoning/textDelta", { itemId: "thought-3", contentIndex: 0, delta: "Another thought" });
  await notice.waitFor({ state: "visible" });
  send("error", { message: "Retry", willRetry: true });
  await notice.waitFor({ state: "hidden" });
  send("item/reasoning/textDelta", { itemId: "thought-3", contentIndex: 0, delta: "Resumed" });
  await notice.waitFor({ state: "visible" });
  thread.activity = { state: "interrupted", turnId };
  send("turn/completed", { turn: { id: turnId, status: "interrupted" } });
  await notice.waitFor({ state: "hidden" });
  send("item/reasoning/textDelta", { itemId: "late", contentIndex: 0, delta: "Late event" });
  assert.equal(await notice.isVisible(), false, "late deltas cannot revive a finished turn");
  await begin(); acknowledge(); acknowledge = null;
  send("item/reasoning/textDelta", { itemId: "disconnect", contentIndex: 0, delta: "Thinking" });
  await notice.waitFor({ state: "visible" });
  stream.close({ code: 1011, reason: "fixture disconnect" });
  await page.waitForFunction(() => !document.querySelector("#conversationActivityText").textContent.includes("正在思考"));
  assert.deepEqual(errors, []);
  console.log("PASS: visible thinking before completion/answer/ack, empty prefill state, drafts, collapsed thoughts, mobile, tools, retry, cancellation, late events and disconnect");
} finally { await browser.close(); }

async function assertEventually(predicate) {
  for (let n = 0; n < 100; n++) { if (predicate()) return; await new Promise(resolve => setTimeout(resolve, 50)); }
  assert.fail("Expected turn/start request");
}

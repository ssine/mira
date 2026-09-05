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
  ["/thread-title.js", ["public/thread-title.js", "text/javascript"]],
  ["/account-status.js", ["public/account-status.js", "text/javascript"]],
  ["/trace-activity.js", ["public/trace-activity.js", "text/javascript"]],
  ["/trace-images.js", ["public/trace-images.js", "text/javascript"]],
  ["/conversation-progress.js", ["public/conversation-progress.js", "text/javascript"]],
  ["/theme.js", ["public/theme.js", "text/javascript"]],
  ["/pwa.js", ["public/pwa.js", "text/javascript"]],
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
        loadAgentTranscript, loadAgentThreads, resetAgentTranscript, closeAgentSocket, syncActiveTurnUi, restoreAgentThread, refreshActiveTurn,
        upsertTrace, replyProgress, renderReplyProgress, prepareTurnInput, renderLocalSessions,
        show, renderNodes, renderEnrollments, renderAudit,
        message: onAgentSocketMessage, nodes: dashboardNodes, connectAgentSocket, resumeAgentThread, recoverAgentSession, stopAgentRecovery, mergeTranscriptItems,
        setInvoke: (fn) => { invokeNode = fn; }, clear: () => clear($("#conversationTrace")) };
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
  const reconnectMessages = [];
  let reconnectedSocket;
  let automaticResume = false;
  let ignoreProbe = false;
  let socketCount = 0;
  let nodeRequests = 0;
  let expireLogin = false;
  let transcriptRequests = 0;
  let missingTranscript = false;
  let deferThreadLists = false;
  const pendingThreadLists = [];
  let threadListData = [];
  await page.route("**/v1/codex/threads?*", route => {
    if (deferThreadLists) pendingThreadLists.push(route);
    else return route.fulfill({ json: { data: threadListData } });
  });
  await page.route("**/v1/nodes/test-node", (route) => {
    nodeRequests++;
    return route.fulfill({ status: expireLogin ? 401 : 200, json: expireLogin
      ? { error: "login expired" }
      : { nodeId: "test-node", hostname: "Test runtime", reportedAppServer: { status: "running" } } });
  });
  await page.route("**/v1/codex/threads/*/transcript?*", (route) => {
    transcriptRequests++;
    const url = new URL(route.request().url());
    assert.equal(url.searchParams.get("tail"), "1");
    const threadId = url.pathname.split("/").at(-2);
    if (missingTranscript) return route.fulfill({ status: 404, json: { error: "thread was deleted" } });
    return route.fulfill({ json: { generation: 1, nextCursor: null, trace: [{
      key: `recovered-${threadId}`, kind: "assistant", body: `Recent messages for ${threadId}`,
      sourceItemSeq: 99, turnId: `recovered-turn-${threadId}`,
    }] } });
  });
  await page.route(/\/v1\/codex\/threads\/[^/?]+\?storeId=personal$/, (route) => route.fulfill({ json: {
    threadId: new URL(route.request().url()).pathname.split("/").at(-1), cwd: "/project", runtimeNodeId: "test-node",
  } }));
  await page.routeWebSocket(/\/v1\/nodes\/test-node\/app-server\?storeId=personal$/, (socket) => {
    reconnectedSocket = socket;
    socketCount++;
    socket.onMessage((data) => {
      const message = JSON.parse(data);
      if (message.method === "initialize") {
        socket.send(JSON.stringify({ id: message.id, result: {} }));
      } else if (message.method === "thread/turns/list") {
        socket.send(JSON.stringify({ id: message.id, result: { data: [] } }));
      } else if (message.id !== undefined) {
        reconnectMessages.push(message);
        if (message.method === "thread/resume" && automaticResume) {
          socket.send(JSON.stringify({ id: message.id, result: { thread: { id: message.params.threadId }, cwd: "/project" } }));
        }
        if (message.method === "thread/loaded/list" && !ignoreProbe) {
          socket.send(JSON.stringify({ id: message.id, result: { data: [], nextCursor: null } }));
        }
      }
    });
  });
  await page.goto(`http://127.0.0.1:${server.address().port}/`);
  await page.waitForFunction(() => !!window.traceHarness);
  assert.equal(await page.locator("html").getAttribute("data-theme"), "light");
  assert.equal(await page.locator("#globalAgent").getAttribute("aria-current"), "page");
  assert.equal(await page.locator("#agentThreadDrawer").getAttribute("aria-hidden"), "false", "wide chat opens its sidebar by default");
  assert.equal(await page.locator("#agentThreadDrawer").evaluate((node) => node.inert), false);
  const sidebar = await page.locator("#agentThreadDrawer").boundingBox();
  const conversation = await page.locator(".conversation-card").boundingBox();
  assert.ok(sidebar.x + sidebar.width <= conversation.x, "wide sidebar must not cover the conversation");
  assert.equal(await page.locator("#agentThreadDrawerBackdrop").isVisible(), false);
  assert.equal(await page.locator("#agentRuntimeNode").evaluate((node) => node.closest("section[id]")?.id), "runtimeView",
    "runtime selection belongs to its own management page");
  assert.equal(await page.locator("#sessionSourceNode").evaluate((node) => node.closest("section[id]")?.id), "runtimeView",
    "session import belongs to its own management page");
  await page.locator("#agentThreadDrawerToggle").click();
  assert.equal(await page.locator("#agentThreadDrawer").getAttribute("aria-hidden"), "true");
  await page.locator("#agentThreadDrawerToggle").click();
  assert.equal(await page.locator("#agentThreadDrawer").getAttribute("aria-hidden"), "false");
  assert.equal(await page.locator("#agentThreadDrawerToggle").getAttribute("aria-expanded"), "true");
  await page.keyboard.press("Escape");
  assert.equal(await page.locator("#agentThreadDrawer").getAttribute("aria-hidden"), "true");
  await page.setViewportSize({ width: 1099, height: 1100 });
  await page.locator("#agentThreadDrawerToggle").click();
  assert.equal(await page.locator("#agentThreadDrawerBackdrop").getAttribute("tabindex"), "0");
  await page.locator("#agentThreadDrawerClose").click();
  await page.locator("#conversationInput").focus();
  await page.setViewportSize({ width: 1440, height: 1100 });
  await page.waitForFunction(() => document.querySelector("#agentThreadDrawer").getAttribute("aria-hidden") === "false");
  assert.equal(await page.locator("#conversationInput").evaluate((node) => node === document.activeElement), true, "resizing must not steal input focus");
  await page.locator("#agentNewThread").click();
  assert.equal(await page.locator("#agentThreadDrawer").getAttribute("aria-hidden"), "false", "wide sidebar stays open after choosing a new conversation");
  await page.locator("#agentThemeToggle").click();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "dark");
  assert.equal(await page.evaluate(() => localStorage.getItem("mira.theme")), "dark");
  await page.locator("#agentThemeToggle").click();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "light");
  assert.equal(await page.locator("#conversationCwd").getAttribute("type"), "hidden", "thread cwd must not look editable per message");
  assert.equal(await page.locator("#conversationInput").getAttribute("rows"), "1");
  const composerStyle = () => page.locator("#conversationDropZone").evaluate((node) => {
    const style = getComputedStyle(node);
    return { border: style.borderColor, shadow: style.boxShadow };
  });
  await page.locator("#conversationDropZone").evaluate(async (node) => {
    await Promise.all(node.getAnimations().map((animation) => animation.finished));
  });
  const unfocusedComposer = await composerStyle();
  await page.locator("#conversationInput").focus();
  assert.deepEqual(await composerStyle(), unfocusedComposer, "focusing the composer must not highlight its border");
  assert.equal(await page.locator("#conversationInput").evaluate((node) => getComputedStyle(node).outlineStyle), "none");
  const singleLineHeight = await page.locator("#conversationInput").evaluate((node) => node.getBoundingClientRect().height);
  await page.locator("#conversationInput").fill(Array.from({ length: 12 }, (_, index) => `Line ${index}`).join("\n"));
  const expandedInput = await page.locator("#conversationInput").evaluate((node) => ({
    height: node.getBoundingClientRect().height, overflow: getComputedStyle(node).overflowY,
  }));
  assert.ok(expandedInput.height > singleLineHeight && expandedInput.height <= 144);
  assert.equal(expandedInput.overflow, "auto", "long input scrolls after reaching its height cap");
  assert.equal(await page.locator("#conversationAttach svg").count(), 1);
  assert.equal(await page.locator("#conversationSend svg").count(), 1);
  await page.locator("#conversationInput").fill("");
  await page.evaluate(() => {
    const h = window.traceHarness;
    document.querySelector("#agentRuntimeNode").append(new Option("Test runtime", "test-node"));
    document.querySelector("#conversationSend").disabled = false;
    h.agent.socketNodeId = "test-node";
    h.agent.socketInitialized = true;
    window.rpcMessages = [];
    h.agent.socket = { readyState: 1, send: (message) => window.rpcMessages.push(JSON.parse(message)) };
    document.querySelector("#conversationInput").value = "Waiting hint test";
  });
  await page.locator("#conversationInput").focus();
  await page.keyboard.press("End");
  await page.keyboard.press("Shift+Enter");
  assert.equal(await page.locator("#conversationInput").inputValue(), "Waiting hint test\n", "Shift+Enter inserts a newline");
  await page.locator("#conversationInput").evaluate((node) => {
    node.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", isComposing: true, bubbles: true, cancelable: true }));
    node.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", keyCode: 229, bubbles: true, cancelable: true }));
    node.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", repeat: true, bubbles: true, cancelable: true }));
  });
  assert.equal(await page.evaluate(() => window.rpcMessages.length), 0, "IME confirmation and key repeats must not submit");
  await page.keyboard.press("Enter");
  assert.equal(await page.locator("#conversationProgress").isVisible(), true, "hint must appear before thread/start returns");
  assert.match(await page.locator("#conversationProgressText").textContent(), /创建/);
  await page.keyboard.press("Enter");
  assert.equal(await page.evaluate(() => window.rpcMessages.filter((m) => m.method === "thread/start").length), 1, "Enter while sending stays single-flight");
  assert.equal(await page.locator('[data-thread-row]').count(), 0, 'unacknowledged creation has no phantom conversation');
  await page.evaluate(() => {
    const request = window.rpcMessages.find((m) => m.method === "thread/start");
    window.traceHarness.message({ data: JSON.stringify({ id: request.id, error: { message: 'Temporary creation failure' } }) });
  });
  await page.waitForFunction(() => !window.traceHarness.agent.sendPromise);
  assert.equal(await page.locator('[data-thread-row]').count(), 0, 'failed creation has no phantom conversation');
  assert.equal(await page.locator('#conversationInput').inputValue(), 'Waiting hint test\n', 'creation failure preserves the draft');
  await page.keyboard.press('Enter');
  await page.waitForFunction(() => window.rpcMessages.filter(m => m.method === 'thread/start').length === 2);
  assert.equal(await page.evaluate(() => new Set(window.rpcMessages.filter(m => m.method === 'thread/start').map(m => m.params.miraRequestId)).size), 1, 'creation retry reuses its request ID');
  await page.evaluate(() => {
    const request = window.rpcMessages.filter((m) => m.method === "thread/start").at(-1);
    window.traceHarness.message({ data: JSON.stringify({ id: request.id, result: { thread: { id: "progress-thread" }, cwd: "/project" } }) });
  });
  await page.waitForFunction(() => window.rpcMessages.some((m) => m.method === "turn/start"));
  const createdRow = page.locator('[data-thread-row="progress-thread"]');
  assert.equal(await createdRow.count(), 1, 'new conversation appears before turn/start acknowledgement');
  assert.equal(await createdRow.locator('[aria-current="page"]').count(), 1);
  assert.equal(await createdRow.locator('strong').textContent(), 'Waiting hint test');
  assert.equal(await createdRow.evaluate(e => e.closest('.thread-project').dataset.projectKey), JSON.stringify(['test-node', '/project']));
  await page.evaluate(() => window.traceHarness.loadAgentThreads());
  assert.equal(await createdRow.count(), 1, 'an empty list response cannot erase an acknowledged creation');
  assert.equal(await page.locator('.thread-project-count').textContent(), '1 对话');
  deferThreadLists = true;
  await page.evaluate(() => { window.olderList = window.traceHarness.loadAgentThreads(); });
  await page.waitForFunction(() => window.traceHarness.agent.threadListRequest >= 3);
  await page.evaluate(() => { window.newerList = window.traceHarness.loadAgentThreads(); });
  for (let attempt = 0; pendingThreadLists.length < 2 && attempt < 100; attempt++) await new Promise(resolve => setTimeout(resolve, 20));
  assert.equal(pendingThreadLists.length, 2, 'both refresh requests are held before delivering them out of order');
  threadListData = [{ threadId: 'progress-thread', title: 'Canonical conversation title', cwd: '/project', runtimeNodeId: 'test-node', updatedAt: new Date().toISOString() }];
  await pendingThreadLists[1].fulfill({ json: { data: threadListData } });
  await page.evaluate(() => window.newerList);
  await pendingThreadLists[0].fulfill({ json: { data: [] } });
  await page.evaluate(() => window.olderList);
  deferThreadLists = false;
  assert.equal(await createdRow.count(), 1, 'out-of-order list responses cannot remove the new conversation');
  assert.equal(await createdRow.locator('strong').textContent(), 'Canonical conversation title');
  assert.equal(await page.evaluate(() => window.traceHarness.agent.pendingThreadSummaries.size), 0, 'canonical metadata replaces the temporary preview');
  threadListData[0] = { ...threadListData[0], name: 'Native Codex title', title: 'Native Codex title' };
  await page.evaluate(() => window.traceHarness.notify({ method: 'thread/name/updated', params: { threadId: 'progress-thread', threadName: 'Older notification title' } }));
  await page.waitForFunction(() => document.title === 'Native Codex title · Mira');
  assert.equal(await createdRow.locator('strong').textContent(), 'Native Codex title', 'native title updates use the current canonical name, not a stale notification payload');
  await page.evaluate(() => {
    const h = window.traceHarness;
    h.notify({ method: "turn/started", params: { threadId: "progress-thread", turn: { id: "progress-turn" } } });
    h.notify({ method: "item/started", params: { threadId: "progress-thread", turnId: "progress-turn", item: { id: "empty", type: "reasoning", summary: [] } } });
  });
  assert.equal(await page.locator("#conversationProgress").isVisible(), true);
  assert.equal(await page.locator(".trace-card.reasoning").count(), 0);
  await page.evaluate(() => {
    const h = window.traceHarness;
    h.notify({ method: "item/agentMessage/delta", params: { threadId: "another-thread", turnId: "other", delta: "unrelated" } });
  });
  assert.equal(await page.locator("#conversationProgress").isVisible(), true, "another thread cannot clear progress");
  await page.evaluate(() => {
    const h = window.traceHarness;
    h.notify({ method: "item/agentMessage/delta", params: { threadId: "progress-thread", turnId: "progress-turn", itemId: "prose", delta: "First body" } });
    const request = window.rpcMessages.find((m) => m.method === "turn/start");
    h.message({ data: JSON.stringify({ id: request.id, result: { turn: { id: "progress-turn" } } }) });
  });
  assert.equal(await page.locator("#conversationProgress").isVisible(), false, "first prose clears hint even before turn/start acknowledgement");
  await page.waitForFunction(() => !window.traceHarness.agent.sendPromise);
  // A follow-up accepted into the same turn has no new turn/started event.
  await page.locator("#conversationInput").fill("Follow up in the active turn");
  await page.keyboard.press("Enter");
  await page.waitForFunction(() => window.rpcMessages.filter((m) => m.method === "turn/start").length === 2);
  await page.evaluate(() => {
    const h = window.traceHarness;
    const request = window.rpcMessages.filter((m) => m.method === "turn/start").at(-1);
    h.message({ data: JSON.stringify({ id: request.id, result: { turn: { id: "progress-turn" } } }) });
  });
  await page.waitForFunction(() => !window.traceHarness.agent.sendPromise);
  assert.equal(await page.locator("#conversationProgress").isVisible(), false, "acknowledgement ends submission even when the current turn continues");
  await page.locator('.trace-card.user').last().evaluate((node) => node.remove());
  await page.evaluate(() => window.traceHarness.notify({
    method: "item/completed",
    params: { threadId: "progress-thread", turnId: "progress-turn", item: { id: "prose", type: "agentMessage", text: "First body" } },
  }));
  assert.equal(await page.locator(".trace-card.assistant .trace-head").count(), 0, "Codex messages have no redundant heading");
  assert.equal(await page.locator(".trace-card.user .trace-head").count(), 0, "user identity is expressed by alignment and bubble only");
  assert.equal(await page.locator(".trace-card.user .trace-footer").count(), 0, "user messages need no elapsed time or copy action");
  assert.match(await page.locator(".trace-card.assistant .trace-completed").textContent(), /^\d{2}:\d{2}:\d{2}$/);
  assert.equal(await page.locator(".trace-card.assistant .trace-elapsed").isVisible(), false);
  assert.equal(await page.locator("#conversationActivity").isVisible(), true, "the turn stays visibly active after prose completes");
  assert.equal(await page.locator("#conversationActivityText").textContent(), "Codex 仍在处理中…");
  assert.equal(await page.locator(".trace-card.assistant .trace-footer .trace-copy svg").count(), 1);
  assert.equal(await page.locator(".trace-card.assistant .trace-footer .trace-copy").getAttribute("aria-label"), "复制这条消息的原文");

  // Reconnect to a fresh App Server while keeping the selected conversation.
  // Only thread/resume loads the old ID; turn/start cannot restore it itself.
  await page.evaluate(async () => {
    const h = window.traceHarness;
    h.agent.socket.readyState = 3;
    h.agent.threads = [{ threadId: "progress-thread", cwd: "/project", runtimeNodeId: "test-node" }];
    h.agent.resumeRequestedThreadId = null;
    await h.connectAgentSocket("test-node");
  });
  await page.locator("#conversationInput").fill("Continue after restart");
  await page.waitForFunction(() => window.traceHarness.agent.resumePromises.has('progress-thread'));
  assert.equal(await page.evaluate(()=>window.traceHarness.agent.sendPromise),null,'editing prepares the conversation before sending');
  await page.locator("#conversationInput").press("Enter");
  await page.waitForFunction(() => window.traceHarness.agent.pending.size === 1);
  // A second submit while resume is pending must remain single-flight.
  await page.locator("#conversationForm").evaluate((form) => form.requestSubmit());
  assert.deepEqual(reconnectMessages.map((message) => message.method), ["thread/resume"]);
  assert.deepEqual(reconnectMessages[0].params, { threadId: "progress-thread", cwd: "/project", excludeTurns: true });
  assert.match(await page.locator("#conversationProgressText").textContent(), /恢复/);
  assert.equal(await page.locator(".trace-card.user").count(), 1, "resume must precede the optimistic user message");
  reconnectedSocket.send(JSON.stringify({ id: reconnectMessages[0].id, error: { code: -32600, message: "resume unavailable" } }));
  await page.waitForFunction(() => !window.traceHarness.agent.sendPromise);
  assert.equal(await page.locator("#conversationInput").inputValue(), "Continue after restart");
  assert.equal(await page.locator("#conversationNotice").textContent(), "resume unavailable");
  assert.equal(await page.locator("#conversationProgress").isVisible(), false);
  assert.equal(reconnectMessages.length, 1, "failed resume must not send a turn or create a replacement thread");

  await page.locator("#conversationInput").press("Enter");
  await page.waitForFunction(() => window.traceHarness.agent.pending.size === 1);
  assert.equal(reconnectMessages[1].method, "thread/resume", "failed restoration must remain retryable");
  reconnectedSocket.send(JSON.stringify({ id: reconnectMessages[1].id, result: { thread: { id: "progress-thread" }, cwd: "/project" } }));
  await page.waitForFunction(() => document.querySelectorAll(".trace-card.user").length === 2);
  assert.deepEqual(reconnectMessages.map((message) => message.method), ["thread/resume", "thread/resume", "turn/start"]);
  assert.equal(reconnectMessages[2].params.threadId, "progress-thread");
  assert.equal((await page.locator(".trace-card.assistant .trace-body").textContent()).trim(), "First body", "restoration preserves visible history");
  reconnectedSocket.send(JSON.stringify({ id: reconnectMessages[2].id, result: { turn: { id: "reconnected-turn" } } }));
  await page.waitForFunction(() => !window.traceHarness.agent.sendPromise);
  assert.equal(await page.locator("#conversationInput").inputValue(), "");

  await page.locator("#conversationInput").fill("Another message on the same connection");
  await page.locator("#conversationInput").press("Enter");
  await page.waitForFunction(() => document.querySelectorAll(".trace-card.user").length === 3);
  assert.equal(reconnectMessages[3].method, "turn/start", "a loaded conversation must not be resumed before every message");
  reconnectedSocket.send(JSON.stringify({ id: reconnectMessages[3].id, result: { turn: { id: "next-turn" } } }));
  await page.waitForFunction(() => !window.traceHarness.agent.sendPromise);

  automaticResume = true;
  const sendsBeforeRecovery = reconnectMessages.filter((message) => message.method === "turn/start").length;
  await page.locator("#conversationInput").fill("Draft stays through mobile suspension");
  reconnectedSocket.close({ code: 1001, reason: "mobile radio suspended" });
  await page.waitForFunction(() => window.traceHarness.agent.socketInitialized &&
    window.traceHarness.agent.loadedThreadIds.has("progress-thread") && !window.traceHarness.agent.recoveryPromise);
  assert.equal(socketCount, 2, "closed sockets reconnect automatically");
  assert.ok(nodeRequests > 0 && transcriptRequests > 0);
  assert.match(await page.locator("#conversationTrace").textContent(), /Recent messages for progress-thread/,
    "recovery must backfill notifications missed while asleep");
  assert.equal(await page.locator("#conversationInput").inputValue(), "Draft stays through mobile suspension");

  // An OPEN browser socket with no reply is also recoverable on foreground.
  ignoreProbe = true;
  await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true })));
  await page.waitForFunction(() => window.traceHarness.agent.recoveryPromise !== null);
  await page.waitForFunction(() => window.traceHarness.agent.socketInitialized &&
    window.traceHarness.agent.loadedThreadIds.has("progress-thread") && !window.traceHarness.agent.recoveryPromise,
  null, { timeout: 15_000 });
  ignoreProbe = false;
  assert.equal(socketCount, 3, "foreground probe replaces a half-open connection");
  assert.equal(reconnectMessages.filter((message) => message.method === "turn/start").length, sendsBeforeRecovery,
    "reconnect must never replay an uncertain user submission");

  // Browsing and focusing only read the transcript. Actual edits start one
  // shared resume; a late result cannot replace a subsequently selected thread.
  automaticResume = false;
  const resumesBeforeBrowsing = reconnectMessages.filter(message=>message.method==='thread/resume').length;
  await page.evaluate(() => {
    const h = window.traceHarness;
    h.agent.threads.push({ threadId: "fast-thread", title: "Fast thread", cwd: "/fast" });
    window.openRecent = h.resumeAgentThread("fast-thread");
  });
  await page.waitForFunction(() => document.querySelector("#conversationTrace").textContent.includes("Recent messages for fast-thread"));
  assert.equal(await page.evaluate(() => window.traceHarness.agent.loadedThreadIds.has("fast-thread")), false);
  await page.locator('#conversationInput').focus();
  await page.locator('#conversationInput').press('ArrowLeft');
  await page.locator('#conversationInput').evaluate(node=>node.dispatchEvent(new InputEvent('input',{bubbles:true})));
  await page.evaluate(()=>window.traceHarness.recoverAgentSession({refresh:false}));
  assert.equal(reconnectMessages.filter(message=>message.method==='thread/resume').length,resumesBeforeBrowsing);
  await page.locator('#conversationInput').press('x');
  await page.waitForFunction(()=>window.traceHarness.agent.resumePromises.has('fast-thread'));
  const slowResume = reconnectMessages.findLast((message) => message.method === "thread/resume");
  assert.equal(slowResume.params.threadId,'fast-thread');
  await page.locator('#conversationInput').press('y');
  assert.equal(reconnectMessages.filter(message=>message.method==='thread/resume').length,resumesBeforeBrowsing+1,'typing more characters shares the pending resume');
  await page.evaluate(() => { window.openOther = window.traceHarness.resumeAgentThread("other-thread"); });
  await page.waitForFunction(() => document.querySelector("#conversationTrace").textContent.includes("Recent messages for other-thread"));
  await page.locator('#conversationInput').focus();
  await page.evaluate(()=>window.traceHarness.recoverAgentSession({refresh:false}));
  assert.equal(reconnectMessages.filter(message=>message.method==='thread/resume').length,resumesBeforeBrowsing+1,'an existing draft does not resume a newly opened conversation');
  await page.locator('#conversationInput').evaluate(node=>{
    node.value='Draft stays through mobile suspension中';
    node.dispatchEvent(new InputEvent('input',{inputType:'insertCompositionText',data:'中',isComposing:true,bubbles:true}));
  });
  await page.waitForFunction(()=>window.traceHarness.agent.resumePromises.has('other-thread'));
  const otherResume = reconnectMessages.findLast((message) => message.method === "thread/resume");
  assert.notEqual(slowResume.id, otherResume.id);
  reconnectedSocket.send(JSON.stringify({ id: slowResume.id, result: { thread: { id: "fast-thread", name: "STALE TITLE" }, cwd: "/fast" } }));
  reconnectedSocket.send(JSON.stringify({ id: otherResume.id, result: { thread: { id: "other-thread", name: "Current thread" }, cwd: "/other", model: "gpt-6-astra" } }));
  await page.waitForFunction(() => document.querySelector("#conversationTitle").textContent === "Current thread");
  assert.equal(await page.evaluate(() => window.traceHarness.agent.threadId), "other-thread");
  assert.equal(await page.locator("#conversationMeta .conversation-directory").textContent(), "/other");
  assert.equal(await page.locator("#conversationMeta .conversation-model").textContent(), "gpt-6-astra");
  assert.equal((await page.locator("#conversationMeta").textContent()).includes("other-thread"), false);

  // A healthy socket plus a failed resume is a conversation error, not an
  // offline runtime. Keep its actual error visible and preserve the draft.
  await page.evaluate(() => {
    const h = window.traceHarness;
    h.agent.loadedThreadIds.delete("other-thread");
    window.failedRecovery = h.recoverAgentSession({ refresh: false });
  });
  await page.waitForFunction(() => window.traceHarness.agent.pending.size === 1);
  const failedResume = reconnectMessages.findLast(message => message.method === "thread/resume");
  reconnectedSocket.send(JSON.stringify({ id: failedResume.id, error: { code: -32600, message: "resume unavailable" } }));
  await page.evaluate(() => window.failedRecovery);
  assert.equal(await page.locator("#agentRuntimeBadge").textContent(), "online");
  assert.equal(await page.locator("#conversationConnection").isVisible(), false);
  assert.equal(await page.locator("#conversationNotice").textContent(), "恢复会话失败：resume unavailable");
  assert.equal(await page.locator("#conversationInput").inputValue(), "Draft stays through mobile suspension中");
  automaticResume = true;
  await page.evaluate(() => window.traceHarness.recoverAgentSession({ refresh: false }));
  assert.equal(await page.locator("#conversationNotice").textContent(), "", "a recovered conversation clears its recovery error");

  automaticResume = false;
  await page.evaluate(() => {
    const h = window.traceHarness;
    h.agent.loadedThreadIds.delete("other-thread");
    window.deletedRecovery = h.recoverAgentSession({ refresh: false });
  });
  await page.waitForFunction(() => window.traceHarness.agent.pending.size === 1);
  const deletedResume = reconnectMessages.findLast(message => message.method === "thread/resume");
  reconnectedSocket.send(JSON.stringify({ id: deletedResume.id, error: { code: -32004, message: "此会话已永久删除，不能继续写入或恢复。" } }));
  await page.evaluate(() => window.deletedRecovery);
  assert.equal(await page.evaluate(() => window.traceHarness.agent.connectionWanted), false, "broker deletion errors stop automatic retries too");
  assert.match(await page.locator("#conversationNotice").textContent(), /此会话已永久删除/);
  automaticResume = true;
  await page.evaluate(async () => {
    const h = window.traceHarness;
    h.agent.connectionWanted = true;
    await h.connectAgentSocket("test-node");
  });

  // A deleted thread's transcript cannot recover; don't reconnect forever or
  // mislabel the healthy service as offline after deletion committed.
  missingTranscript = true;
  await page.evaluate(() => window.traceHarness.recoverAgentSession());
  assert.equal(await page.evaluate(() => window.traceHarness.agent.connectionWanted), false);
  assert.match(await page.locator("#conversationNotice").textContent(), /会话已删除或不可访问.*thread was deleted/);
  missingTranscript = false;
  await page.evaluate(async () => {
    const h = window.traceHarness;
    h.agent.connectionWanted = true;
    await h.connectAgentSocket("test-node");
  });

  expireLogin = true;
  reconnectedSocket.close({ code: 1001 });
  await page.waitForFunction(() => !window.traceHarness.agent.connectionWanted);
  assert.match(await page.locator("#conversationNotice").textContent(), /登录已过期/);
  const stoppedRequests = nodeRequests;
  await page.evaluate(() => window.dispatchEvent(new Event("online")));
  assert.equal(nodeRequests, stoppedRequests, "expired sessions must stop automatic retries");
  await page.locator("#conversationInput").fill("");

  const fragmentMerge = await page.evaluate(() => window.traceHarness.mergeTranscriptItems([
    { key: "tool-a", kind: "tool", turnId: "turn-a", sourceItemSeq: 250, title: "工具输出", body: "输出\nresult", toolFragment: { input: null, output: "result" } },
  ], [
    { key: "tool-a", kind: "tool", turnId: "turn-a", sourceItemSeq: 5, title: "functions.exec", body: "输入\ncommand", toolFragment: { input: "command", output: null } },
  ]));
  assert.equal(fragmentMerge.length, 1);
  assert.equal(fragmentMerge[0].sourceItemSeq, 5);
  assert.equal(fragmentMerge[0].body, "输入\ncommand\n\n输出\nresult");
  assert.equal(fragmentMerge[0].title, "functions.exec");

  const glass = await page.locator("#agentThreadDrawerToggle").evaluate((head) => {
    const style = getComputedStyle(head);
    const trace = document.querySelector("#conversationScroll");
    return { blur: style.backdropFilter, background: style.backgroundColor,
      headTop: head.getBoundingClientRect().top, traceTop: trace.getBoundingClientRect().top,
      padding: parseFloat(getComputedStyle(trace).paddingTop), height: head.getBoundingClientRect().height };
  });
  assert.match(glass.blur, /blur\(2px\)/);
  assert.match(glass.background, /rgba/);
  assert.ok(glass.headTop >= glass.traceTop, "navigation floats over the reading surface");
  assert.ok(glass.padding > glass.height + glass.headTop - glass.traceTop, "initial messages must clear the navigation button");

  const streamBenchmark = await page.evaluate(async () => {
    const h = window.traceHarness;
    h.clear();
    h.agent.threadId = "stream-benchmark";
    const chunk = "A short **streamed** sentence with `code`.\n";
    const startedAt = performance.now();
    for (let index = 0; index < 1_200; index += 1) {
      h.notify({ method: "item/agentMessage/delta", params: {
        threadId: "stream-benchmark", turnId: "stream-turn", itemId: "stream-item", delta: chunk,
      } });
    }
    const dispatchMs = performance.now() - startedAt;
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    const body = document.querySelector(".trace-card.assistant .trace-body");
    return {
      dispatchMs,
      sourceLength: body?._miraSource?.length ?? 0,
      renderedLength: body?.textContent?.length ?? 0,
      markdownDuringStream: body?.classList.contains("markdown-body") ?? false,
    };
  });
  console.log(`TRACE_STREAM_BENCHMARK dispatch=${streamBenchmark.dispatchMs.toFixed(1)}ms bytes=${streamBenchmark.sourceLength}`);
  assert.equal(streamBenchmark.sourceLength, 1_200 * "A short **streamed** sentence with `code`.\n".length);
  assert.equal(streamBenchmark.renderedLength, streamBenchmark.sourceLength);
  assert.equal(streamBenchmark.markdownDuringStream, false, "live deltas use the lossless lightweight renderer");
  assert.ok(streamBenchmark.dispatchMs < 1_000,
    `streaming 1,200 deltas blocked the browser for ${streamBenchmark.dispatchMs.toFixed(1)}ms`);
  const incremental = await page.evaluate(async () => {
    const h = window.traceHarness;
    const body = document.querySelector(".trace-card.assistant .trace-body");
    const textNode = body.firstChild;
    const started = performance.now();
    h.notify({ method: "item/agentMessage/delta", params: {
      threadId: "stream-benchmark", turnId: "stream-turn", itemId: "stream-item", delta: "New words immediately" } });
    const immediate = body.textContent.endsWith("New words immediately");
    await new Promise((resolve) => requestAnimationFrame(resolve));
    return { immediate, sameTextNode: textNode === body.firstChild, tail: body.textContent.endsWith("New words immediately"), latencyMs: performance.now() - started };
  });
  assert.equal(incremental.immediate, true, "received text enters the DOM in the same message task");
  assert.equal(incremental.sameTextNode, true, "new words append without replacing already-rendered text");
  assert.equal(incremental.tail, true);
  assert.ok(incremental.latencyMs < 250, "visible output must not wait for a typing timer");
  console.log(`TRACE_PAINT_BENCHMARK append latency=${incremental.latencyMs.toFixed(1)}ms`);
  if (process.env.MIRA_STREAM_CAPTURE) {
    // Replay real App Server payloads at 20x recorded speed with a long reading
    // surface and a throttled mobile CPU. No model, credentials or production
    // mutations are involved in this rendering regression.
    const capture = JSON.parse(await fs.readFile(process.env.MIRA_STREAM_CAPTURE, "utf8"));
    const deltas = capture.capture.filter((entry) => entry.stage === "proxy" && entry.message.method === "item/agentMessage/delta");
    assert(deltas.length > 100, "supply a real stream capture with at least 100 deltas");
    const cdp = await page.context().newCDPSession(page);
    await cdp.send("Emulation.setCPUThrottlingRate", { rate: 6 });
    await page.setViewportSize({ width: 390, height: 844 });
    const replay = await page.evaluate(async (events) => {
      const h = window.traceHarness;
      h.clear();
      for (let i = 0; i < 60; i++) h.upsertTrace(`replay-history-${i}`, "assistant", "Codex", "Earlier conversation paragraph.\n\n".repeat(20), "", { autoScroll: false });
      const first = events[0];
      h.agent.threadId = first.message.params.threadId;
      h.agent.turnId = first.message.params.turnId;
      const scroll = document.querySelector("#conversationScroll");
      scroll.scrollTop = scroll.scrollHeight;
      const received = [], frames = [];
      let running = true, characters = 0, source = "";
      const frame = () => {
        const body = document.querySelector(".trace-card.assistant:last-child .trace-body");
        frames.push({ at: performance.now(), characters: body?.textContent.length ?? 0 });
        if (running) requestAnimationFrame(frame);
      };
      requestAnimationFrame(frame);
      const started = performance.now();
      for (const event of events) {
        const wait = (event.at - first.at) / 20 - (performance.now() - started);
        if (wait > 0) await new Promise((resolve) => setTimeout(resolve, wait));
        const at = performance.now();
        h.message({ data: JSON.stringify(event.message) });
        source += event.message.params.delta;
        characters += event.message.params.delta.length;
        received.push({ at, characters });
      }
      await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      running = false;
      const body = document.querySelector(".trace-card.assistant:last-child .trace-body");
      const lags = received.map((event) => frames.find((frame) => frame.at >= event.at && frame.characters >= event.characters)?.at - event.at).sort((a, b) => a - b);
      return { deltas: received.length, characters, lossless: body?._miraSource === source && body?.textContent === source,
        p95Ms: lags[Math.floor(lags.length * .95)], maxMs: lags.at(-1) };
    }, deltas);
    await cdp.send("Emulation.setCPUThrottlingRate", { rate: 1 });
    await page.setViewportSize({ width: 1440, height: 1100 });
    assert.equal(replay.lossless, true);
    assert(replay.p95Ms < 100 && replay.maxMs < 250, "real stream replay must keep up under mobile CPU load");
    console.log(`REAL_STREAM_REPLAY ${JSON.stringify(replay)}`);
    // Restore the deterministic stream used by the remaining Markdown checks.
    await page.evaluate(() => {
      const h = window.traceHarness; h.clear(); h.agent.threadId = "stream-benchmark";
      h.notify({ method: "item/agentMessage/delta", params: { threadId: "stream-benchmark", turnId: "stream-turn", itemId: "stream-item",
        delta: "A short **streamed** sentence with `code`.\n".repeat(1200) } });
    });
  }
  await page.evaluate(() => {
    const h = window.traceHarness;
    const body = document.querySelector(".trace-card.assistant .trace-body");
    h.notify({ method: "item/completed", params: {
      threadId: "stream-benchmark", turnId: "stream-turn",
      item: { id: "stream-item", type: "agentMessage", text: body._miraSource, status: "completed" },
    } });
  });
  assert.equal(await page.locator(".trace-card.assistant .trace-body").evaluate((node) => node.classList.contains("markdown-body")), true,
    "completed messages receive the full Markdown renderer");
  assert.equal(await page.locator(".trace-card.assistant strong").count(), 1_200);
  await page.evaluate(() => {
    const h = window.traceHarness;
    h.notify({ method: "item/completed", params: { threadId: "stream-benchmark", turnId: "stream-turn",
      item: { id: "compact", type: "contextCompaction", status: "completed" } } });
    h.agent.transcriptItems = [{ key: "stored-message", kind: "assistant", body: "Stored response", turnId: "stream-turn" }];
    h.renderTranscript();
  });
  assert.equal(await page.locator(".trace-card.compaction").count(), 1, "a lagging history refresh retains the live compaction notice");
  assert.equal(await page.locator(".trace-card.compaction .trace-head, .trace-card.compaction .trace-copy").count(), 0);
  await page.evaluate((compaction) => {
    const h = window.traceHarness;
    h.agent.transcriptItems.push({ ...compaction, turnId: "stream-turn" });
    h.renderTranscript();
  }, projectCodexTranscript([{ type: "compacted", payload: { message: "Internal summary" } }])[0]);
  assert.equal(await page.locator(".trace-card.compaction").count(), 1, "durable compaction replaces the live notice without duplication");
  assert.ok(await page.locator(".trace-card.compaction").evaluate((node) => node.getBoundingClientRect().height < 18), "compaction stays a compact single line");
  const turnFooters = await page.evaluate(() => {
    const h = window.traceHarness;
    h.clear(); h.agent.threadId = null;
    h.agent.turnTimings.set("timed", { startedAt: Date.now() - 20000 });
    for (const [id, clock] of [["first", "2026-09-05T10:00:02.000Z"], ["last", "2026-09-05T10:00:08.000Z"]]) {
      h.notify({ method: "item/completed", params: { turnId: "timed", item: { id, type: "agentMessage", text: id, completedAt: clock } } });
    }
    const clocks = () => [...document.querySelectorAll(".trace-completed")].map((node) => node.textContent);
    const totals = () => [...document.querySelectorAll(".trace-elapsed:not([hidden])")].map((node) => ({ text: node.textContent, body: node.closest(".trace-card").querySelector(".trace-body")._miraSource }));
    const before = clocks(); const running = totals();
    h.notify({ method: "turn/completed", params: { turn: { id: "timed", status: "completed", durationMs: 12542 } } });
    const completed = totals(); const after = clocks();
    h.agent.turnTimings.clear();
    h.agent.transcriptItems = ["first", "last"].map((body, i) => ({ key: body, kind: "assistant", turnId: "timed", body,
      completedAt: "2026-09-05T10:00:12.000Z", timingScope: "turn", elapsedMs: 12542, sourceItemSeq: i + 1 }));
    h.renderTranscript();
    return { before, after, refreshed: clocks(), running, completed, recovered: totals() };
  });
  assert.deepEqual(turnFooters.running, []);
  assert.deepEqual(turnFooters.before, turnFooters.after, "turn completion preserves each paragraph's clock");
  assert.deepEqual(turnFooters.before, turnFooters.refreshed, "history refresh preserves precise live clocks");
  assert.equal(turnFooters.completed.length, 1);
  assert.equal(turnFooters.completed[0].body, "last");
  assert.match(turnFooters.completed[0].text, /^本轮总耗时 12\.5/);
  assert.deepEqual(turnFooters.recovered, turnFooters.completed, "reconnect keeps the total only on the final paragraph");
  const paragraphGap = await page.evaluate(() => {
    const card = window.traceHarness.upsertTrace("spacing", "assistant", "Codex", "First paragraph.\n\nSecond paragraph.", "");
    const [first, second] = card.querySelectorAll("p");
    return second.getBoundingClientRect().top - first.getBoundingClientRect().bottom;
  });
  assert.ok(paragraphGap >= 7 && paragraphGap <= 9, `Markdown block separators must not add blank text lines: ${paragraphGap}`);
  await page.evaluate(() => { window.traceHarness.agent.threadId = "progress-thread"; window.traceHarness.clear(); });

  const uploaded = await page.evaluate(async () => {
    const h = window.traceHarness;
    h.agent.socketNodeId = "test-node";
    h.nodes.set("test-node", { platform: "linux", capabilities: { files: true } });
    const calls = [];
    h.setInvoke(async (_node, _capability, params) => { calls.push({ ...params, content: params.content?.length }); return {}; });
    const file = new File([new Uint8Array(12 * 1024 * 1024 + 17)], "large.bin");
    const image = new File([new Uint8Array(5 * 1024 * 1024)], "large.png", { type: "image/png" });
    const result = await h.prepareTurnInput("attachments", [file, image]);
    return { calls, inputs: result.inputs };
  });
  assert.equal(uploaded.calls.filter((p) => p.action === "write").length, 6);
  assert(uploaded.calls.some((p) => p.append && p.offset === 12 * 1024 * 1024));
  assert(uploaded.inputs.some((p) => p.type === "localImage" && p.path.endsWith("large.png")));
  const cancellation = await page.evaluate(async () => {
    const h = window.traceHarness;
    const calls = [];
    h.setInvoke(async (_node, _capability, params) => {
      calls.push({ action: params.action, path: params.path });
      if (params.action === "write") h.agent.uploadController.abort();
      return {};
    });
    try { await h.prepareTurnInput("cancel", [new File([new Uint8Array(9 * 1024 * 1024)], "cancel.bin")]); }
    catch (error) { return { error: error.name, calls }; }
  });
  assert.equal(cancellation.error, "AbortError");
  assert.equal(cancellation.calls.filter((p) => p.action === "write").length, 1);
  assert.match(cancellation.calls.find((p) => p.action === "remove").path, /^\/tmp\/mira-web-uploads\/progress-thread\//);
  await page.evaluate(() => {
    const h = window.traceHarness;
    h.show("runtimeView");
    h.agent.sessions = Array.from({ length: 65 }, (_, index) => ({ threadId: `desktop-${index}`, title: `Desktop ${index}`, clientKind: "desktop", archived: index % 2 === 0, cwd: "C:\\project" }));
    h.agent.sessions.push({ threadId: "cli", title: "CLI", clientKind: "cli" });
    h.renderLocalSessions();
  });
  assert.equal(await page.locator(".local-session").count(), 40);
  await page.locator("#sessionShowMore").click();
  assert.equal(await page.locator(".local-session").count(), 66);
  await page.locator("#sessionSourceFilter").selectOption("desktop");
  await page.locator("#sessionArchiveFilter").selectOption("archived");
  assert.equal(await page.locator(".local-session").count(), 33);
  await page.locator("#sessionSearch").fill("desktop-2");
  assert.equal(await page.locator(".local-session").count(), 6);
  await page.evaluate(() => window.traceHarness.show("agentView"));
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
  const toolRow = await page.locator(".tool-group-summary").evaluate((node) => ({
    height: node.getBoundingClientRect().height,
    latestHeight: node.querySelector(".tool-group-latest").getBoundingClientRect().height,
  }));
  assert.ok(toolRow.height <= 36 && toolRow.latestHeight < 20, "collapsed tool activity occupies one line");
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
  });
  await page.locator("#conversationScroll").evaluate((trace) => { trace.scrollTop = 0; });
  await page.evaluate(() => window.traceHarness.upsertTrace("next-message", "assistant", "Codex", "Streaming update", ""));
  assert.equal(await page.locator("#conversationScroll").evaluate((node) => node.scrollTop), 0, "new activity must respect scrolling into history");
  await page.evaluate(() => {
    const h = window.traceHarness;
    const trace = document.querySelector("#conversationScroll");
    trace.scrollTop = trace.scrollHeight;
    const now = Date.now();
    h.upsertTrace("bottom-message", "assistant", "Codex", "Follow newest update", "", {
      completedAt: new Date(now - 5200).toISOString(), elapsedMs: 4800,
    });
    h.upsertTrace("preview-user", "user", "你", "把消息元信息放到正文下面。", "", {
      completedAt: new Date(now - 4300).toISOString(), elapsedMs: 180,
    });
    h.upsertTrace("preview-assistant", "assistant", "Codex", "已经调整：左侧正文不再显示身份标题，也不使用气泡。", "", {
      completedAt: new Date(now).toISOString(), elapsedMs: 4100,
    });
  });
  assert.ok(await page.locator("#conversationScroll").evaluate((node) => node.scrollHeight - node.clientHeight - node.scrollTop < 2));
  const flow = await page.evaluate(() => {
    const bounds = (selector) => { const r = document.querySelector(selector).getBoundingClientRect(); return { left: r.left, right: r.right, width: r.width, bottom: r.bottom }; };
    return { trace: bounds("#conversationTrace"), form: bounds("#conversationForm"), user: bounds('[data-trace-key="preview-user"]'), assistant: bounds('[data-trace-key="preview-assistant"]'),
      parent: document.querySelector("#conversationForm").parentElement.id,
      barBorder: getComputedStyle(document.querySelector("#conversationForm")).borderTopWidth };
  });
  assert.equal(flow.parent, "conversationComposer", "composer has its own anchored workspace row");
  assert.equal(flow.barBorder, "0px", "composer does not draw a full-width bottom bar");
  assert.ok(flow.trace.width <= 808 && Math.abs(flow.form.width - flow.trace.width) < 1);
  assert.ok(Math.abs(flow.user.right - flow.assistant.right) < 1, "user messages align right inside the same reading column");
  assert.ok(Math.abs(flow.form.left - flow.assistant.left) < 1, "composer and assistant share a column");
  const agentViewport = await page.locator(".chat-shell").evaluate((node) => ({
    bottom: node.getBoundingClientRect().bottom,
    viewportBottom: window.innerHeight,
    documentHeight: document.documentElement.scrollHeight,
    bodyOverflow: getComputedStyle(document.body).overflow,
  }));
  assert.ok(Math.abs(agentViewport.bottom - agentViewport.viewportBottom) <= 1,
    "desktop Agent workspace must reach the viewport bottom");
  assert.ok(agentViewport.documentHeight <= agentViewport.viewportBottom + 1,
    "dedicated conversation page must not leave document-level space below the viewport");
  assert.equal(agentViewport.bodyOverflow, "hidden", "only the transcript may scroll in conversation mode");
  for (const position of [0, 0.5, 1]) {
    const anchored = await page.locator("#conversationScroll").evaluate(async (scroll, position) => {
      scroll.scrollTop = (scroll.scrollHeight - scroll.clientHeight) * position;
      await new Promise(requestAnimationFrame);
      const form = document.querySelector("#conversationForm").getBoundingClientRect();
      const composer = document.querySelector("#conversationComposer").getBoundingClientRect();
      return { bottom: form.bottom, viewport: innerHeight, seam: Math.abs(scroll.getBoundingClientRect().bottom - composer.top) };
    }, position);
    assert.ok(Math.abs(anchored.bottom - anchored.viewport) <= 1, `composer remains at bottom at scroll ${position}`);
    assert.ok(anchored.seam <= 1, "no extra scrollable space below the composer");
  }

  if (process.env.MIRA_TRACE_WIDE_SCREENSHOT) {
    await page.screenshot({ path: process.env.MIRA_TRACE_WIDE_SCREENSHOT, fullPage: true });
  }
  if (process.env.MIRA_DASHBOARD_SCREENSHOT) {
    await page.evaluate(() => {
      const h = window.traceHarness;
      h.show("dashboardView");
      document.querySelector("#onlineCount").textContent = "3";
      document.querySelector("#pendingCount").textContent = "1";
      document.querySelector("#approvedCount").textContent = "4";
      h.renderEnrollments([{ enrollmentId: "enrollment", hostname: "New NAS", nodeKey: "nas-lab",
        verificationCode: "284 091", platform: "linux", architecture: "arm64",
        requestedFrom: "192.168.2.19", expiresAt: new Date(Date.now() + 600000).toISOString() }]);
      h.renderNodes([
        { nodeId: "00000000-0000-0000-0000-000000000001", hostname: "Sine Desktop", nodeKey: "windows-main",
          platform: "windows", architecture: "amd64", nodeVersion: "0.13.1", status: "online", approvalStatus: "approved",
          lastSeenAt: new Date().toISOString(), capabilities: { files: true, processes: true, pty: true, appServer: true } },
        { nodeId: "00000000-0000-0000-0000-000000000002", hostname: "Home Server", nodeKey: "homeserver",
          platform: "linux", architecture: "amd64", nodeVersion: "0.13.1", status: "online", approvalStatus: "approved",
          lastSeenAt: new Date().toISOString(), capabilities: { files: true, processes: true, pty: true, appServer: true } },
        { nodeId: "00000000-0000-0000-0000-000000000003", hostname: "Android Lab", nodeKey: "android-lab",
          platform: "android", architecture: "arm64", nodeVersion: "0.13.1", status: "online", approvalStatus: "approved",
          lastSeenAt: new Date().toISOString(), capabilities: { files: true, processes: true, screen: true, input: true } },
        { nodeId: "00000000-0000-0000-0000-000000000004", hostname: "Old WSL", nodeKey: "wsl-old",
          platform: "linux", architecture: "amd64", nodeVersion: "0.12.0", status: "offline", approvalStatus: "approved",
          lastSeenAt: new Date(Date.now() - 86400000).toISOString(), capabilities: { files: true, processes: true } },
      ]);
      h.renderAudit([
        { createdAt: new Date().toISOString(), action: "node.connected", clientType: "node", targetNodeId: "android-lab", success: true },
        { createdAt: new Date(Date.now() - 45000).toISOString(), action: "codex_runtime.started", clientType: "admin", targetNodeId: "windows-main", success: true },
      ]);
    });
    await page.screenshot({ path: process.env.MIRA_DASHBOARD_SCREENSHOT, fullPage: true });
    await page.evaluate(() => window.traceHarness.show("agentView"));
  }

  await page.setViewportSize({ width: 390, height: 844 });
  await page.waitForFunction(() => document.querySelector("#agentThreadDrawer").getAttribute("aria-hidden") === "true");
  assert.equal(await page.locator("#agentThreadDrawer").evaluate((node) => node.inert), true);
  await page.locator("#agentThreadDrawerToggle").click();
  assert.equal(await page.locator("#agentThreadDrawer").getAttribute("aria-hidden"), "false");
  await page.keyboard.press("Escape");
  if (process.env.MIRA_TRACE_MOBILE_MESSAGES_SCREENSHOT) {
    await page.locator("#conversationScroll").evaluate((node) => { node.scrollTop = node.scrollHeight; });
    await page.locator(".conversation-card").screenshot({ path: process.env.MIRA_TRACE_MOBILE_MESSAGES_SCREENSHOT });
  }
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
  await notify("turn/completed", { threadId: "thread-a", turn: { id: "turn-1", status: "failed", error: {
    message: JSON.stringify({ type: "error", status: 400, error: {
      message: "The requested model requires a newer version of Codex.",
    } }),
  } } });
  assert.equal(await page.locator(".trace-card.error .trace-body").last().textContent(),
    "The requested model requires a newer version of Codex.", "live nested Codex errors must be readable");
  // A failed write cannot store its own error. Neither completion reconciliation,
  // later canonical refreshes nor switching conversations may erase that diagnostic.
  const errorRequests = transcriptRequests;
  await notify("error", { threadId: "thread-a", turnId: "turn-1", error: {
    message: "The requested model requires a newer version of Codex.",
  } });
  await notify("turn/completed", { threadId: "thread-a", turn: { id: "turn-1", status: "failed", error: {
    message: "The requested model requires a newer version of Codex.",
  } } });
  await page.waitForTimeout(2800);
  assert.equal(transcriptRequests - errorRequests, 1, "completion burst reconciles once, not three polls");
  assert.equal(await page.locator("[data-diagnostic-key]").count(), 1, "repeated reports share one per-turn error card");
  await page.evaluate(async () => {
    const h = window.traceHarness;
    await h.loadAgentTranscript("thread-a", null, { preserveLoaded: true });
    h.agent.threadId = "thread-b"; h.resetAgentTranscript("thread-b"); h.renderTranscript();
  });
  assert.equal(await page.locator("[data-diagnostic-key]").count(), 0, "errors must not leak into another thread");
  await notify("error", { threadId: "thread-a", turnId: "turn-1", error: { message: "Final save also failed" } });
  await page.evaluate(async () => {
    const h = window.traceHarness;
    h.agent.threadId = "thread-a"; h.resetAgentTranscript("thread-a");
    await h.loadAgentTranscript("thread-a");
  });
  assert.equal(await page.locator("[data-diagnostic-key]").count(), 1);
  assert.match(await page.locator("[data-diagnostic-key] .trace-body").textContent(), /Final save also failed/);
  await page.locator("[data-dismiss-diagnostic]").click();
  await page.evaluate(() => window.traceHarness.loadAgentTranscript("thread-a", null, { preserveLoaded: true }));
  assert.equal(await page.locator("[data-diagnostic-key]").count(), 0, "explicit dismissal survives refresh");
  let stableVersion = 8;
  await page.route("**/v1/codex/threads/stable-thread/transcript?*", (route) => route.fulfill({ json: {
    generation: 1, storeVersion: stableVersion, itemCount: 20, nextCursor: null,
    trace: [{ key: "stable-item", kind: "assistant", body: "Already durable", sourceItemSeq: 20, turnId: "stable-turn" },
      ...(stableVersion === 9 ? [{ key: "persisted-pending", kind: "user", body: "Pending without event", sourceItemSeq: 21, turnId: "pending-turn" }] : [])],
  } }));
  const unchanged = await page.evaluate(async () => {
    const h = window.traceHarness;
    h.agent.threadId = "stable-thread"; h.resetAgentTranscript("stable-thread");
    await h.loadAgentTranscript("stable-thread");
    const original = document.querySelector('[data-trace-key="stable-item"]');
    h.notify({ method: "item/started", params: { threadId: "stable-thread", turnId: "new-turn",
      item: { id: "unpersisted-user", type: "userMessage", content: [{ type: "text", text: "Unsaved input" }] } } });
    await h.loadAgentTranscript("stable-thread", null, { preserveLoaded: true });
    return { sameNode: original === document.querySelector('[data-trace-key="stable-item"]'),
      retainedInput: document.querySelector("#conversationTrace").textContent.includes("Unsaved input") };
  });
  assert.deepEqual(unchanged, { sameNode: true, retainedInput: true }, "unchanged tails avoid DOM rebuilding and retain live-only input");
  stableVersion = 9;
  const pendingCount = await page.evaluate(async () => {
    const h = window.traceHarness;
    const pending = h.upsertTrace("pending-user", "user", "你", "Pending without event", "", { turnId: "pending-turn" });
    pending.dataset.pendingUser = "true";
    await h.loadAgentTranscript("stable-thread", null, { preserveLoaded: true });
    return [...document.querySelectorAll(".trace-card.user .trace-body")].filter((body) => body._miraSource === "Pending without event").length;
  });
  assert.equal(pendingCount, 1, "a persisted user message replaces its optimistic copy even if the live item event was missed");
  await notify("error", { threadId: "stable-thread", turnId: "pending-turn", willRetry: true,
    error: { message: "Temporary network issue; retrying" } });
  assert.equal(await page.locator("[data-diagnostic-key]").count(), 0, "a runtime retry warning must not be mislabeled as permanent failure");
  assert.deepEqual(errors, [], "browser must have no uncaught errors");
  const screenshot = process.env.MIRA_TRACE_SCREENSHOT ?? process.argv[4];
  if (screenshot) await page.locator(".conversation-card").screenshot({ path: screenshot });
  for (const width of [1440, 390]) {
    const historyPage = await browser.newPage({ viewport: { width, height: 844 } });
    historyPage.on("pageerror", (error) => errors.push(error.message));
    const requests = [];
    const pending = [];
    const awaitPending = async () => {
      for (let attempt = 0; !pending.length && attempt < 500; attempt++) await new Promise((resolve) => setTimeout(resolve, 10));
      assert.ok(pending.length, `expected another history request; received ${JSON.stringify(requests)}`);
    };
    const messages = (start, count) => Array.from({ length: count }, (_, i) => ({
      key: `history-${start + i}`, kind: "assistant", turnId: `turn-${start + i}`,
      sourceItemSeq: start + i, body: `Message ${start + i}\n\nA paragraph of conversation history.`,
    }));
    await historyPage.route("**/v1/codex/threads/*/transcript?*", async (route) => {
      const url = new URL(route.request().url());
      const cursor = url.searchParams.get("cursor");
      requests.push(cursor);
      if (!cursor) return route.fulfill({ json: { generation: 1, trace: messages(201, 30), nextCursor: "older-200" } });
      const response = await new Promise((resolve) => pending.push(resolve));
      await route.fulfill(response);
    });
    await historyPage.goto(`http://127.0.0.1:${server.address().port}/`);
    await historyPage.waitForFunction(() => !!window.traceHarness);
    await historyPage.evaluate(async () => {
      const h = window.traceHarness;
      h.agent.threadId = "history-thread";
      h.resetAgentTranscript("history-thread");
      await h.loadAgentTranscript("history-thread");
      await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    });
    assert.deepEqual(requests, [null], "opening a long conversation stays at its latest page");
    await historyPage.locator("#conversationScroll").evaluate((scroll) => { scroll.scrollTop = 0; });
    await historyPage.waitForFunction(() => window.traceHarness.agent.transcriptLoadingOlder);
    await historyPage.waitForFunction(() => document.querySelector(".history-load-status").textContent.includes("正在加载"));
    await awaitPending();
    await historyPage.locator("#conversationScroll").evaluate(async (scroll) => {
      for (let i = 0; i < 20; i++) scroll.dispatchEvent(new Event("scroll"));
      await new Promise(requestAnimationFrame);
      scroll.scrollTop = 140; // Keep the reader's latest position while the request is in flight.
      window.traceHarness.agent.turnId = "live-turn";
      window.traceHarness.notify({ method: "item/agentMessage/delta", params: {
        threadId: "history-thread", turnId: "live-turn", itemId: "live", delta: "Still streaming while reading history",
      } });
    });
    assert.deepEqual(requests, [null, "older-200"], "top scroll events coalesce into one page request");
    const anchor = historyPage.locator('[data-trace-key="history-202"]');
    const before = await anchor.boundingBox();
    pending.shift()({ json: { generation: 1, trace: messages(171, 30), nextCursor: "older-170" } });
    await historyPage.locator('[data-trace-key="history-171"]').waitFor();
    await historyPage.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.ok(Math.abs((await anchor.boundingBox()).y - before.y) <= 1, "prepending preserves the visible message position");
    assert.equal(await historyPage.getByText("Still streaming while reading history", { exact: true }).count(), 1);
    assert.deepEqual(requests, [null, "older-200"], "a restored anchor must not drain the next page");

    // Empty canonical pages advance automatically; errors pause until explicitly retried.
    await historyPage.locator("#conversationScroll").evaluate((scroll) => { scroll.scrollTop = 0; });
    await awaitPending();
    pending.shift()({ json: { generation: 1, trace: [], nextCursor: "older-160" } });
    await awaitPending();
    assert.deepEqual(requests.slice(-2), ["older-170", "older-160"]);
    pending.shift()({ status: 503, json: { error: "Temporary history outage" } });
    await historyPage.locator('[data-load-older]:not([hidden])').waitFor();
    const failedCount = requests.length;
    await historyPage.locator("#conversationScroll").evaluate(async (scroll) => {
      scroll.dispatchEvent(new Event("scroll"));
      await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    });
    assert.equal(requests.length, failedCount, "failed history must not spin automatic retries");
    await historyPage.locator("[data-load-older]").click();
    await awaitPending();
    pending.shift()({ json: { generation: 1, trace: messages(1, 30), nextCursor: null } });
    await historyPage.locator('[data-trace-key="history-1"]').waitFor();
    assert.equal(await historyPage.locator(".history-loader").count(), 0, "oldest page removes the loader");
    const completeCount = requests.length;
    await historyPage.locator("#conversationScroll").evaluate(async (scroll) => {
      scroll.scrollTop = 0;
      await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    });
    assert.equal(requests.length, completeCount);

    // An old request's finally block must not clear the next conversation's loading state.
    await historyPage.evaluate(() => {
      const h = window.traceHarness;
      h.agent.transcriptCursor = "stale-page";
      h.renderTranscript(null, { anchorBottom: false });
      document.querySelector("#conversationScroll").scrollTop = 0;
    });
    await awaitPending();
    await historyPage.evaluate(() => {
      const h = window.traceHarness;
      h.agent.selectionEpoch++;
      h.agent.threadId = "next-thread";
      h.resetAgentTranscript("next-thread");
      h.agent.transcriptLoadingOlder = true;
      h.clear();
      h.upsertTrace("next-thread-message", "assistant", "Codex", "Next conversation");
    });
    pending.shift()({ json: { generation: 1, trace: messages(999, 1), nextCursor: null } });
    await historyPage.waitForLoadState("networkidle");
    assert.equal(await historyPage.getByText("Next conversation", { exact: true }).count(), 1);
    assert.equal(await historyPage.locator('[data-trace-key="history-999"]').count(), 0);
    assert.equal(await historyPage.evaluate(() => window.traceHarness.agent.transcriptLoadingOlder), true);
    const activity = await historyPage.evaluate(() => {
      const h = window.traceHarness;
      h.agent.socket = { readyState: WebSocket.OPEN, close() {} };
      h.agent.threadId = "activity-thread";
      const send = (method, params = {}) => h.notify({ method, params: { threadId: "activity-thread", turnId: "activity-turn", ...params } });
      const status = () => ({ visible: !document.querySelector("#conversationActivity").classList.contains("hidden"), text: document.querySelector("#conversationActivityText").textContent });
      send("turn/started", { turn: { id: "activity-turn" } });
      send("item/completed", { item: { id: "prose", type: "agentMessage", text: "I will continue." } });
      const gap = status();
      send("item/started", { item: { id: "tool", type: "commandExecution", command: "pwd" } });
      const tool = status();
      send("item/completed", { item: { id: "tool", type: "commandExecution", command: "pwd", status: "completed" } });
      const nextGap = status();
      send("turn/completed", { threadId: "other-thread", turn: { id: "other-turn" } });
      const unrelated = status();
      send("item/agentMessage/delta", { itemId: "next-prose", delta: "Next words" });
      const prose = status();
      h.agent.threadId = "idle-thread"; h.syncActiveTurnUi();
      const switched = status();
      h.agent.threadId = "activity-thread"; h.syncActiveTurnUi();
      const returned = status();
      send("turn/completed", { turn: { id: "activity-turn", status: "completed" } });
      const completed = status();
      send("turn/started", { turn: { id: "failed-turn" } });
      send("error", { turnId: "failed-turn", error: { message: "Execution failed" }, willRetry: false });
      const failed = status();
      send("turn/started", { turn: { id: "disconnect-turn" } });
      h.closeAgentSocket();
      return { gap, tool, nextGap, unrelated, prose, switched, returned, completed, failed, disconnected: status() };
    });
    assert.deepEqual(activity.gap, { visible: true, text: "Codex 仍在处理中…" });
    assert.deepEqual(activity.nextGap, activity.gap);
    assert.deepEqual(activity.unrelated, activity.gap);
    assert.deepEqual(activity.tool, { visible: true, text: "Codex 正在调用工具…" });
    assert.deepEqual(activity.prose, { visible: true, text: "Codex 正在回复…" });
    assert.deepEqual(activity.returned, activity.prose);
    for (const state of ["switched", "completed", "failed", "disconnected"]) assert.equal(activity[state].visible, false, state);
    const controls = await historyPage.evaluate(async () => {
      const h = window.traceHarness;
      h.agent.threadId = "stop-lifecycle";
      h.agent.selectionEpoch++;
      h.agent.threads = [{ threadId: "stop-lifecycle", cwd: "/project" }];
      let latest = { id: "whole-turn", status: "inProgress" };
      let held;
      let hold = false;
      const socket = { readyState: WebSocket.OPEN, close() {}, send(data) {
        const request = JSON.parse(data);
        const result = request.method === "thread/resume"
          ? { thread: { id: "stop-lifecycle", status: { type: "idle" } }, cwd: "/project" }
          : { data: [latest] };
        const reply = () => h.message({ data: JSON.stringify({ id: request.id, result }) });
        if (hold) held = reply; else queueMicrotask(reply);
      } };
      const attach = () => { h.agent.socket = socket; h.agent.socketInitialized = true; h.agent.loadedThreadIds.add("stop-lifecycle"); h.syncActiveTurnUi(); };
      const send = (method, params = {}) => h.notify({ method, params: { threadId: "stop-lifecycle", turnId: latest.id, ...params } });
      const state = () => ({ stop: !document.querySelector("#agentInterrupt").classList.contains("hidden"), disabled: document.querySelector("#agentInterrupt").disabled, send: !document.querySelector("#conversationSend").classList.contains("hidden") });
      attach();
      send("turn/started", { turn: latest });
      send("item/completed", { item: { id: "paragraph", type: "agentMessage", text: "I am still working." } });
      const paragraph = state();
      send("item/completed", { item: { id: "tool-finished", type: "commandExecution", command: "pwd", status: "completed" } });
      const tool = state();
      h.closeAgentSocket();
      const disconnected = state();
      attach();
      await h.restoreAgentThread("stop-lifecycle", socket);
      const resumed = state();
      latest = { ...latest, status: "completed" };
      hold = true;
      const staleRead = h.refreshActiveTurn("stop-lifecycle", socket);
      send("turn/started", { turn: { id: "newer-turn", status: "inProgress" } });
      held(); await staleRead;
      const stale = { ...state(), turnId: h.agent.turnId };
      hold = false;
      latest = { id: "newer-turn", status: "interrupted" };
      await h.refreshActiveTurn("stop-lifecycle", socket);
      const completed = state();
      send("item/completed", { item: { id: "late-item", type: "agentMessage", text: "Late completed item" } });
      const late = state();
      latest = { id: "missed-start", status: "inProgress" };
      send("item/agentMessage/delta", { itemId: "recover-prose", delta: "Continuing after reconnect" });
      const missedStart = state();
      send("turn/completed", { turn: { ...latest, status: "completed" } });
      hold = true;
      send("thread/status/changed", { status: { type: "active", activeFlags: [] } });
      const pendingId = state();
      latest = { id: "status-only-turn", status: "inProgress" };
      // The status-triggered read was captured before changing latest.
      held(); await h.agent.turnStateRequests.get("stop-lifecycle")?.promise;
      hold = false;
      await h.refreshActiveTurn("stop-lifecycle", socket);
      const statusRecovered = state();
      send("turn/completed", { turn: { ...latest, status: "completed" } });
      return { paragraph, tool, disconnected, resumed, stale, completed, late, missedStart, pendingId, statusRecovered };
    });
    const stoppable = { stop: true, disabled: false, send: false };
    for (const name of ["paragraph", "tool", "resumed", "missedStart", "statusRecovered"]) assert.deepEqual(controls[name], stoppable, name);
    for (const name of ["disconnected", "pendingId"]) assert.deepEqual(controls[name], { stop: true, disabled: true, send: false }, name);
    assert.deepEqual(controls.stale, { ...stoppable, turnId: "newer-turn" }, "stale completion cannot end a newer turn");
    for (const name of ["completed", "late"]) assert.equal(controls[name].send, true, name);
    const reconciledOrder = await historyPage.evaluate(() => {
      const h = window.traceHarness;
      h.clear(); h.agent.threadId = "ordered-thread";
      h.resetAgentTranscript("ordered-thread");
      const key = id => `item-${JSON.stringify(["ordered-thread", "ordered-turn", id])}`;
      const add = (id, kind, title, body) => h.upsertTrace(key(id), kind, title, body, "完成", { turnId: "ordered-turn", autoScroll: false });
      add("wrapper", "tool", "Wrapper", "Wrapper output");
      add("nested-one", "tool", "Nested 1", "First internal command");
      add("nested-two", "tool", "Nested 2", "Second internal command");
      add("final", "assistant", "Codex", "Final answer");
      add("actual-later-tool", "tool", "After final", "A tool really invoked later");
      h.agent.transcriptItems = [
        { key: "stored-wrapper", itemId: "wrapper", sourceItemSeq: 10, turnId: "ordered-turn", kind: "tool", title: "Wrapper", body: "Wrapper output", status: "完成" },
        { key: "stored-final", sourceItemSeq: 20, turnId: "ordered-turn", kind: "assistant", title: "Codex", body: "Final answer" },
      ];
      const orders = [];
      for (let i = 0; i < 3; i++) {
        h.renderTranscript(null, { preserveLive: true });
        orders.push([...document.querySelectorAll("#conversationTrace .trace-card")].map(card => card.dataset.traceTitle));
      }
      return orders;
    });
    for (const order of reconciledOrder) assert.deepEqual(order, ["Wrapper", "Nested 1", "Nested 2", "Codex", "After final"], "repeated history reconciliation retains nested tool order without moving real post-reply calls");
    await historyPage.close();
  }
  {
    const imagePage = await browser.newPage({ viewport: { width: 1100, height: 850 } });
    imagePage.on("pageerror", (error) => errors.push(error.message));
    await imagePage.route(`http://127.0.0.1:${server.address().port}/`, async (route) => {
      const response = await route.fetch();
      await route.fulfill({ response, headers: { ...response.headers(), "content-security-policy": "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self' ws: wss:" } });
    });
    await imagePage.addInitScript(() => {
      window.imagePolicyViolations = [];
      document.addEventListener("securitypolicyviolation", (event) => window.imagePolicyViolations.push(event.violatedDirective));
    });
    await imagePage.goto(`http://127.0.0.1:${server.address().port}/`);
    await imagePage.waitForFunction(() => !!window.traceHarness);
    const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aG1UAAAAASUVORK5CYII=";
    await imagePage.evaluate((png) => {
      const h = window.traceHarness;
      h.agent.threadId = "image-thread";
      h.agent.threadRuntimeNodeId = "image-node";
      h.resetAgentTranscript("image-thread");
      window.imageReads = [];
      window.imageOffline = false;
      window.blobs = new Set();
      const create = URL.createObjectURL.bind(URL), revoke = URL.revokeObjectURL.bind(URL);
      URL.createObjectURL = (blob) => { const url = create(blob); window.blobs.add(url); return url; };
      URL.revokeObjectURL = (url) => { window.blobs.delete(url); revoke(url); };
      h.setInvoke(async (node, capability, args) => {
        window.imageReads.push({ node, capability, ...args });
        if (window.imageOffline) throw new Error("运行节点离线");
        if (window.holdImage && args.action === "stat") await new Promise((resolve) => { window.releaseImage = resolve; });
        return args.action === "stat" ? { type: "file", size: atob(png).length } : { encoding: "base64", content: png, eof: true };
      });
      const params = { threadId: "image-thread", turnId: "image-turn", item: { type: "imageView", id: "view-1", path: "/tmp/chart.png" } };
      h.notify({ method: "item/started", params });
      h.notify({ method: "item/completed", params });
    }, png);
    const image = imagePage.locator("#conversationTrace > .trace-card.image img");
    await image.waitFor({ state: "visible" });
    assert.equal(await image.count(), 1, "started/completed share a single standalone preview");
    assert.equal(await image.evaluate((img) => img.naturalWidth), 1);
    assert.equal(await imagePage.locator(".tool-group[open]").count(), 0, "image remains visible while tool group is collapsed");
    assert.equal(await imagePage.locator(".tool-group .trace-card.tool").count(), 1);
    assert.deepEqual(await imagePage.evaluate(() => window.imageReads.map((read) => read.action)), ["stat", "read"]);
    const durable = projectCodexTranscript([
      { type: "event_msg", payload: { type: "task_started", turn_id: "image-turn" } },
      { type: "response_item", payload: { type: "function_call", call_id: "view-1", name: "view_image", arguments: '{"path":"/tmp/chart.png"}' } },
      { type: "event_msg", payload: { type: "item_completed", item: { type: "ImageView", id: "view-1", path: "file:///tmp/chart.png" } } },
      { type: "response_item", payload: { type: "function_call_output", call_id: "view-1", output: [{ type: "input_image", image_url: `data:application/octet-stream;base64,${png}` }] } },
    ], { fragments: true });
    await imagePage.evaluate((trace) => {
      const h = window.traceHarness;
      window.imageOffline = true;
      h.agent.transcriptItems = trace;
      h.renderTranscript(null, { preserveLive: true });
    }, durable);
    await image.waitFor({ state: "visible" });
    assert.equal(await image.count(), 1, "canonical snapshot supersedes the live path preview");
    assert.equal(await imagePage.evaluate(() => window.imageReads.length), 2, "saved image works when the original Node is offline");
    assert.equal(await imagePage.evaluate(() => window.blobs.size), 1, "reconciliation releases the old file blob");
    await imagePage.evaluate(() => {
      const h = window.traceHarness;
      const later = h.agent.transcriptItems;
      const earlier = [{ ...later[0], images: [{ path: "/tmp/chart.png" }] }];
      h.agent.transcriptItems = h.mergeTranscriptItems(later, earlier);
      h.renderTranscript(null, { preserveViewport: { mode: "prepend" } });
    });
    await image.waitFor({ state: "visible" });
    assert.equal(await image.count(), 1, "older path-only fragments cannot replace a saved snapshot");
    await imagePage.setViewportSize({ width: 390, height: 844 });
    assert.equal(await imagePage.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    await imagePage.evaluate(() => {
      const h = window.traceHarness;
      h.clear();
      h.upsertTrace("image-retry", "tool", "查看图片", "/tmp/missing.png", "完成", { images: [{ path: "/tmp/missing.png" }], turnId: "image-turn" });
    });
    await imagePage.locator(".trace-card.image button").waitFor({ state: "visible" });
    assert.match(await imagePage.locator(".trace-image-status").textContent(), /运行节点离线/);
    await imagePage.evaluate(() => { window.imageOffline = false; });
    await imagePage.locator(".trace-card.image button").click();
    await image.waitFor({ state: "visible" });
    await imagePage.evaluate(() => {
      const h = window.traceHarness;
      h.clear(); window.holdImage = true;
      h.upsertTrace("image-cancel", "tool", "查看图片", "/tmp/slow.png", "完成", { images: [{ path: "/tmp/slow.png" }], turnId: "image-turn" });
    });
    await imagePage.waitForFunction(() => !!window.releaseImage);
    await imagePage.evaluate(() => { window.traceHarness.clear(); });
    await imagePage.waitForFunction(() => window.blobs.size === 0);
    await imagePage.evaluate(() => { window.releaseImage(); });
    await imagePage.waitForTimeout(50);
    assert.equal(await imagePage.evaluate(() => window.imageReads.filter((read) => read.path === "/tmp/slow.png" && read.action === "read").length), 0,
      "switching conversations cancels pending image reads before requesting bytes");
    assert.deepEqual(await imagePage.evaluate(() => window.imagePolicyViolations), []);
    await imagePage.close();
  }
  assert.deepEqual(errors, []);
  console.log("PASS: standalone live/history images, snapshots without an online Node, pagination, mobile, retry, blob cleanup and cancellation");
  console.log("PASS: automatic top pagination on desktop/mobile, stable reading anchor, live stream preservation, empty pages, retry, exhaustion, stale-thread isolation and ongoing turn activity");
  console.log("PASS: real-browser mobile recovery, half-open probe, tail-first paint, stale resume isolation, thin glass, submission hint, cross-thread isolation, 17 MiB chunked attachments, cancellation cleanup, live/history activity and responsive layout");
} finally {
  await browser?.close();
  await new Promise((resolve) => server.close(resolve));
}

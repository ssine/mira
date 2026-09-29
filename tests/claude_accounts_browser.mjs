import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const nodeId = randomUUID(), ids = [randomUUID(), randomUUID()];
const account = (id, name, provider) => ({ engine: "claude", nodeAccountId: id, accountId: id, nodeId, name,
  enabled: true, configured: true, provider, reportedAppServer: { status: "ready", provider: {
    id: provider, baseUrl: "https://api.example.test", region: "us-east-1", model: provider === "bedrock" ? "anthropic.claude-example" : "claude-example",
    ...(provider === "anthropic" ? { effort: "xhigh" } : {}) } } });
const node = { nodeId, hostname: "WSL fixture", platform: "linux", status: "online", approvalStatus: "approved",
  capabilities: { appServer: true, codexAccountsV1: true, claudeRuntimeV1: true, claudeAccountsV1: true, claudeSessionCacheV1: true },
  desiredAppServer: { defaultCwd: "/work" }, reportedAppServer: { status: "stopped" }, codexAccounts: [],
  claudeAccounts: [account(ids[0], "Messages account", "anthropic"), account(ids[1], "AWS account", "bedrock")] };
const codexId = randomUUID();
node.codexAccounts = [{ nodeAccountId: codexId, name: "Messages account", enabled: true, isDefault: true, reportedAppServer: {status:"stopped"} }];
const calls = [], sessions = [];
let cacheBytes = 4 * 1024 ** 3;
const summary = s => ({ ...s, threadId:s.sessionId, engine:"claude", runtimeNodeId:s.nodeId, generation:1, itemCount:1, listRoot:true, childCount:0,
 updatedAt: new Date().toISOString(), activity:{state:s.activeTurn?"running":"idle",turnId:s.activeTurn || s.lastTurn,generation:1,itemCount:1,
 ...(s.activeTurn ? {costEstimate:{amount:.12,status:"complete",basis:"claude_sdk",running:true,note:"运行中估算"}} : {})}, costEstimate:{amount:.4,status:"complete",basis:"claude_sdk"} });
const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({ viewport: { width: 1200, height: 900 } });
  await context.route(/\/v1\/nodes(?:\?.*)?$/, route => route.fulfill({ json: { data: [node] } }));
  await context.route(`**/v1/nodes/${nodeId}`, route => route.fulfill({ json: node }));
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data:[],projects:[],paged:true } }));
  await context.route("**/v1/codex/threads/*?*", route => route.fulfill({status:404,json:{error:"missing"}}));
  await context.route("**/v1/claude/**", route => {
    const req = route.request(), path = new URL(req.url()).pathname, body = req.postData() ? req.postDataJSON() : null;
    calls.push({ path, body, query: new URL(req.url()).search });
    if (path.endsWith("cost-history")) {
      const at = Date.parse("2026-09-30T00:00:00Z"), date = "2026-09-30";
      return route.fulfill({json:{basis:"claude_sdk",estimate:{amount:.4,status:"complete"},from:at,to:at+3600000,
        days:[{date,at,end:at+86400000,amount:.4,status:"complete"}],
        points:[{date,at:at+600000,amount:.1,turnCount:1},{date,at:at+1200000,amount:.3,turnCount:1}]}});
    }
    if (path === "/v1/claude/conversations") return route.fulfill({json:{data:sessions.map(summary),paged:true,projects:sessions.length?[{key:JSON.stringify([nodeId,"/work"]),nodeId,cwd:"/work",count:sessions.length}]:[]}});
    if (path.startsWith("/v1/claude/conversations/")) return route.fulfill({json:summary(sessions.find(s=>s.sessionId===path.split("/")[4]))});
    if (path.includes("/accounts/")) return route.fulfill({ json: { configured: true } });
    if (path.endsWith("/cache-status") || path.endsWith("/cache-configure")) {
      if (path.endsWith("/cache-configure")) cacheBytes = body.maxBytes;
      return route.fulfill({ json: { maxBytes: cacheBytes, usedBytes: 0, entries: 0, cleanupPending: false } });
    }
    if (path.includes("/runtimes/")) return route.fulfill({ json: path.endsWith("describe") ? { models: [{ value: "opus", resolvedModel: "claude-example", displayName: "Claude", supportedEffortLevels: ["low", "high", "xhigh"] }] } : { status: "ready" } });
    if (path === "/v1/claude/sessions") {
      if (req.method() === "POST") { const session = { ...body, sessionId: randomUUID(), persistence: "saved", activeTurn: null }; sessions.push(session); return route.fulfill({ json: session }); }
      return route.fulfill({ json: { data: sessions, nextOffset: null } });
    }
    const session = sessions.find(s => s.sessionId === path.split("/")[4]);
    if (path.endsWith("/turns")) { session.nodeAccountId = body.nodeAccountId; session.model = body.model; session.activeTurn = session.lastTurn = randomUUID(); return route.fulfill({ json: { turnId: session.activeTurn } }); }
    if (path.endsWith("/costs")) return route.fulfill({json:{generation:1,turnCostEstimates:{[session.lastTurn]:{amount:.4,status:"complete",basis:"claude_sdk"}}}});
    if (path.endsWith("/children")) return route.fulfill({ json: { data: [] } });
    if (path.endsWith("/events")) {
      const after = Number(new URL(req.url()).searchParams.get("after") || 0), events = session.events || [];
      return route.fulfill({ json: { data: events.filter(e => e.seq > after), cursor: events.at(-1)?.seq || 0, earliest: events.length ? 1 : 0, session, hasMore: false } });
    }
    return route.fulfill({ json: session });
  });
  const page = await context.newPage(), errors = [];
  page.on("pageerror", error => errors.push(error.message));
  page.setDefaultTimeout(12000);
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator("#loginForm button[type=submit]").click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.locator(`[data-action="workspace"][data-id="${nodeId}"]`).click();
  await page.locator("#claudeCacheSave:not([disabled])").waitFor();
  assert.equal(await page.locator("#claudeCacheLimit").inputValue(), "4");
  await page.locator("#claudeCacheLimit").fill("1.5");
  await page.locator("#claudeCacheSave").click();
  await page.waitForFunction(() => document.querySelector("#claudeCacheStatus").textContent.includes("已用"));
  assert.equal(cacheBytes, 1.5 * 1024 ** 3);
  await page.locator("#claudeCacheLimit").fill("0");
  await page.locator("#claudeCacheSave").click();
  await page.waitForFunction(() => document.querySelector("#claudeCacheStatus").textContent.includes("已关闭"));
  await page.locator("#workspaceBack").click();
  await page.locator(".codex-account-row").filter({ hasText: "AWS account" }).click();
  const dialog = page.locator("#codexAccountDialog");
  assert.equal(await dialog.locator("[name=engine]").inputValue(), "claude");
  assert.equal(await dialog.locator("[data-account-login]").isVisible(), false);
  assert.equal(await dialog.locator("[name=claudeEffort]").inputValue(), "");
  await dialog.locator("[name=claudeEffort]").selectOption("high");
  await dialog.locator("[name=apiKey]").fill("synthetic-claude-browser-secret");
  await dialog.locator("button[type=submit]").click();
  await dialog.getByText("Claude 账号已保存，可在对话的账号列表中选择使用。", { exact: true }).waitFor();
  assert.equal(calls.find(c => c.path.endsWith("/configure")).body.apiKey, "synthetic-claude-browser-secret");
  assert.equal(calls.find(c => c.path.endsWith("/configure")).body.provider.effort, "high");
  assert.equal(await dialog.locator("[name=apiKey]").inputValue(), "");
  assert.equal(await page.evaluate(() => JSON.stringify(localStorage).includes("synthetic-claude-browser-secret")), false);
  await dialog.locator("[data-account-close]").click();
  await page.locator("#globalAgent").click();
  const view = page.locator("#agentView");
  assert.equal(await page.locator("#claudeView").count(), 0);
  await view.locator("#conversationAccount").selectOption(ids[1]);
  await view.locator("#conversationInput").fill("Remember this account");
  await view.locator("#conversationSend").click();
  await page.waitForFunction(() => document.querySelector("#conversationInput").value === "");
  await page.waitForFunction(() => document.querySelector("#conversationAccount").disabled);
  assert.equal(calls.find(c => c.path === "/v1/claude/sessions" && c.body).body.nodeAccountId, ids[1]);
  assert.equal(calls.find(c => c.path.endsWith("/turns")).body.nodeAccountId, ids[1]);
  assert.equal(await view.locator(`#conversationAccount option[value="${codexId}"]`).count(),0,"existing Claude session offers same-engine accounts only");
  await page.reload();
  await page.waitForFunction(() => document.querySelector("#conversationTitle").textContent === "Remember this account");
  assert.equal(await view.locator("#conversationAccount").inputValue(), ids[1]);
  await view.locator("#conversationActivity:not(.hidden)").waitFor();
  assert.match(await view.locator("#conversationActivityText").textContent(), /^Claude /);
  await page.waitForFunction(() => document.querySelector("#conversationActivityCost").textContent === "本轮约 $0.12");
  assert.equal(await view.locator("#conversationComposer #claudeReconcile").count(), 0);
  assert.equal(await view.locator(`[data-thread-activity="${sessions[0].sessionId}"]`).getAttribute("title"), "Claude 正在运行");
  await view.locator("#conversationDetailsToggle").click();
  await view.locator("#conversationDetails #claudeReconcile").waitFor({ state: "visible" });
  const emit = payload => {
    const events = sessions[0].events ||= [];
    events.push({ seq: events.length + 1, turnId: sessions[0].activeTurn, payload });
  };
  emit({ type: "stream_event", event: { type: "message_start", message: { id: "live-reply" } } });
  emit({ type: "assistant", uuid: "thinking", message: { id: "live-reply", content: [{ type: "thinking", thinking: "Consider the question" }] } });
  await view.locator("#conversationTrace .trace-card.reasoning").waitFor();
  emit({ type: "stream_event", event: { type: "content_block_delta", index: 1, delta: { type: "text_delta", text: "One live answer" } } });
  await view.locator("#conversationTrace .trace-card.assistant").filter({ hasText: "One live answer" }).waitFor();
  emit({ type: "assistant", uuid: "answer", timestamp: "2026-09-30T01:02:03.456Z", message: { id: "live-reply", content: [{ type: "text", text: "One live answer" }] } });
  emit({ type: "stream_event", event: { type: "message_stop" } });
  const running = sessions[0].activeTurn;
  assert.equal(calls.some(c => c.path.endsWith("/costs") && c.query.includes(running)), false, "the summary prices a running turn");
  emit({ type: "result", duration_ms: 100 });
  sessions[0].activeTurn = null;
  await page.waitForFunction(() => !document.querySelector("#conversationAccount").disabled);
  assert.equal(await view.locator("#conversationTrace .trace-card.assistant").count(), 1, "the final reply replaces streamed prose without requiring a reload");
  const clock = view.locator("#conversationTrace .trace-card.assistant .trace-completed");
  assert.equal(await clock.isVisible(), true, "Claude messages retain their native record time");
  assert.match(await clock.textContent(), /\d{2}:\d{2}:\d{2}/);
  assert.equal(await clock.getAttribute("title"), "消息记录时间");
  await page.waitForFunction(() => /0\.40/.test(document.querySelector("#conversationTrace .trace-card.assistant .trace-cost").textContent));
  assert.equal(await view.locator("#conversationActivityCost").isVisible(), false);
  await view.locator("#claudeReconcile").waitFor({ state: "hidden" });
  await view.locator("#conversationDetailsClose").click();
  await view.locator("#conversationAccount").selectOption(ids[0]);
  await view.locator("#conversationInput").fill("Switch the idle conversation");
  await view.locator("#conversationSend").click();
  await page.waitForFunction(() => document.querySelector("#conversationInput").value === "");
  assert.equal(calls.filter(c => c.path.endsWith("/turns")).at(-1).body.nodeAccountId, ids[0]);
  assert.equal(await view.locator('.sidebar-account-row[data-account-engine="codex"]').count(),1);
  assert.equal(await view.locator('.sidebar-account-row[data-account-engine="claude"]').count(),2);
  const awsRow = view.locator('.sidebar-account-row[data-account-engine="claude"]').filter({hasText:"AWS account"});
  assert.equal(await awsRow.isVisible(), false, "accounts outside the recent conversations stay folded");
  await view.locator("[data-account-more]").click();
  await awsRow.click();
  const curve = view.locator("#agentAccountDetails .spend-line");
  await curve.waitFor();
  assert.equal((await curve.getAttribute("d")).match(/L/g).length, 3, "each same-hour turn contributes a separate curve point");
  await page.evaluate(() => document.querySelector("#agentAccountDetails").hidePopover());
  await view.locator("#agentNewThread").click();
  assert.equal(await view.locator(`#conversationAccount option[value="${codexId}"]`).count(),1,"new sessions can select Codex again");
  await view.locator("#conversationAccount").selectOption(ids[0]);
  await page.waitForFunction(() => document.querySelector("#conversationEffortLabel").textContent === "思考 · 很高");
  assert.equal(await view.locator("#conversationModelLabel").textContent(), "Claude", "the account model ID uses its SDK alias capabilities");
  await view.locator("#conversationInput").fill("Use the account default effort");
  await view.locator("#conversationSend").click();
  await page.waitForFunction(() => document.querySelector("#conversationInput").value === "");
  const defaultTurn = calls.filter(c => c.path.endsWith("/turns")).at(-1).body;
  assert.deepEqual([defaultTurn.nodeAccountId, defaultTurn.model, defaultTurn.effort], [ids[0], "claude-example", "xhigh"]);
  assert.deepEqual(errors, []);
  console.log("PASS: shared Claude account management, secret clearing, provider models, binding reload and idle account switch");
} finally { await browser.close(); }

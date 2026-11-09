import assert from "node:assert/strict";
import crypto from "node:crypto";
const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const nodeId = crypto.randomUUID(), binding = crypto.randomUUID(), removed = crypto.randomUUID(), custom = crypto.randomUUID(), threadId = crypto.randomUUID();
const account = (id, name, provider, extra = {}) => ({ nodeAccountId: id, accountId: id, nodeId, name, enabled: true,
  reportedAppServer: { status: "running", provider }, desiredAppServer: { running: true }, ...extra });
const node = { nodeId, hostname: "Fast fixture", platform: "linux", status: "online", approvalStatus: "approved",
  capabilities: { appServer: true, codexAccountsV1: true, claudeRuntimeV1: true }, desiredAppServer: { defaultCwd: "/work" }, reportedAppServer: { status: "running" },
  codexAccounts: [account(binding, "Login", { id: "openai", credentialSource: "chatgpt" }, { isDefault: true, snapshot: { account: { type: "chatgpt" } } }),
    account(removed, "Removed", { id: "openai" }, { enabled: false, deletedAt: "2026-01-01T00:00:00Z" }), account(custom, "API", { id: "fixture", credentialSource: "environment" })] };
let latest = { threadId: "not-loaded-in-sidebar", model: "gpt-6-astra", effort: "high", serviceTier: "priority" };
const rows = [], calls = [];
const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  await context.route(/\/v1\/nodes(?:\?.*)?$/, route => route.fulfill({ json: { data: [node] } }));
  await context.route(`**/v1/nodes/${nodeId}`, route => route.fulfill({ json: node }));
  await context.route("**/last-settings", route => route.fulfill({ json: latest }));
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data: rows } }));
  await context.route(/\/v1\/codex\/threads\/[^?]+\?/, route => route.fulfill({ json: new URL(route.request().url()).pathname.endsWith("/transcript") ? { generation: 1, itemCount: 0, trace: [] } : rows[0] }));
  await context.routeWebSocket(/\/app-server\?/, socket => {
    let client;
    socket.onMessage(raw => {
      const req = JSON.parse(raw); if (req.id === undefined) return;
      const reply = result => socket.send(JSON.stringify({ id: req.id, result }));
      if (req.method === "initialize") { client = req.params.clientInfo.name; reply({}); return; }
      if (client === "mira_web_models") {
        reply(req.method === "config/read" ? { config: { model: "gpt-6-astra", model_reasoning_effort: "medium" } } : { data: [{ model: "gpt-6-astra", displayName: "Astra", isDefault: true, supportedReasoningEfforts: [{ reasoningEffort: "medium" }, { reasoningEffort: "high" }] }] }); return;
      }
      if (client === "mira_web_account") { reply({ account: { type: "chatgpt" } }); return; }
      if (client === "mira_web_title") { socket.send(JSON.stringify({ id: req.id, error: { message: "disabled in fixture" } })); return; }
      calls.push(req);
      if (req.method === "thread/start") {
        rows.push({ threadId, title: "Fast test", nodeAccountId: binding, runtimeNodeId: nodeId, model: req.params.model, cwd: "/work", generation: 1, itemCount: 0, serviceTier: req.params.serviceTier ?? "default", activity: { state: "idle" } });
        reply({ thread: { id: threadId }, cwd: "/work", model: req.params.model, serviceTier: req.params.serviceTier });
      } else if (req.method === "turn/start") {
        latest = { ...latest, serviceTier: req.params.serviceTier ?? "default" };
        rows[0].serviceTier = latest.serviceTier;
        reply({ turn: { id: crypto.randomUUID(), status: "completed" } });
      } else if (req.method === "thread/resume") reply({ thread: { id: threadId, status: { type: "idle" } }, cwd: "/work", model: "gpt-6-astra", serviceTier: req.params.serviceTier });
      else if (req.method === "thread/loaded/list") reply({ data: [threadId] });
      else reply({ thread: { id: threadId, status: { type: "idle" }, turns: [] } });
    });
  });
  const page = await context.newPage(), errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(origin); await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator("#loginForm button[type=submit]").click(); await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?view=agent`);
  const fast = page.locator("#conversationFast");
  await page.waitForFunction(() => document.querySelector("#conversationFast").textContent === "Fast · 开");
  assert.equal(await page.locator(`#conversationAccount option[value="${removed}"]`).count(), 0);
  assert.equal(await page.locator('#conversationAccount option[value="claude-default"]').count(), 0);
  assert.equal(await page.locator("#conversationEffortSelect").evaluate(el => el.value), "high", "account defaults come from history outside the loaded page");
  await page.locator("#conversationInput").fill("Fast please"); await page.locator("#conversationSend").click();
  await page.waitForURL(`**/?thread=${threadId}`); await page.waitForFunction(() => !document.querySelector("#conversationFast").disabled);
  assert.equal(calls.find(c => c.method === "thread/start").params.serviceTier, "priority");
  assert.equal(calls.find(c => c.method === "turn/start").params.serviceTier, "priority");
  await fast.click();
  await page.locator("#conversationInput").fill("Standard please"); await page.locator("#conversationSend").click();
  await page.waitForFunction(() => !document.querySelector("#conversationFast").disabled);
  assert.equal(calls.filter(c => c.method === "turn/start").at(-1).params.serviceTier, null, "explicit null resets the native thread tier");
  await page.reload(); await page.waitForFunction(() => document.querySelector("#conversationFast").textContent === "Fast · 关");
  await page.locator("#agentNewThread").click();
  await page.waitForFunction(() => document.querySelector("#conversationFast").textContent === "Fast · 关");
  await page.locator("#conversationAccount").selectOption(custom);
  assert.equal(await fast.isVisible(), false, "custom API providers have no ChatGPT Fast toggle");
  assert.deepEqual(errors, []);
  console.log("PASS: Fast native start/turn/reset, canonical reload, complete-account defaults, deleted accounts and no implicit Claude entry");
} finally { await browser.close(); }

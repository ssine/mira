import assert from "node:assert/strict";
import crypto from "node:crypto";
const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const nodeId = crypto.randomUUID(), official = crypto.randomUUID(), custom = crypto.randomUUID(), threadId = crypto.randomUUID();
const account = (id, name, isDefault, provider) => ({ nodeAccountId: id, accountId: crypto.randomUUID(), nodeId, name, isDefault,
  enabled: true, revision: 1, credentialRevision: 1, provider: provider.id, authType: isDefault ? "chatgpt" : "providerConfig",
  desiredAppServer: { running: false }, reportedAppServer: { status: "stopped", provider } });
const node = { nodeId, hostname: "Accounts fixture", platform: "linux", status: "online", approvalStatus: "approved",
  capabilities: { appServer: true, codexAccountsV1: true }, reportedAppServer: { status: "stopped" }, desiredAppServer: { defaultCwd: "/work" },
  codexAccounts: [account(official, "Official", true, { id: "openai" }), account(custom, "Provider", false, { id: "custom", name: "Private API", baseUrl: "https://api.example.test/v1" })] };
const thread = { threadId, title: "Account handoff", cwd: "/work", model: "gpt-6-astra", runtimeNodeId: nodeId, nodeAccountId: official,
  generation: 1, itemCount: 5, activity: { state: "idle" } };
const mutations = [], calls = [], failureId = crypto.randomUUID();
let incompatible = false, confirmed = false;
const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({ viewport: { width: 1200, height: 900 } });
  await context.route("**/v1/nodes", route => route.fulfill({ json: { data: [node] } }));
  await context.route("**/v1/nodes?*", route => route.fulfill({ json: { data: [node] } }));
  await context.route(`**/v1/nodes/${nodeId}`, route => route.fulfill({ json: node }));
  await context.route(`**/v1/nodes/${nodeId}/codex-accounts/**`, route => {
    const request = route.request(), path = new URL(request.url()).pathname;
    const body = request.postDataJSON(); mutations.push({ path, body, method: request.method() });
    const binding = path.split("/")[5], selected = node.codexAccounts.find(value => value.nodeAccountId === binding);
    if (request.method() === "PATCH") selected.name = body.name;
    return route.fulfill({ json: path.endsWith("/login") ? { status: "completed" } : { ok: true } });
  });
  await context.route("**/v1/codex/runtimes/**", route => {
    const body = route.request().postDataJSON(), selected = node.codexAccounts.find(value => value.nodeAccountId === body.nodeAccountId);
    selected.reportedAppServer.status = "running"; selected.desiredAppServer.running = true;
    return route.fulfill({ json: { ok: true } });
  });
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data: [thread] } }));
  await context.route(/\/v1\/codex\/threads\/[^?]+\?/, route => {
    const request = route.request(), path = new URL(request.url()).pathname;
    if (path.endsWith("/input-recovery")) {
      if (request.method() === "POST") { mutations.push({ path, body: request.postDataJSON() }); confirmed = true; return route.fulfill({ json: { status: "confirmed", canonicalHistoryUnchanged: true } }); }
      return route.fulfill(incompatible && !confirmed ? { json: { failureId, generation: 1, itemCount: 5, throughItemSeq: 5,
        encryptedItems: 2, policy: "rebuildContext", recoverable: true } } : { status: 409, json: { error: "no failure" } });
    }
    return route.fulfill({ json: path.endsWith("/transcript") ? { generation: 1, itemCount: 5, trace: [], nextCursor: null } : thread });
  });
  await context.routeWebSocket(/\/app-server\?/, socket => {
    const binding = new URL(socket.url()).searchParams.get("nodeAccountId"); let client;
    socket.onMessage(raw => {
      const request = JSON.parse(raw); if (request.id === undefined) return;
      const reply = result => socket.send(JSON.stringify({ id: request.id, result }));
      if (request.method === "initialize") { client = request.params.clientInfo.name; reply({}); return; }
      if (client === "mira_web_models") {
        if (request.method === "config/read") reply({ config: { model: "gpt-6-astra", model_reasoning_effort: "medium" } });
        else reply({ data: [{ model: "gpt-6-astra", displayName: "Astra", isDefault: true, supportedReasoningEfforts: [{ reasoningEffort: "medium" }] }], nextCursor: null });
        return;
      }
      if (client === "mira_web_account") { reply(request.method === "account/read" ? { account: { type: "apiKey" } } : {}); return; }
      calls.push({ binding, ...request });
      if (request.method === "thread/resume") reply({ thread: { id: threadId, status: { type: "idle" } }, cwd: "/work", model: "gpt-6-astra" });
      else if (request.method === "thread/loaded/list") reply({ data: [threadId] });
      else if (request.method === "thread/read") reply({ thread: { id: threadId, status: { type: "idle" }, turns: [] } });
      else if (request.method === "turn/start") {
        const turnId = crypto.randomUUID(); incompatible = true;
        reply({ turn: { id: turnId, status: "inProgress" } });
        setTimeout(() => {
          socket.send(JSON.stringify({ method: "turn/started", params: { threadId, turn: { id: turnId } } }));
          socket.send(JSON.stringify({ method: "mira/account/contextIncompatible", params: { threadId, nodeAccountId: binding, failureId } }));
          socket.send(JSON.stringify({ method: "turn/completed", params: { threadId, turn: { id: turnId, status: "failed", error: { message: "invalid_encrypted_content" } } } }));
        }, 100);
      } else reply({});
    });
  });
  const page = await context.newPage(), errors = [];
  page.on("pageerror", error => errors.push(error.message)); page.setDefaultTimeout(12_000);
  await page.goto(origin); await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator("#loginForm button[type=submit]").click(); await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.locator("#globalAccounts").click();
  await page.locator(".codex-account-row").filter({ hasText: "Provider" }).click();
  const dialog = page.locator("#codexAccountDialog");
  assert.equal(await dialog.locator("[data-account-login]").isVisible(), false);
  await dialog.locator('[name="name"]').fill("Renamed provider"); await dialog.locator('[type="submit"]').click();
  await page.getByText("配置已保存。启动账号后可查看额度或登录。", { exact: true }).waitFor();
  assert.equal(mutations.filter(value => value.path.endsWith("/configure")).length, 0, "renaming must not require changing live credentials");
  await dialog.locator('[name="apiKey"]').fill("synthetic-browser-secret"); await dialog.locator('[type="submit"]').click();
  await page.waitForFunction(() => document.querySelector('[name="apiKey"]').value === "");
  assert.equal(mutations.find(value => value.path.endsWith("/configure")).body.apiKey, "synthetic-browser-secret");
  assert.equal(await page.evaluate(() => JSON.stringify(localStorage).includes("synthetic-browser-secret")), false);
  await dialog.locator("[data-account-close]").click();
  await page.goto(`${origin}/?view=agent&thread=${threadId}`);
  await page.locator("#conversationAccount").selectOption(custom);
  await page.locator("#conversationInput").fill("Continue using the selected provider"); await page.locator("#conversationSend").click();
  const recovery = page.locator("#conversationCompatibility button"); await recovery.waitFor();
  assert.equal(calls.find(value => value.method === "thread/resume").binding, custom);
  assert.equal(calls.find(value => value.method === "turn/start").binding, custom);
  const turns = calls.filter(value => value.method === "turn/start").length;
  page.once("dialog", dialog => dialog.dismiss()); await recovery.click();
  assert.equal(confirmed, false, "dismissal preserves encrypted input");
  page.once("dialog", dialog => { assert.match(dialog.message(), /原始历史不会删除/); return dialog.accept(); });
  await recovery.click(); await page.locator("#conversationCompatibility").waitFor({ state: "hidden" });
  assert.equal(confirmed, true);
  assert.equal(calls.filter(value => value.method === "turn/start").length, turns, "confirmation never replays model/tool side effects");
  assert.equal(mutations.at(-1).body.confirm, true);
  assert.deepEqual(errors, []);
  console.log("Codex accounts browser: credential management, selection and explicit input recovery passed");
} finally { await browser.close(); }

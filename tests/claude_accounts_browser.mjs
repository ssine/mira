import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const nodeId = randomUUID(), ids = [randomUUID(), randomUUID()];
const account = (id, name, provider) => ({ engine: "claude", nodeAccountId: id, accountId: id, nodeId, name,
  enabled: true, configured: true, provider, reportedAppServer: { status: "ready", provider: {
    id: provider, baseUrl: "https://api.example.test", region: "us-east-1", model: provider === "bedrock" ? "anthropic.claude-example" : "claude-example" } } });
const node = { nodeId, hostname: "WSL fixture", platform: "linux", status: "online", approvalStatus: "approved",
  capabilities: { appServer: true, codexAccountsV1: true, claudeRuntimeV1: true, claudeAccountsV1: true },
  desiredAppServer: { defaultCwd: "/work" }, reportedAppServer: { status: "stopped" }, codexAccounts: [],
  claudeAccounts: [account(ids[0], "Messages account", "anthropic"), account(ids[1], "AWS account", "bedrock")] };
const codexId = randomUUID();
node.codexAccounts = [{ nodeAccountId: codexId, name: "Messages account", enabled: true, isDefault: true, reportedAppServer: {status:"stopped"} }];
const calls = [], sessions = [];
const summary = s => ({ ...s, threadId:s.sessionId, engine:"claude", runtimeNodeId:s.nodeId, generation:1, itemCount:1, listRoot:true, childCount:0,
 updatedAt: new Date().toISOString(), activity:{state:s.activeTurn?"running":"idle",turnId:s.activeTurn || s.lastTurn,generation:1,itemCount:1}, costEstimate:{amount:.4,status:"complete",basis:"claude_sdk"} });
const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({ viewport: { width: 1200, height: 900 } });
  await context.route(/\/v1\/nodes(?:\?.*)?$/, route => route.fulfill({ json: { data: [node] } }));
  await context.route(`**/v1/nodes/${nodeId}`, route => route.fulfill({ json: node }));
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data:[],projects:[],paged:true } }));
  await context.route("**/v1/codex/threads/*?*", route => route.fulfill({status:404,json:{error:"missing"}}));
  await context.route("**/v1/claude/**", route => {
    const req = route.request(), path = new URL(req.url()).pathname, body = req.postData() ? req.postDataJSON() : null;
    calls.push({ path, body });
    if (path.endsWith("cost-history")) return route.fulfill({json:{estimate:{amount:.4,status:"complete"},days:[],points:[]}});
    if (path === "/v1/claude/conversations") return route.fulfill({json:{data:sessions.map(summary),paged:true,projects:sessions.length?[{key:JSON.stringify([nodeId,"/work"]),nodeId,cwd:"/work",count:sessions.length}]:[]}});
    if (path.startsWith("/v1/claude/conversations/")) return route.fulfill({json:summary(sessions.find(s=>s.sessionId===path.split("/")[4]))});
    if (path.includes("/accounts/")) return route.fulfill({ json: { configured: true } });
    if (path.includes("/runtimes/")) return route.fulfill({ json: path.endsWith("describe") ? { models: [{ value: "claude-example", displayName: "Claude" }] } : { status: "ready" } });
    if (path === "/v1/claude/sessions") {
      if (req.method() === "POST") { const session = { ...body, sessionId: randomUUID(), persistence: "saved", activeTurn: null }; sessions.push(session); return route.fulfill({ json: session }); }
      return route.fulfill({ json: { data: sessions, nextOffset: null } });
    }
    const session = sessions.find(s => s.sessionId === path.split("/")[4]);
    if (path.endsWith("/turns")) { session.nodeAccountId = body.nodeAccountId; session.model = body.model; session.activeTurn = session.lastTurn = randomUUID(); return route.fulfill({ json: { turnId: session.activeTurn } }); }
    if (path.endsWith("/costs")) return route.fulfill({json:{generation:1,turnCostEstimates:{}}});
    if (path.endsWith("/children")) return route.fulfill({ json: { data: [] } });
    if (path.endsWith("/events")) return route.fulfill({ json: { data: [], cursor: 0, earliest: 0, session, hasMore: false } });
    return route.fulfill({ json: session });
  });
  const page = await context.newPage(), errors = [];
  page.on("pageerror", error => errors.push(error.message));
  page.setDefaultTimeout(12000);
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator("#loginForm button[type=submit]").click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.locator(".codex-account-row").filter({ hasText: "AWS account" }).click();
  const dialog = page.locator("#codexAccountDialog");
  assert.equal(await dialog.locator("[name=engine]").inputValue(), "claude");
  assert.equal(await dialog.locator("[data-account-login]").isVisible(), false);
  await dialog.locator("[name=apiKey]").fill("synthetic-claude-browser-secret");
  await dialog.locator("button[type=submit]").click();
  await dialog.getByText("Claude 账号已保存，可在对话的账号列表中选择使用。", { exact: true }).waitFor();
  assert.equal(calls.find(c => c.path.endsWith("/configure")).body.apiKey, "synthetic-claude-browser-secret");
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
  sessions[0].activeTurn = null;
  await page.waitForFunction(() => !document.querySelector("#conversationAccount").disabled);
  await view.locator("#conversationAccount").selectOption(ids[0]);
  await view.locator("#conversationInput").fill("Switch the idle conversation");
  await view.locator("#conversationSend").click();
  await page.waitForFunction(() => document.querySelector("#conversationInput").value === "");
  assert.equal(calls.filter(c => c.path.endsWith("/turns")).at(-1).body.nodeAccountId, ids[0]);
  assert.equal(await view.locator('.sidebar-account-row[data-account-engine="codex"]').count(),1);
  assert.equal(await view.locator('.sidebar-account-row[data-account-engine="claude"]').count(),2);
  await view.locator("#agentNewThread").click();
  assert.equal(await view.locator(`#conversationAccount option[value="${codexId}"]`).count(),1,"new sessions can select Codex again");
  assert.deepEqual(errors, []);
  console.log("PASS: shared Claude account management, secret clearing, provider models, binding reload and idle account switch");
} finally { await browser.close(); }

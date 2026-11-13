import assert from "node:assert/strict";
import { accountOverviewFixture } from "./account_overview_fixtures.mjs";
// Claude.ai subscription rows show plan windows like Codex quota; API-key
// Claude accounts keep their SDK cost estimate. Account RPCs are simulated.
const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const { nodes, threads, history } = accountOverviewFixture();
const subscriptionId = "30000000-0000-4000-8000-000000000001", apiId = "30000000-0000-4000-8000-000000000002";
const claude = (nodeAccountId, name, authType) => ({ engine: "claude", nodeAccountId, accountId: nodeAccountId, nodeId: nodes[0].nodeId, name, enabled: true,
  configured: true, provider: "anthropic", credentialRevision: 2, authType, email: "", desiredAppServer: {}, reportedAppServer: { provider: { id: "anthropic", model: "opus" }, status: "ready" } });
nodes[0].capabilities.claudeAccountsV1 = true;
nodes[0].claudeAccounts = [claude(subscriptionId, "Max", "subscription"), claude(apiId, "Gateway", "api_key")];
const reset = Date.now() + 3 * 3600_000, weekly = Date.now() + 4 * 86400_000;
let usageCalls = [], failUsage = false, sevenDay = 4;
const browser = await chromium.launch({ headless: true, ...(process.env.MIRA_BROWSER_EXECUTABLE ? { executablePath: process.env.MIRA_BROWSER_EXECUTABLE } : {}) });
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, timezoneId: "Asia/Shanghai" });
  await context.route(/\/v1\/nodes(?:\?|$)/, route => route.fulfill({ json: { data: nodes } }));
  for (const node of nodes) await context.route(`**/v1/nodes/${node.nodeId}`, route => route.fulfill({ json: node }));
  await context.route("**/v1/codex/accounts/cost-history?*", route => route.fulfill({ json: history(new URL(route.request().url()).searchParams.get("range")) }));
  await context.route("**/v1/claude/accounts/cost-history?*", route => route.fulfill({ json: history(new URL(route.request().url()).searchParams.get("range")) }));
  await context.route(/\/v1\/claude\/accounts\/[^/]+\/usage(?:\?|$)/, route => {
    const url = new URL(route.request().url());
    usageCalls.push({ id: url.pathname.split("/")[4], refresh: url.searchParams.get("refresh") });
    if (failUsage) return route.fulfill({ status: 409, json: { error: "Claude account Node is offline", code: "claude_session" } });
    return route.fulfill({ json: { nodeAccountId: subscriptionId, observedAt: Date.now(), stale: false, usage: {
      subscriptionType: "max", available: true, fiveHour: { utilization: 12.5, resetsAt: reset }, sevenDay: { utilization: sevenDay, resetsAt: weekly },
      models: [{ name: "Fable", utilization: 0, resetsAt: weekly }], extra: { enabled: false } } } });
  });
  const historyCalls = [];
  await context.route(/\/v1\/claude\/accounts\/[^/]+\/usage-history/, route => {
    const url = new URL(route.request().url()), range = url.searchParams.get("range"), to = Date.now();
    historyCalls.push({ id: url.pathname.split("/")[4], range });
    const from = to - { "24h": 86400_000, "7d": 7 * 86400_000, "30d": 30 * 86400_000 }[range];
    const points = Array.from({ length: 289 }, (_, i) => ({ at: to - 86400_000 + i * 300_000, status: i === 100 ? "error" : "ok",
      remaining: i === 100 ? null : 100 - i * 0.014, session: 90, resetsAt: weekly }));
    return route.fulfill({ json: { nodeAccountId: subscriptionId, range, from, to, intervalMs: 300_000, points } });
  });
  const claudeThreads = [{ threadId: subscriptionId.replace(/^3/, "4"), sessionId: subscriptionId.replace(/^3/, "4"), engine: "claude", title: "Claude plan",
    cwd: "/work", runtimeNodeId: nodes[0].nodeId, nodeAccountId: subscriptionId, updatedAt: new Date().toISOString(), generation: 1, itemCount: 0 }];
  await context.route("**/v1/claude/conversations?*", route => route.fulfill({ json: { data: claudeThreads, paged: true, projects: [] } }));
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data: threads } }));
  for (const thread of threads) {
    await context.route(`**/v1/codex/threads/${thread.threadId}?*`, route => route.fulfill({ json: thread }));
    await context.route(`**/v1/codex/threads/${thread.threadId}/transcript?*`, route => route.fulfill({ json: { generation: 1, itemCount: 0, trace: [] } }));
  }
  const page = await context.newPage(), errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?thread=${threads[0].threadId}`);
  const rows = page.locator(".sidebar-account-row");
  await rows.first().waitFor({ state: "attached" });
  if (await page.locator("[data-account-more]").isVisible()) await page.locator("[data-account-more]").click();
  const row = rows.filter({ hasText: /^Claude · Max/ });
  await page.waitForFunction(() => [...document.querySelectorAll(".sidebar-account-row")].some(row => row.textContent === "Claude · Max剩余 5h 87.5% · 周 96%"));
  assert.match(await row.getAttribute("title"), /5 小时 .*重置；本周 · 全部模型 .*重置；本周 · Fable/);
  await page.waitForFunction(() => /7 天/.test([...document.querySelectorAll(".sidebar-account-row")].find(row => row.textContent.startsWith("Claude · Gateway"))?.textContent ?? ""));
  assert.deepEqual([...new Set(usageCalls.map(call => call.id))], [subscriptionId], "API-key Claude accounts never request plan usage");

  await row.click();
  const usage = page.locator("[data-account-plan-usage]");
  await usage.locator(".account-plan-row").first().waitFor();
  assert.deepEqual(await usage.locator(".account-plan-row > div").allTextContents(), ["5 小时剩余 87.5%", "本周 · 全部模型剩余 96%", "本周 · Fable剩余 100%"]);
  assert.equal(await usage.locator("meter").first().evaluate(meter => meter.value), 87.5);
  assert.match(await usage.locator(".account-plan-row small").first().textContent(), /^\d\d\/\d\d \d\d:\d\d 重置$/);
  assert.equal(await page.locator("[data-account-plan]").textContent(), "MAX");
  assert.equal(await page.locator("[data-account-spend]").isVisible(), false, "subscriptions chart quota history instead of estimated cost");
  const chart = page.locator("[data-account-history] .quota-chart:not(.hidden)");
  await chart.waitFor();
  assert.equal(await page.locator("[data-account-history] .quota-line").count(), 1, "the Claude weekly quota is drawn as a step line");
  assert.ok(historyCalls.every(call => call.id === subscriptionId));
  await chart.focus(); await page.keyboard.press("End");
  assert.match(await page.locator("[data-account-history] .quota-tooltip").textContent(), /剩余 96%/);
  await page.locator("[data-account-history] .quota-chart").blur();
  await page.screenshot({ path: process.env.MIRA_SCREENSHOT ?? "/tmp/claude-plan-usage.png" });

  // Manual refresh asks the Server to bypass its cache; a failure keeps the last windows.
  failUsage = true;
  await page.locator("[data-account-refresh]").click();
  await page.waitForFunction(() => /更新失败，显示上次结果：Claude account Node is offline/.test(document.querySelector("[data-account-plan-usage]").textContent));
  assert.equal(usageCalls.at(-1).refresh, "1");
  assert.equal(await usage.locator(".account-plan-row").count(), 3);
  await page.getByRole("button", { name: "关闭账户详情", exact: true }).click();
  assert.equal(await row.textContent(), "Claude · Max剩余 5h 87.5% · 周 96% · 上次结果");

  await page.setViewportSize({ width: 390, height: 844 });
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1));
  assert.deepEqual(errors, []);
  console.log("PASS: Claude subscription plan windows in the sidebar and popover, API cost estimates and refresh fallback");
} finally { await browser.close(); }

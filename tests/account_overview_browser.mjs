import assert from "node:assert/strict";
import { accountOverviewFixture } from "./account_overview_fixtures.mjs";
const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const { nodes, threads, history } = accountOverviewFixture();
const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  await context.route(/\/v1\/nodes(?:\?|$)/, route => route.fulfill({ json: { data: nodes } }));
  for (const node of nodes) {
    await context.route(`**/v1/nodes/${node.nodeId}`, route => route.fulfill({ json: node }));
    await context.route(`**/v1/nodes/${node.nodeId}/codex-accounts/*/quota-history?*`, route => route.fulfill({ json: { account: node.codexAccounts[0].snapshot.account, points: [] } }));
  }
  const costCalls = [];
  await context.route("**/v1/codex/accounts/cost-history?*", route => {
    const query = new URL(route.request().url()).searchParams; costCalls.push(query.get("name"));
    return route.fulfill({ json: history(query.get("range")) });
  });
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data: threads } }));
  for (const thread of threads) {
    await context.route(`**/v1/codex/threads/${thread.threadId}?*`, route => route.fulfill({ json: thread }));
    await context.route(`**/v1/codex/threads/${thread.threadId}/transcript?*`, route => route.fulfill({ json: { generation: 1, itemCount: 1, trace: [{ kind: "assistant", key: "prose", body: "Persisted conversation", sourceItemSeq: 1 }] } }));
  }
  const page = await context.newPage(), errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?thread=${threads[0].threadId}`);
  const rows = page.locator(".sidebar-account-row"); await rows.first().waitFor();
  assert.equal(await rows.count(), 3);
  await page.waitForFunction(() => document.querySelector('[data-account-name="API"]').textContent.includes("$13.00"));
  assert.match(await rows.filter({ hasText: /^API/ }).textContent(), /7 天 ≥ \$13\.00/);
  assert.equal(await rows.filter({ hasText: "Shared" }).textContent(), "Shared剩余 0%");
  await rows.filter({ hasText: "Shared" }).click();
  assert.equal(await page.locator("[data-account-remaining]").textContent(), "0%");
  assert.match(await page.locator("[data-account-node]").textContent(), /Node 2.*Node 1/);
  await page.getByRole("button", { name: "关闭账户详情", exact: true }).click();
  await rows.filter({ hasText: /^API/ }).click();
  const spend = page.locator("[data-account-spend]");
  await spend.locator(".spend-bar").first().waitFor();
  assert.equal(await spend.locator(".spend-bar").count(), 7);
  assert.equal(await spend.locator(".spend-line").count(), 7);
  await spend.locator("svg").focus(); await page.keyboard.press("End");
  assert.match(await spend.locator(".quota-tooltip").textContent(), /累计 \$/);
  await spend.locator("select").selectOption("30d");
  await page.waitForFunction(() => document.querySelectorAll(".spend-bar").length === 30);
  assert.ok(costCalls.includes("API"));
  await page.getByRole("button", { name: "关闭账户详情", exact: true }).click();
  const account = await page.locator("#conversationAccount").boundingBox(), attach = await page.locator("#conversationAttach").boundingBox();
  assert.ok(account.x + account.width <= attach.x, "desktop account is left of attachments");
  for (const width of [600, 390, 320]) {
    await page.setViewportSize({ width, height: 844 });
    assert.equal(await page.locator("#conversationAccount").isVisible(), false);
    assert.equal(await page.locator("#conversationAttach").isVisible(), true);
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1));
  }
  await page.locator("#conversationDetailsToggle").click();
  await page.locator("#conversationDetailsAccount").selectOption(nodes[0].codexAccounts[1].nodeAccountId);
  assert.equal(await page.locator("#conversationAccount").inputValue(), nodes[0].codexAccounts[1].nodeAccountId);
  await page.locator("#conversationDetailsClose").click();
  await page.setViewportSize({ width: 1200, height: 900 });
  await page.locator("#conversationAccount").waitFor({ state: "visible" });
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.deepEqual(errors, []);
  console.log("PASS: all-node account grouping, zero balance, overlaid spending chart and desktop/mobile account controls");
} finally { await browser.close(); }

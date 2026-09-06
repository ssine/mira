import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { sidebarAction } from "./sidebar_browser_helpers.mjs";
const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const ids = ["10000000-0000-4000-8000-000000000001", "10000000-0000-4000-8000-000000000002", "10000000-0000-4000-8000-000000000003"];
const rows = ids.map((threadId, index) => ({ threadId, title: ["Large conversation", "New conversation", "Unknown usage"][index],
  cwd: "/work", generation: 1, itemCount: 1, updatedAt: new Date().toISOString(),
  activity: { state: "idle", generation: 1, itemCount: 1 }, tokenUsage: index === 2 ? null :
    { inputTokens: index ? 0 : 125000, outputTokens: index ? 0 : 8000, cachedInputTokens: index ? 0 : 100000 } }));
const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data: rows } }));
  await context.route(/\/v1\/codex\/threads\/[^?]+\?/, route => {
    const path = new URL(route.request().url()).pathname, row = rows.find(row => path.includes(row.threadId));
    return route.fulfill({ json: path.endsWith("/transcript") ? { generation: row.generation, itemCount: row.itemCount,
      trace: [{ key: "message", kind: "assistant", body: "A persisted conversation.", sourceItemSeq: 1 }], nextCursor: null } : row });
  });
  const page = await context.newPage(), errors = [];
  page.setDefaultTimeout(15_000); page.on("pageerror", error => errors.push(error.message));
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?thread=${ids[0]}`);
  const summary = page.locator(`[data-thread-token-usage="${ids[0]}"]`);
  await summary.waitFor({ state: "visible" });
  assert.equal(await summary.textContent(), "125k in · 8k out");
  assert.match(await summary.getAttribute("title"), /缓存输入 100,000 Token/);
  assert.equal(await page.locator(`[data-thread-token-usage="${ids[1]}"]`).textContent(), "0 in · 0 out");
  assert.equal(await page.locator(`[data-thread-token-usage="${ids[2]}"]`).isVisible(), false);
  const row = page.locator(`[data-thread-row="${ids[0]}"]`);
  const height = (await row.boundingBox()).height;
  const openDetails = async id => { await page.locator(`[data-thread-row="${id}"]`).hover(); await page.locator(`[data-thread-menu="${id}"]`).click(); await page.locator("#threadShowDetails").click(); await page.waitForFunction(() => document.querySelector("#conversationDetailsStatus")?.textContent === ""); };
  await openDetails(ids[0]);
  const facts = page.locator("#conversationTokenUsageFacts");
  assert.deepEqual(await facts.locator("dd").allTextContents(), ["125,000", "100,000", "8,000"]);
  assert.match(await facts.textContent(), /含缓存/);
  // Metadata can change before the history count advances. Both views update in place.
  rows[0].tokenUsage = { inputTokens: 250000, cachedInputTokens: 200000, outputTokens: 16000 };
  await page.waitForFunction(id => document.querySelector(`[data-thread-token-usage="${id}"]`)?.textContent === "250k in · 16k out", ids[0]);
  assert.deepEqual(await facts.locator("dd").allTextContents(), ["250,000", "200,000", "16,000"]);
  assert.equal((await row.boundingBox()).height, height, "usage stays on the existing second row");
  if (process.env.MIRA_WEB_SCREENSHOT_DIR) {
    await fs.mkdir(process.env.MIRA_WEB_SCREENSHOT_DIR, { recursive: true });
    await page.screenshot({ path: `${process.env.MIRA_WEB_SCREENSHOT_DIR}/thread-token-usage-desktop.png` });
  }
  await page.locator("#conversationDetailsClose").click();
  await openDetails(ids[2]);
  assert.deepEqual(await facts.locator("dd").allTextContents(), ["未提供", "未提供", "未提供"]);
  await page.locator("#conversationDetailsClose").click();
  // Replaced history must clear the old generation, even when counters decrease.
  rows[0].generation = 2; rows[0].tokenUsage = { inputTokens: 1000, cachedInputTokens: 0, outputTokens: 100 };
  await page.waitForFunction(id => document.querySelector(`[data-thread-token-usage="${id}"]`)?.textContent === "1k in · 100 out", ids[0]);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator("#agentThreadDrawerToggle").click();
  await summary.waitFor({ state: "visible" });
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  await sidebarAction(page, "agentThemeToggle");
  await openDetails(ids[0]);
  assert.deepEqual(await facts.locator("dd").allTextContents(), ["1,000", "0", "100"]);
  if (process.env.MIRA_WEB_SCREENSHOT_DIR) await page.screenshot({ path: `${process.env.MIRA_WEB_SCREENSHOT_DIR}/thread-token-usage-mobile.png` });
  await page.locator("#conversationDetailsClose").click();
  await page.setViewportSize({ width: 320, height: 844 });
  assert.equal(await summary.isVisible(), false, "narrow rows hide the summary while details remain available");
  assert.equal(await page.locator(`[data-thread-activity="${ids[0]}"]`).isVisible(), true);
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  assert.deepEqual(errors, []);
  console.log("PASS: compact token summaries, exact details, cache inclusion, zero/unknown, metadata-only polling, generation replacement, stable row height and narrow/mobile/dark layout");
} finally { await browser.close(); }

// Real browser + disposable Server. Use MIRA_BROWSER_CHANNEL=msedge for native Edge.
// node tests/web_windows_browser.mjs [playwright-module]
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";

const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8789";
const threads = [
  { threadId: "00000000-0000-4000-8000-0000000000a1", title: "第一个窗口", itemCount: 40 },
  { threadId: "00000000-0000-4000-8000-0000000000b2", title: "第二个窗口", itemCount: 40 },
].map((thread) => ({ ...thread, cwd: "/workspace", model: "gpt-6-astra", updatedAt: new Date().toISOString() }));
const profile = await fs.mkdtemp(path.join(os.tmpdir(), "mira-windows-browser-"));
let context;
try {
  context = await chromium.launchPersistentContext(profile, { headless: true, viewport: { width: 1440, height: 900 },
    ...(process.env.MIRA_BROWSER_CHANNEL ? { channel: process.env.MIRA_BROWSER_CHANNEL } : {}),
    ...(process.env.MIRA_BROWSER_EXECUTABLE ? { executablePath: process.env.MIRA_BROWSER_EXECUTABLE } : {}) });
  const errors = [];
  context.on("page", (page) => page.on("pageerror", (error) => errors.push(error.message)));
  await context.route("**/v1/codex/threads?*", (route) => route.fulfill({ json: { data: threads } }));
  await context.route("**/v1/codex/threads/*/transcript?*", (route) => {
    const threadId = new URL(route.request().url()).pathname.split("/").at(-2);
    return route.fulfill({ json: { generation: 1, trace: Array.from({ length: 40 }, (_, i) => ({
      key: `${threadId}-${i}`, kind: "assistant", body: `窗口 ${threadId} 中的消息 ${i}。`, turnId: `turn-${i}`,
    })) } });
  });
  const page = await context.newPage();
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator("#loginForm button[type=submit]").click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?thread=${threads[0].threadId}`);
  await page.locator(".trace-card.assistant").first().waitFor();
  assert.equal(await page.title(), "第一个窗口 · Mira");
  await page.locator("#conversationInput").fill("原窗口里未发送的草稿");
  await page.locator("#conversationScroll").evaluate((scroll) => { scroll.scrollTop = 500; });
  const position = await page.locator("#conversationScroll").evaluate((scroll) => scroll.scrollTop);
  if (await page.locator("#agentThreadDrawer").getAttribute("aria-hidden") === "true") await page.locator("#agentThreadDrawerToggle").click();
  assert.equal(await page.locator('[data-open-thread-window]').count(), 0, 'new-window actions do not occupy conversation rows');
  const row = page.locator(`[data-thread-row="${threads[1].threadId}"]`);
  await row.hover();
  await row.locator('[data-thread-menu]').click();
  const open = page.locator('#threadOpenWindow');
  assert.equal(await open.getAttribute("href"), `/?thread=${threads[1].threadId}`);
  const popupReady = context.waitForEvent("page");
  await open.click();
  const popup = await popupReady;
  await popup.locator(".trace-card.assistant").first().waitFor();
  assert.equal(await popup.title(), "第二个窗口 · Mira");
  assert.equal(new URL(popup.url()).searchParams.get("thread"), threads[1].threadId);
  assert.equal(await popup.evaluate(() => window.opener === null), true);
  assert.equal(await popup.locator("#conversationInput").inputValue(), "");
  await popup.locator("#conversationInput").fill("新窗口自己的草稿");
  assert.equal(await page.locator("#conversationInput").inputValue(), "原窗口里未发送的草稿");
  assert.equal(await page.locator("#conversationScroll").evaluate((scroll) => scroll.scrollTop), position);
  assert.equal(new URL(page.url()).searchParams.get("thread"), threads[0].threadId);
  // Opening the same thread twice creates independent windows, never navigates a named window.
  await row.hover();
  await row.locator('[data-thread-menu]').click();
  const thirdReady = context.waitForEvent("page");
  await open.click();
  const third = await thirdReady;
  await third.locator(".trace-card.assistant").first().waitFor();
  assert.notEqual(third, popup);
  assert.equal(await popup.locator("#conversationInput").inputValue(), "新窗口自己的草稿");
  await third.close();
  // A sibling window changes the shared cold-launch route. Explicit URLs still win on reload.
  await page.reload();
  await page.locator(".trace-card.assistant").first().waitFor();
  assert.equal(await page.title(), "第一个窗口 · Mira");
  await popup.reload();
  await popup.locator(".trace-card.assistant").first().waitFor();
  assert.equal(await popup.title(), "第二个窗口 · Mira");
  assert.equal(await popup.locator("#loginView").isVisible(), false, "windows share the administrator login");
  if (await popup.locator("#agentThreadDrawer").getAttribute("aria-hidden") === "true") await popup.locator("#agentThreadDrawerToggle").click();
  await popup.locator(`button[data-thread-id="${threads[0].threadId}"]`).click();
  await popup.waitForFunction(() => document.title === "第一个窗口 · Mira");
  assert.deepEqual(errors, []);
  console.log(`PASS: ${process.env.MIRA_BROWSER_CHANNEL ?? "Chromium"} browser: distinct windows, deep links, shared login, independent drafts/scroll, reload and window titles`);
} finally {
  await context?.close();
  await fs.rm(profile, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 });
}

import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { sidebarAction } from "./sidebar_browser_helpers.mjs";

const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const now = Date.now();
const row = (project, index, ageDays) => ({
  threadId: `${project === "/alpha" ? "10000000" : "20000000"}-0000-4000-8000-${String(index).padStart(12, "0")}`,
  title: `${project} conversation ${index}`,
  cwd: project,
  generation: 1,
  itemCount: 1,
  updatedAt: new Date(now - ageDays * 24 * 60 * 60 * 1000).toISOString(),
});
const threads = [
  ...[0, 1, 2, 3, 4, 5, 8].map((days, index) => row("/alpha", index + 1, days)),
  ...[0, 1, 2, 3, 4].map((days, index) => row("/beta", index + 1, days)),
];
const parent = threads[0], oldParent = threads[6];
parent.updatedAt = new Date(now + 1).toISOString();
const child = (index, parentThreadId, ageDays = 0) => ({ ...row("/alpha", index, ageDays), parentThreadId, sourceKind: null,
  activity: { state: "idle", generation: 1, itemCount: 1 },
  readState: { generation: 1, latestItemSeq: 1, readItemCount: 1 } });
const firstChild = { ...child(21, parent.threadId), cwd: "/other-workspace", runtimeNodeId: "child-node",
  activity: { state: "running", turnId: "child-turn", generation: 1, itemCount: 1 },
  readState: { generation: 1, latestItemSeq: 1, readItemCount: 0 } };
const grandchild = child(31, firstChild.threadId);
const oldChild = child(41, oldParent.threadId, 9);
threads.push(firstChild, child(22, parent.threadId), child(23, parent.threadId), grandchild, oldChild);
// Ordinary forks stay independent conversations.
threads[4].forkedFromId = parent.threadId;
const archivedThreads = [];

const browser = await chromium.launch({ headless: true,
  ...(process.env.MIRA_BROWSER_EXECUTABLE ? { executablePath: process.env.MIRA_BROWSER_EXECUTABLE } : {}) });
let page;
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  await context.route("**/v1/codex/threads?*", route => {
    const archived = new URL(route.request().url()).searchParams.get("archived") === "1";
    return route.fulfill({ json: { data: archived ? archivedThreads : threads } });
  });
  const reads = [], costs = [];
  await context.route(/\/v1\/codex\/threads\/[^?]+\?/, route => {
    const url = new URL(route.request().url());
    const thread = [...threads, ...archivedThreads].find(thread => url.pathname.includes(thread.threadId));
    if (!thread) return route.fulfill({ status: 404, json: { error: "thread_not_found" } });
    if (url.pathname.endsWith("/read")) {
      reads.push(thread.threadId);
      thread.readState = { generation: 1, latestItemSeq: 1, readItemCount: 1 };
      return route.fulfill({ json: { threadId: thread.threadId, ...thread.readState } });
    }
    if (url.searchParams.has("includeCost")) costs.push(thread.threadId);
    if (url.pathname.endsWith("/transcript")) return route.fulfill({ json: { generation: 1, itemCount: 1,
      trace: [{ key: "message", kind: "assistant", body: `History ${thread.threadId}`, sourceItemSeq: 1 }], nextCursor: null } });
    return route.fulfill({ json: thread });
  });
  page = await context.newPage();
  const errors = [];
  let sockets = 0;
  page.on("websocket", () => sockets++);
  page.setDefaultTimeout(10_000);
  page.on("pageerror", error => { errors.push(error.message); console.error(error.message); });
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?thread=${parent.threadId}`);
  await page.getByText(`History ${parent.threadId}`, { exact: true }).waitFor();
  const projects = page.locator(".thread-project");
  await projects.first().waitFor();
  assert.equal(await projects.count(), 2);
  for (const project of [projects.nth(0), projects.nth(1)]) {
    assert.equal(await project.locator(".thread-project-threads > .agent-thread-row").count(), 4);
  }
  const alpha = projects.filter({ has: page.locator(".thread-project-identity strong", { hasText: "alpha" }) });
  const beta = projects.filter({ has: page.locator(".thread-project-identity strong", { hasText: "beta" }) });
  const alphaHistory = alpha.locator(".thread-project-history");
  const betaHistory = beta.locator(".thread-project-history");
  const subagents = page.locator(`[data-subagent-parent="${parent.threadId}"]`);
  const nested = page.locator(`[data-subagent-parent="${firstChild.threadId}"]`);
  const summary = subagents.locator(":scope > summary");
  const threadButton = id => page.locator(`[data-thread-id="${id}"]`);
  assert.equal(await subagents.getAttribute("open"), null, "subagents start collapsed");
  assert.equal(await subagents.locator(":scope > summary .thread-subagents-label").textContent(), "子 Agent · 4");
  assert.equal(await subagents.locator(":scope > summary .thread-subagents-activity").textContent(), "1 进行中 · 1 未读");
  assert.equal(await threadButton(firstChild.threadId).isVisible(), false);
  assert.equal(await alpha.locator(`[data-thread-id="${firstChild.threadId}"]`).count(), 1, "cross-Node/cwd children follow the parent project");
  assert.equal(await alphaHistory.locator(":scope > summary").textContent(), "展开 3 个隐藏对话");
  assert.equal(await betaHistory.locator("summary").textContent(), "展开 1 个隐藏对话");
  assert.equal(await alphaHistory.locator(".agent-thread-row").first().isVisible(), false);
  await alphaHistory.locator(":scope > summary").click();
  await alphaHistory.locator(":scope > summary").getByText("收起 3 个较早对话", { exact: true }).waitFor();
  assert.equal(await alphaHistory.locator(":scope > summary").textContent(), "收起 3 个较早对话");
  assert.equal(await alphaHistory.locator(".agent-thread-row:visible").count(), 3);
  assert.equal(await betaHistory.locator(".agent-thread-row").first().isVisible(), false);

  // A normal in-page list reload keeps the user's expanded project history.
  await page.locator("#agentNavMenuToggle").click();
  await page.locator("#agentArchiveToggle").click();
  await page.waitForFunction(() => document.querySelectorAll("[data-thread-row]").length === 0);
  await page.locator("#agentNavMenuToggle").click();
  await page.locator("#agentArchiveToggle").click();
  await projects.first().waitFor();
  assert.equal(await alpha.locator(".thread-project-history").getAttribute("open"), "");

  await summary.focus();
  await page.keyboard.press("Enter");
  await threadButton(firstChild.threadId).waitFor({ state: "visible" });
  assert.equal(await threadButton(grandchild.threadId).isVisible(), false, "nested subagents have their own disclosure");
  await nested.locator(":scope > summary").click();
  await threadButton(grandchild.threadId).click();
  await page.waitForURL(`**/?thread=${grandchild.threadId}`);
  await page.getByText(`History ${grandchild.threadId}`, { exact: true }).waitFor();
  assert.equal(await threadButton(grandchild.threadId).getAttribute("aria-current"), "page");
  await page.locator(`[data-thread-row="${grandchild.threadId}"]`).hover();
  await page.locator(`[data-thread-menu="${grandchild.threadId}"]`).click();
  assert.equal(await page.locator("#threadOptionsMenu").evaluate(element => element.matches(":popover-open")), true);
  await page.keyboard.press("Escape");

  // Collapsing selected descendants remains a deliberate user choice on list refresh.
  await summary.click();
  await threadButton(grandchild.threadId).waitFor({ state: "hidden" });
  await sidebarAction(page, "agentArchiveToggle");
  await page.waitForFunction(() => document.querySelectorAll("[data-thread-row]").length === 0);
  await sidebarAction(page, "agentArchiveToggle");
  await summary.waitFor();
  assert.equal(await subagents.getAttribute("open"), null);

  // New children appear during polling without reopening a collapsed family.
  const newChild = child(24, parent.threadId);
  newChild.tokenUsage = { inputTokens: 100, outputTokens: 20, cachedInputTokens: 0 };
  threads.push(newChild);
  await threadButton(newChild.threadId).waitFor({ state: "attached" });
  assert.equal(await subagents.getAttribute("open"), null);
  assert.equal(await subagents.locator(":scope > summary .thread-subagents-label").textContent(), "子 Agent · 5");
  assert.equal(costs.includes(newChild.threadId), false, "collapsed children do not request cost details");
  assert.equal(reads.includes(firstChild.threadId), false, "folding/expanding does not mark children read");

  // Status updates keep the summary DOM and keyboard focus intact.
  await summary.focus();
  firstChild.activity.state = "failed";
  await page.waitForFunction(id => document.querySelector(`[data-subagent-parent="${id}"] > summary .thread-subagents-activity`)?.textContent === "1 失败 · 1 未读", parent.threadId);
  assert.equal(await summary.evaluate(element => document.activeElement === element), true);

  await page.reload();
  await threadButton(grandchild.threadId).waitFor({ state: "visible" });
  assert.equal(await subagents.getAttribute("open"), "", "direct routes reveal the parent path");
  assert.equal(await nested.getAttribute("open"), "");

  if (process.env.MIRA_WEB_SCREENSHOT_DIR) {
    await fs.mkdir(process.env.MIRA_WEB_SCREENSHOT_DIR, { recursive: true });
    await page.screenshot({ path: `${process.env.MIRA_WEB_SCREENSHOT_DIR}/subagent-list-light.png` });
  }
  await page.goto(`${origin}/?thread=${oldChild.threadId}`);
  await threadButton(oldChild.threadId).waitFor({ state: "visible" });
  assert.equal(await alphaHistory.getAttribute("open"), "", "direct routes reveal older parent history too");

  await sidebarAction(page, "agentThemeToggle");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.waitForFunction(() => document.querySelector("#agentThreadDrawer")?.getAttribute("aria-hidden") === "true");
  await page.locator("#agentThreadDrawerToggle").click();
  await page.locator("#agentThreadDrawer").evaluate(element => Promise.all(element.getAnimations().map(animation => animation.finished.catch(() => {}))));
  await summary.waitFor({ state: "visible" });
  assert.ok((await summary.boundingBox()).height >= 44, "mobile disclosure is a touch target");
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  assert.equal(await page.locator("html").getAttribute("data-theme"), "dark");
  if (process.env.MIRA_WEB_SCREENSHOT_DIR) await page.screenshot({ path: `${process.env.MIRA_WEB_SCREENSHOT_DIR}/subagent-list-mobile-dark.png` });

  // A parent outside the archive filter cannot make its child disappear.
  const archivedChild = { ...child(51, parent.threadId), archived: true };
  archivedThreads.push(archivedChild);
  await sidebarAction(page, "agentArchiveToggle");
  await threadButton(archivedChild.threadId).waitFor({ state: "visible" });
  assert.equal(await page.locator(".thread-project-threads > .agent-thread-row").count(), 1);
  assert.equal(await threadButton(parent.threadId).count(), 0);
  assert.equal(sockets, 0, "browsing subagent history never starts an App Server");
  assert.deepEqual(errors, []);
  console.log("PASS: recent root conversations, nested subagents, live discovery/status, selection paths, archive fallbacks and mobile disclosure");
} catch (error) {
  if (page && process.env.MIRA_WEB_SCREENSHOT_DIR) {
    await fs.mkdir(process.env.MIRA_WEB_SCREENSHOT_DIR, { recursive: true });
    await page.screenshot({ path: `${process.env.MIRA_WEB_SCREENSHOT_DIR}/subagent-list-failure.png` });
  }
  throw error;
} finally {
  await browser.close();
}

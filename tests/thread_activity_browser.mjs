import { sidebarAction } from "./sidebar_browser_helpers.mjs";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8789";
const runningId = "00000000-0000-4000-8000-0000000000a1", idleId = "00000000-0000-4000-8000-0000000000a2";
const otherId = "00000000-0000-4000-8000-0000000000a3";
let browser;
try {
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const rows = [
    { threadId: runningId, title: "Running conversation", cwd: "/work", generation: 1, itemCount: 1, activity: { state: "running", turnId: "turn-a", generation: 1, itemCount: 1 } },
    { threadId: idleId, title: "Finished conversation", cwd: "/work", generation: 1, itemCount: 1, activity: { state: "idle", turnId: "turn-b", generation: 1, itemCount: 2 } },
    { threadId: otherId, title: "Windows conversation", cwd: "C:\\work", generation: 1, itemCount: 1, activity: { state: "running", turnId: "turn-windows", generation: 1, itemCount: 1 } },
  ];
  for (const row of rows) row.readState = { generation: 1, latestItemSeq: 1, readItemCount: 1, unread: false };
  let calls = 0, sockets = 0;
  const acknowledgments = [];
  const bodies = new Map(rows.map(row => [row.threadId, "A persisted conversation."]));
  await context.route("**/v1/codex/threads?*", route => { calls++; return route.fulfill({ json: { data: rows } }); });
  await context.route(/\/v1\/codex\/threads\/[^?]+\?/, route => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/read")) {
      const row = rows.find(row => path.includes(row.threadId));
      const position = route.request().postDataJSON();
      acknowledgments.push({ threadId: row.threadId, ...position });
      row.readState.readItemCount = Math.max(row.readState.readItemCount, position.itemCount);
      row.readState.unread = row.readState.latestItemSeq > row.readState.readItemCount;
      return route.fulfill({ json: { threadId: row.threadId, ...row.readState } });
    }
    if (path.endsWith("/transcript")) {
      const row = rows.find(row => path.includes(row.threadId));
      return route.fulfill({ json: { generation: 1, itemCount: row.itemCount, storeVersion: row.itemCount, trace: [{ key: "prose", kind: "assistant", body: bodies.get(row.threadId), turnId: "previous-turn", sourceItemSeq: row.itemCount }] } });
    }
    return route.fulfill({ json: rows.find(row => path.endsWith(row.threadId)) });
  });
  const page = await context.newPage();
  page.setDefaultTimeout(20_000);
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  page.on("websocket", () => sockets++);
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?thread=${runningId}`);
  const status = page.locator("#conversationActivity");
  const runningLabel = page.locator(`[data-thread-activity="${runningId}"]`);
  const idleLabel = page.locator(`[data-thread-activity="${idleId}"]`);
  await status.waitFor({ state: "visible" });
  assert.equal(await runningLabel.getAttribute("data-state"), "running");
  assert.equal(await runningLabel.textContent(), "进行中");
  assert.equal(await page.locator(".thread-activity-icon").count(), 0, "list status uses the whole row, without a spinner");
  assert.equal(await idleLabel.getAttribute("data-state"), "idle");
  assert.equal(sockets, 0, "reading persisted activity never resumes a thread or starts a runtime");
  await page.reload();
  await status.waitFor({ state: "visible" });
  assert.equal(await runningLabel.getAttribute("data-state"), "running", "full reload recovers activity from the Server");
  await page.locator(`[data-thread-id="${idleId}"]`).click();
  await status.waitFor({ state: "hidden" });
  assert.equal(await runningLabel.isVisible(), true, "other running conversations remain visible");
  await page.locator(`[data-thread-id="${runningId}"]`).click();
  await status.waitFor({ state: "visible" });
  const otherWindow = await context.newPage();
  otherWindow.on("pageerror", error => errors.push(error.message));
  otherWindow.on("websocket", () => sockets++);
  await otherWindow.goto(`${origin}/?thread=${otherId}`);
  await otherWindow.locator(".trace-card.assistant").waitFor();
  bodies.set(runningId, "The WSL conversation continued in another client.");
  bodies.set(otherId, "The Windows conversation also continued in another client.");
  rows[0].itemCount = 2; rows[2].itemCount = 2;
  rows[0].activity.itemCount = 2; rows[2].activity.itemCount = 2;
  await page.getByText(bodies.get(runningId), { exact: true }).waitFor();
  await otherWindow.getByText(bodies.get(otherId), { exact: true }).waitFor();
  assert.equal(await page.getByText(bodies.get(otherId), { exact: true }).count(), 0, "windows remain isolated by thread");
  assert.equal(sockets, 0, "all read-only windows follow durable messages independently");
  await otherWindow.close();
  await page.bringToFront();
  // Polling must update in place, preserving focus and the open options menu.
  await page.locator(`[data-thread-menu="${runningId}"]`).click();
  const beforePoll = calls;
  rows[0].activity = { ...rows[0].activity, state: "unknown", reason: "offline" };
  await page.waitForFunction(id => document.querySelector(`[data-thread-activity="${id}"]`)?.dataset.state === "unknown", runningId);
  assert(calls > beforePoll);
  assert.equal(await status.isVisible(), true);
  assert.match(await page.locator("#conversationActivityText").innerText(), /节点离线/);
  assert.equal(await page.locator("#threadOptionsMenu").evaluate(element => element.matches(":popover-open")), true);
  await page.locator("#conversationInput").click();
  rows[0].activity = { ...rows[0].activity, state: "idle", itemCount: 3 };
  rows[0].itemCount = 3;
  await status.waitFor({ state: "hidden" });
  assert.equal(await runningLabel.getAttribute("data-state"), "idle", "completion is picked up without refreshing the page");
  rows[1].activity = { state: "running", turnId: "turn-c", generation: 1, itemCount: 3 };
  await page.waitForFunction(id => document.querySelector(`[data-thread-activity="${id}"]`)?.dataset.state === "running", idleId);
  assert.equal(await idleLabel.getAttribute("data-state"), "running", "another client's new turn appears automatically");
  const idleRow = page.locator(`[data-thread-row="${idleId}"]`);
  rows[1].itemCount = 4;
  rows[1].activity = { ...rows[1].activity, state: "idle", itemCount: 4 };
  rows[1].readState = { generation: 1, latestItemSeq: 4, readItemCount: 1, unread: true };
  await page.waitForFunction(id => document.querySelector(`[data-thread-row="${id}"]`)?.dataset.unread === "true", idleId);
  assert.equal(await idleLabel.textContent(), "已完成 · 未读");
  const unreadBackground = await idleRow.evaluate(row => getComputedStyle(row).backgroundColor);
  assert.ok(await idleRow.locator("strong").evaluate(node => Number(getComputedStyle(node).fontWeight) > 500));
  assert.equal(acknowledgments.some(position => position.threadId === idleId), false, "background conversations stay unread");
  await page.locator(`[data-thread-id="${idleId}"]`).click();
  assert.equal(await idleRow.evaluate(row => getComputedStyle(row).backgroundColor), unreadBackground, "selection keeps the status color");
  assert.notEqual(await idleRow.evaluate(row => getComputedStyle(row).boxShadow), "none", "selection adds an independent outline");
  if (process.env.MIRA_WEB_SCREENSHOT_DIR) {
    await fs.mkdir(process.env.MIRA_WEB_SCREENSHOT_DIR, { recursive: true });
    await page.screenshot({ path: `${process.env.MIRA_WEB_SCREENSHOT_DIR}/thread-states-desktop.png` });
  }
  await page.waitForFunction(id => document.querySelector(`[data-thread-row="${id}"]`)?.dataset.unread === "false", idleId);
  assert.ok(acknowledgments.some(position => position.threadId === idleId && position.itemCount === 4));
  // A second client changes the shared read position, picked up without reload.
  rows[0].readState = { generation: 1, latestItemSeq: 3, readItemCount: 1, unread: true };
  await page.waitForFunction(id => document.querySelector(`[data-thread-row="${id}"]`)?.dataset.unread === "true", runningId);
  rows[0].readState.readItemCount = 3;
  await page.waitForFunction(id => document.querySelector(`[data-thread-row="${id}"]`)?.dataset.unread === "false", runningId);
  // Keep the reader above the newest content while further messages arrive.
  bodies.set(idleId, Array.from({ length: 80 }, (_, i) => `Paragraph ${i + 1}.`).join("\n\n"));
  rows[1].itemCount = 5; rows[1].activity.itemCount = 5;
  rows[1].readState.latestItemSeq = 5;
  await page.getByText("Paragraph 80.", { exact: true }).waitFor();
  await page.waitForFunction(id => document.querySelector(`[data-thread-row="${id}"]`)?.dataset.unread === "false", idleId);
  await page.locator("#conversationScroll").evaluate(scroll => { scroll.scrollTop = 300; });
  rows[1].itemCount = 6; rows[1].activity.itemCount = 6; rows[1].readState.latestItemSeq = 6;
  bodies.set(idleId, bodies.get(idleId) + "\n\nA new unseen result.");
  await page.getByText("A new unseen result.", { exact: true }).waitFor({ state: "attached" });
  await page.waitForTimeout(1100);
  assert.equal(await idleRow.getAttribute("data-unread"), "true", "reading old messages cannot consume the new tail");
  assert.equal(acknowledgments.some(position => position.threadId === idleId && position.itemCount === 6), false);
  await page.locator("#conversationScroll").evaluate(scroll => { scroll.scrollTop = scroll.scrollHeight; });
  await page.waitForFunction(id => document.querySelector(`[data-thread-row="${id}"]`)?.dataset.unread === "false", idleId);
  assert.equal(sockets, 0);
  await page.emulateMedia({ reducedMotion: "reduce" });
  assert.equal(await idleLabel.evaluate(element => getComputedStyle(element).animationName), "none");
  await sidebarAction(page, "agentThemeToggle");
  assert.equal(await idleLabel.isVisible(), true);
  await page.setViewportSize({ width: 412, height: 820 });
  await page.locator("#agentThreadDrawerToggle").click();
  assert.equal(await idleLabel.isVisible(), true);
  rows[1].itemCount = 7; rows[1].activity.itemCount = 7; rows[1].readState.latestItemSeq = 7;
  bodies.set(idleId, bodies.get(idleId) + "\n\nHidden behind the drawer.");
  await page.waitForFunction(id => document.querySelector(`[data-thread-row="${id}"]`)?.dataset.unread === "true", idleId);
  await page.waitForTimeout(1100);
  assert.equal(acknowledgments.some(position => position.threadId === idleId && position.itemCount === 7), false, "the mobile drawer obscures the conversation");
  assert.deepEqual(errors, []);
  console.log("PASS: persisted activity after reload, independent live history in multiple read-only windows, active conversations at a glance, automatic completion/new-turn polling, offline uncertainty, in-place menu/focus preservation, dark/mobile/reduced-motion indicators");
} finally { await browser?.close(); }

import assert from "node:assert/strict";
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
  let calls = 0, sockets = 0;
  const bodies = new Map(rows.map(row => [row.threadId, "A persisted conversation."]));
  await context.route("**/v1/codex/threads?*", route => { calls++; return route.fulfill({ json: { data: rows } }); });
  await context.route(/\/v1\/codex\/threads\/[^?]+\?/, route => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith("/transcript")) {
      const row = rows.find(row => path.includes(row.threadId));
      return route.fulfill({ json: { generation: 1, itemCount: row.itemCount, storeVersion: row.itemCount, trace: [{ key: "prose", kind: "assistant", body: bodies.get(row.threadId), turnId: "previous-turn", sourceItemSeq: row.itemCount }] } });
    }
    return route.fulfill({ json: rows.find(row => path.endsWith(row.threadId)) });
  });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  page.on("websocket", () => sockets++);
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?thread=${runningId}`);
  const status = page.locator("#conversationActivity");
  const runningIcon = page.locator(`[data-thread-activity="${runningId}"]`);
  const idleIcon = page.locator(`[data-thread-activity="${idleId}"]`);
  await status.waitFor({ state: "visible" });
  assert.equal(await runningIcon.getAttribute("data-state"), "running");
  assert.equal(await runningIcon.getAttribute("aria-label"), "Codex 正在运行");
  assert.equal(await idleIcon.isVisible(), false);
  assert.equal(sockets, 0, "reading persisted activity never resumes a thread or starts a runtime");
  await page.reload();
  await status.waitFor({ state: "visible" });
  assert.equal(await runningIcon.getAttribute("data-state"), "running", "full reload recovers activity from the Server");
  await page.locator(`[data-thread-id="${idleId}"]`).click();
  await status.waitFor({ state: "hidden" });
  assert.equal(await runningIcon.isVisible(), true, "other running conversations remain visible");
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
  assert.equal(await runningIcon.isVisible(), false, "completion is picked up without refreshing the page");
  rows[1].activity = { state: "running", turnId: "turn-c", generation: 1, itemCount: 3 };
  await idleIcon.waitFor({ state: "visible" });
  assert.equal(await idleIcon.getAttribute("data-state"), "running", "another client's new turn appears automatically");
  assert.equal(sockets, 0);
  await page.emulateMedia({ reducedMotion: "reduce" });
  assert.equal(await idleIcon.evaluate(element => getComputedStyle(element).animationName), "none");
  await page.locator("#agentThemeToggle").click();
  assert.equal(await idleIcon.isVisible(), true);
  await page.setViewportSize({ width: 412, height: 820 });
  await page.locator("#agentThreadDrawerToggle").click();
  assert.equal(await idleIcon.isVisible(), true);
  assert.deepEqual(errors, []);
  console.log("PASS: persisted activity after reload, independent live history in multiple read-only windows, active conversations at a glance, automatic completion/new-turn polling, offline uncertainty, in-place menu/focus preservation, dark/mobile/reduced-motion indicators");
} finally { await browser?.close(); }

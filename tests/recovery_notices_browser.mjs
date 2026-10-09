import assert from "node:assert/strict";

const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({ viewport: { width: 1080, height: 900 } });
  const thread = { threadId: "00000000-0000-4000-8000-000000000092", title: "Recovery notice", cwd: "/work", generation: 1, itemCount: 4, updatedAt: new Date().toISOString() };
  const raw = "The encrypted content for item rs_example could not be verified. Reason: Encrypted content could not be decrypted or parsed. (request id: example)";
  let notice = { retryCount: 3, status: "dispatching" };
  await context.route("**/app.js", async route => {
    const response = await route.fetch();
    await route.fulfill({ response, body: (await response.text()).replace("void bootstrap();",
      "window.recoveryRegression = { retainTurnError, handleAgentNotification, renderTranscript, recoveryNotices }; void bootstrap();") });
  });
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data: [thread] } }));
  await context.route(/\/v1\/codex\/threads\/[^?]+\?/, route => {
    const path = new URL(route.request().url()).pathname;
    return route.fulfill({ json: path.endsWith("/automatic-input-recovery") ? {
      enabled: true, generation: 1, turns: { failed: notice, live: notice },
    } : path.endsWith("/transcript") ? {
      generation: 1, storeVersion: 1, itemCount: 4, trace: [
        { key: "reply", turnId: "first", kind: "assistant", body: "Already completed work." },
        { key: "mismatch", turnId: "failed", kind: "error", title: "Turn 失败", status: "失败", body: raw },
        { key: "ordinary", turnId: "other", kind: "error", title: "Turn 失败", status: "失败", body: "Permission denied" },
        { key: "final", turnId: "recovered", kind: "assistant", body: "Recovery finished successfully." },
      ],
    } : thread });
  });
  const page = await context.newPage(), errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?thread=${thread.threadId}`);
  const recovery = page.locator('.trace-card.recovery[data-turn-id="failed"]');
  await recovery.locator(".compaction-label").filter({ hasText: "已自动重试 3 次" }).waitFor();
  assert.equal(await recovery.locator(".trace-body").isVisible(), false);
  assert.equal(await recovery.locator(".trace-body").textContent(), "", "folded raw errors are not rendered");
  assert.equal(await recovery.locator(".trace-detail").getAttribute("open"), null);
  assert.equal(await page.locator(".trace-card.error").count(), 1);
  assert.match(await page.locator(".trace-card.error").textContent(), /Permission denied/);
  await page.evaluate(({ id, raw }) => {
    window.recoveryRegression.retainTurnError(id, "live", raw);
    window.recoveryRegression.retainTurnError(id, "live", "Codex Turn 执行失败");
  }, { id: thread.threadId, raw });
  const live = page.locator('.trace-card.recovery[data-turn-id="live"]');
  await live.locator(".compaction-label").filter({ hasText: "已自动重试 3 次" }).waitFor();
  assert.equal(await page.locator(".trace-card.error").count(), 1, "live and persisted encrypted errors share the neutral renderer");
  await recovery.locator("summary").click();
  await page.waitForFunction(() => document.querySelector('.trace-card.recovery[data-turn-id="failed"] .trace-body').textContent.includes("request id: example"));
  assert.match(await recovery.locator(".trace-body").textContent(), /request id: example/);
  notice = { retryCount: 19, status: "stopped", reason: "同一份上下文已连续失败 20 次" };
  await recovery.locator(".compaction-label").filter({ hasText: "自动重试已停止" }).waitFor();
  assert.notEqual(await recovery.locator(".trace-detail").getAttribute("open"), null, "status updates preserve expanded details");
  await recovery.locator("summary").click();
  await page.setViewportSize({ width: 390, height: 844 });
  assert.equal(await recovery.locator(".trace-body").isVisible(), false);
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1));
  // Replayed live failures must reconcile at their original chronological
  // position, and never accumulate below a later successful final response.
  await recovery.locator("summary").click();
  await page.evaluate(({ id, raw }) => {
    window.recoveryRegression.retainTurnError(id, "failed", raw);
    window.recoveryRegression.retainTurnError(id, "failed", raw);
    window.recoveryRegression.renderTranscript(null);
  }, { id: thread.threadId, raw });
  assert.equal(await recovery.count(), 1, "repeated live/canonical errors were duplicated");
  assert.notEqual(await recovery.locator(".trace-detail").getAttribute("open"), null, "reconciliation closed the user's expanded recovery details");
  await recovery.locator("summary").click();
  assert(await recovery.evaluate(card => !!(card.compareDocumentPosition(document.querySelector('[data-trace-key="final"]')) & Node.DOCUMENT_POSITION_FOLLOWING)), "old recovery error moved after final response");
  notice = { retryCount: 3, status: "completed", resolved: true };
  await page.evaluate(() => { window.recoveryRegression.recoveryNotices.reset(); return window.recoveryRegression.recoveryNotices.refresh(); });
  await recovery.locator(".compaction-label").filter({ hasText: "本次重试已结束" }).waitFor();
  assert.equal(await recovery.locator(".trace-body").isVisible(), false);
  await page.reload();
  await recovery.locator(".compaction-label").filter({ hasText: "本次重试已结束" }).waitFor();
  assert.equal(await recovery.count(), 1);
  assert(await recovery.evaluate(card => !!(card.compareDocumentPosition(document.querySelector('[data-trace-key="final"]')) & Node.DOCUMENT_POSITION_FOLLOWING)));
  assert.equal(await page.locator(".trace-card.error").count(), 1, "ordinary failure was hidden during recovery cleanup");
  // Ephemeral connection warnings clear on actual resumed output or completion.
  await page.evaluate(id => {
    const notify = (method, params) => window.recoveryRegression.handleAgentNotification({ method, params: { threadId: id, ...params } });
    notify("error", { turnId: "transient", willRetry: true, message: "temporary connection failure" });
  }, thread.threadId);
  assert.equal(await page.locator('[data-transient-retry="true"]').count(), 1);
  await page.evaluate(id => {
    const notify = (method, params) => window.recoveryRegression.handleAgentNotification({ method, params: { threadId: id, ...params } });
    notify("item/agentMessage/delta", { turnId: "unrelated", itemId: "unrelated", delta: "Other turn" });
  }, thread.threadId);
  assert.equal(await page.locator('[data-transient-retry="true"]').count(), 1, "unrelated turn cleared the warning");
  await page.evaluate(id => {
    const notify = (method, params) => window.recoveryRegression.handleAgentNotification({ method, params: { threadId: id, ...params } });
    notify("item/agentMessage/delta", { turnId: "transient", itemId: "continued", delta: "Continued output" });
  }, thread.threadId);
  assert.equal(await page.locator('[data-transient-retry="true"]').count(), 0);
  await page.evaluate(id => {
    const notify = (method, params) => window.recoveryRegression.handleAgentNotification({ method, params: { threadId: id, ...params } });
    notify("error", { turnId: "transient", willRetry: true, message: "temporary connection failure" });
    notify("turn/completed", { turn: { id: "transient", status: "completed" } });
    notify("error", { turnId: "transient", willRetry: true, message: "late retry notification" });
  }, thread.threadId);
  assert.equal(await page.locator('[data-transient-retry="true"]').count(), 0);
  assert.deepEqual(errors, []);
  console.log("PASS: neutral recovery notices, durable retry counts, collapsed raw errors, exhaustion and ordinary failures");
} finally {
  await browser.close();
}

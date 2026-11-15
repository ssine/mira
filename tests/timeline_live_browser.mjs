// A running conversation's recent timeline slides continuously (one transform
// per frame, no redraw), keeps its live pulse phase across redraws, and reads
// only increments, triggered by new activity rather than a fixed poll.
import assert from "node:assert/strict";
const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const threadId = "31000000-0000-4000-8000-000000000001";
const minute = 60_000;
const started = Date.now() - 5 * minute;
const row = { threadId, title: "Live timeline", cwd: "/work", generation: 1, itemCount: 10, updatedAt: new Date().toISOString(),
  readState: { generation: 1, latestItemSeq: 10, readItemCount: 10, unread: false },
  activity: { state: "running", turnId: "turn-live", generation: 1, itemCount: 10 } };
const full = { engine: "codex", rootThreadId: threadId, partial: false, truncated: false, delta: false, cursor: `x1.${threadId}:1:10`, lanes: [
  { threadId, parentThreadId: null, title: "Live timeline", depth: 0,
    turns: [{ id: "turn-live", start: started, end: started + minute, open: true }],
    tools: [{ id: "c1", name: "exec_command", start: started + 30_000, end: started + 30_000, open: true, kind: "tool", seq: 4, turnId: "turn-live" }],
    messages: [{ at: started, seq: 1, role: "user", preview: "Run it" }] },
] };
const reads = [];

const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await context.addInitScript(() => {
    try {
      localStorage.setItem("mira.timeline.pinned", "1");
      localStorage.setItem("mira.timeline.mode", "hour");
      localStorage.setItem("mira.timeline.window", String(10 * 60_000));
    } catch { /* Sandboxed frames have no storage. */ }
  });
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data: [row] } }));
  await context.route(/\/v1\/codex\/threads\/[^/?]+(\/[a-z]+)?\?/, route => {
    const url = new URL(route.request().url()), path = url.pathname;
    if (path.endsWith("/timeline")) {
      const cursor = url.searchParams.get("cursor");
      reads.push(cursor);
      if (!cursor) return route.fulfill({ json: full });
      // The increment closes the first tool and starts another.
      const changed = row.itemCount > 10;
      return route.fulfill({ json: { engine: "codex", rootThreadId: threadId, delta: true, partial: false, truncated: false,
        cursor: `x1.${threadId}:1:${row.itemCount}`, lanes: [changed
          ? { threadId, parentThreadId: null, title: "Live timeline", depth: 0,
            turns: [{ id: "turn-live", start: started, end: Date.now() - 2_000, open: true }],
            tools: [{ id: "c1", name: "exec_command", start: started + 30_000, end: Date.now() - 3_000, kind: "tool", seq: 4, turnId: "turn-live" },
              { id: "c2", name: "apply_patch", start: Date.now() - 2_000, end: Date.now() - 2_000, open: true, kind: "tool", seq: 11, turnId: "turn-live" }],
            messages: [] }
          : { threadId, parentThreadId: null, title: "Live timeline", depth: 0, unchanged: true, turns: null, tools: null, messages: null }] } });
    }
    if (path.endsWith("/costs")) return route.fulfill({ json: { threadId, generation: 1, itemCount: row.itemCount, turnCostEstimates: {} } });
    if (path.endsWith("/transcript")) return route.fulfill({ json: { generation: 1, itemCount: row.itemCount, storeVersion: row.itemCount, outline: true, nextCursor: null,
      trace: url.searchParams.get("after") === null ? [{ key: "history-1-user", kind: "user", title: "你", markdown: true, body: "Run it", sourceItemSeq: 1, turnId: "turn-live" }] : [] } });
    return route.fulfill({ json: row });
  });
  const page = await context.newPage(), errors = [];
  page.setDefaultTimeout(15_000);
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?thread=${threadId}`);
  const panel = page.locator("#conversationTimeline");
  await panel.locator(".timeline-pan").waitFor({ state: "attached" });
  assert.equal(reads[0], null, "the first read is whole");
  assert.match(await panel.locator("[data-timeline-summary]").textContent(), /^最近 10 分钟$/);

  // No new activity: after the catch-up read for the running state settles,
  // nothing is read, even though the conversation is running.
  await page.waitForTimeout(3_000);
  const settled = reads.length;
  assert.ok(settled <= 3 && reads.slice(1).every(cursor => cursor), `catch-up reads are increments: ${JSON.stringify(reads)}`);

  // Continuous motion: the content group shifts between frames while the live
  // elements stay the same nodes (no redraw), so their pulse never restarts.
  const sample = () => page.evaluate(() => {
    const pan = document.querySelector("#conversationTimeline .timeline-pan");
    const live = document.querySelector("#conversationTimeline .timeline-span.live");
    live.dataset.probe ??= String(Math.random());
    return { transform: pan.getAttribute("transform"), probe: live.dataset.probe, delay: live.style.animationDelay };
  });
  const first = await sample();
  await page.waitForTimeout(1_500);
  const second = await sample();
  const shift = value => Number(/translate\((-?[\d.]+)/.exec(value ?? "")?.[1] ?? 0);
  assert.ok(shift(second.transform) < shift(first.transform), `the view slides: ${first.transform} → ${second.transform}`);
  assert.equal(second.probe, first.probe, "sliding does not recreate live elements");
  assert.match(first.delay, /^-\d+ms$/, "live elements join the global pulse phase");
  // About 1.5 s of a 10-minute window: a sub-pixel-per-frame slide, not a jump.
  const width = await panel.locator(".timeline-scroll").evaluate(element => element.clientWidth);
  const expected = 1_500 * (width - 22) / (10 * minute);
  assert.ok(Math.abs(shift(first.transform) - shift(second.transform) - expected) < expected, `slide ${shift(first.transform) - shift(second.transform)} vs ${expected}`);

  await page.waitForTimeout(5_000);
  assert.equal(reads.length, settled, "an unchanged running conversation is not polled every few seconds");

  // New records (seen by the activity poll) trigger an incremental read.
  row.itemCount = 14;
  row.activity = { ...row.activity, itemCount: 14 };
  await page.waitForFunction(() => document.querySelectorAll("#conversationTimeline .timeline-span.tool").length === 2);
  assert.ok(reads.slice(settled).includes(`x1.${threadId}:1:10`), `increment read with the cursor: ${JSON.stringify(reads)}`);
  assert.ok(reads.slice(1).every(cursor => cursor), "later reads are increments");
  const tools = await panel.locator(".timeline-span.tool").evaluateAll(elements => elements.map(element => element.classList.contains("live")));
  assert.deepEqual(tools, [false, true], "the first tool closed and the new one runs");
  const phases = await page.evaluate(() => [...document.querySelectorAll("#conversationTimeline .live")].map(element => element.style.animationDelay));
  assert.ok(phases.length && phases.every(delay => /^-\d+ms$/.test(delay)), `redrawn live elements keep the phase: ${phases}`);
  assert.deepEqual(errors, []);
  console.log("PASS: recent timeline slides per frame without redraws, keeps the live pulse phase and reads increments on new activity");
} finally {
  await browser.close();
}

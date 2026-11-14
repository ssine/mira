import assert from "node:assert/strict";
const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const main = "30000000-0000-4000-8000-000000000001";
const other = "30000000-0000-4000-8000-000000000002";
const child = "30000000-0000-4000-8000-000000000003";
const turns = 30;
const start = Date.parse("2026-11-14T08:00:00Z");
const row = (threadId, title) => ({ threadId, title, cwd: "/work", generation: 1, itemCount: threadId === main ? turns * 10 : 4,
  updatedAt: new Date().toISOString(), activity: { state: "idle", generation: 1, itemCount: 1 } });
const rows = [row(main, "Index browser"), row(other, "Other conversation")];
const prose = index => Array.from({ length: 6 }, (_, line) => `Answer ${index + 1}, paragraph ${line + 1}. ${"Detail ".repeat(24)}`).join("\n\n");
const userSeq = index => index * 10 + 1;
const mainTrace = Array.from({ length: turns }, (_, index) => [
  { key: `history-${userSeq(index)}-user`, kind: "user", title: "你", markdown: true, body: `Question ${index + 1}: ${index === 6 ? "find the needle here" : "ordinary question"}`,
    sourceItemSeq: userSeq(index), turnId: `turn-${index}` },
  { key: `history-tool-turn-${index}-c${index}`, kind: "tool", title: "exec_command", markdown: false, body: "", status: "完成", sourceItemSeq: userSeq(index) + 2,
    turnId: `turn-${index}`, itemId: undefined, activity: { status: "completed", actions: [{ kind: "run", label: "ls" }] },
    toolFragment: { materialized: false, hasInput: true, hasOutput: true },
    toolDetail: { pages: [{ cursor: `t2:1:${userSeq(index) + 4}:300`, from: userSeq(index) + 2, limit: 200, loaded: false }] } },
  { key: `history-${userSeq(index) + 5}-agent`, kind: "assistant", title: "Codex", markdown: true, body: prose(index), sourceItemSeq: userSeq(index) + 5, turnId: `turn-${index}` },
]).flat();
const otherTrace = [{ key: "history-1-user", kind: "user", title: "你", markdown: true, body: "Only one question", sourceItemSeq: 1, turnId: "o1" },
  { key: "history-2-agent", kind: "assistant", title: "Codex", markdown: true, body: "Only one answer", sourceItemSeq: 2, turnId: "o1" }];
const minute = 60_000;
const timeline = { engine: "codex", rootThreadId: main, partial: false, truncated: false, lanes: [
  { threadId: main, parentThreadId: null, title: "Index browser", depth: 0,
    turns: [{ id: "turn-0", start, end: start + 5 * minute, status: "completed" }, { id: "turn-1", start: start + 120 * minute, end: start + 130 * minute, status: "completed" }],
    tools: [{ id: "c0", name: "exec_command", start: start + minute, end: start + 2 * minute, kind: "tool", seq: 3, turnId: "turn-0" },
      { id: "w1", name: "wait_agent", start: start + 121 * minute, end: start + 125 * minute, kind: "wait", seq: 14, turnId: "turn-1" }],
    messages: [{ at: start, seq: userSeq(0), role: "user", preview: "Question 1" }, { at: start + 120 * minute, seq: userSeq(4), role: "user", preview: "Question 5" },
      { at: start + 130 * minute, seq: userSeq(4) + 5, role: "assistant", preview: "Answer 5" }] },
  { threadId: child, parentThreadId: main, title: "Subagent", depth: 1,
    turns: [{ id: "child-turn", start: start + 121 * minute, end: start + 124 * minute, status: "completed" }],
    tools: [{ id: "cc", name: "exec_command", start: start + 122 * minute, end: start + 123 * minute, kind: "tool", seq: 2 }], messages: [] },
] };
const requests = { outline: [], timeline: 0, search: [], toolRanges: [] };

const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data: rows } }));
  await context.route(/\/v1\/codex\/threads\/[^/?]+(\/[a-z]+)?\?/, route => {
    const url = new URL(route.request().url()), path = url.pathname;
    const threadId = path.split("/")[4];
    const thread = rows.find(item => item.threadId === threadId) ?? row(threadId, "Subagent");
    if (path.endsWith("/costs")) return route.fulfill({ json: { threadId, generation: 1, itemCount: thread.itemCount, turnCostEstimates: {} } });
    if (path.endsWith("/timeline")) { requests.timeline++; return route.fulfill({ json: timeline }); }
    if (path.endsWith("/transcript")) {
      if (url.searchParams.get("from")) {
        requests.toolRanges.push(url.searchParams.get("from"));
        const key = mainTrace.find(item => item.kind === "tool" && String(item.sourceItemSeq) === url.searchParams.get("from")).key;
        return route.fulfill({ json: { generation: 1, itemCount: 300, pageCursor: url.searchParams.get("cursor"), trace: [{ ...mainTrace.find(item => item.key === key),
          body: "输入\nls\n\n输出\nREADME.md", toolFragment: { materialized: false, input: "ls", output: "README.md" },
          toolDetail: { pages: [{ cursor: url.searchParams.get("cursor"), limit: 200, loaded: true }] } }] } });
      }
      assert.equal(url.searchParams.get("outline"), "1", "conversations open from the whole outline");
      requests.outline.push(Object.fromEntries(url.searchParams));
      const trace = threadId === main ? mainTrace : otherTrace;
      const after = url.searchParams.get("after");
      return route.fulfill({ json: { generation: 1, itemCount: thread.itemCount, storeVersion: 7, outline: true, nextCursor: null,
        trace: after === null ? trace : [], ...(after === null ? {} : { after: Number(after) }) } });
    }
    return route.fulfill({ json: thread });
  });
  await context.route("**/v1/conversations/search?*", route => {
    const query = new URL(route.request().url()).searchParams.get("q");
    requests.search.push(query);
    route.fulfill({ json: { query, terms: query.split(/\s+/), truncated: false, results: query === "needle" ? [
      { engine: "codex", threadId: main, parentThreadId: null, rootThreadId: main, rootTitle: "Index browser", title: "Index browser", archived: false,
        titleMatch: false, hitCount: 1, latestAt: new Date().toISOString(), hits: [{ itemSeq: userSeq(6), role: "user", snippet: "Question 7: find the needle here" }] },
      { engine: "codex", threadId: child, parentThreadId: main, rootThreadId: main, rootTitle: "Index browser", title: "Subagent", archived: true,
        titleMatch: false, hitCount: 2, latestAt: null, hits: [{ itemSeq: 2, role: "assistant", snippet: "needle in a subagent" }] },
    ] : [] } });
  });
  const page = await context.newPage(), errors = [];
  page.setDefaultTimeout(15_000);
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?thread=${main}`);
  await page.waitForFunction(() => document.querySelector("#conversationTrace")?.textContent.includes("Answer 30, paragraph 6"));

  // The whole conversation is local: no older-page loader, every message present.
  assert.equal(await page.locator("#conversationTrace .history-loader").count(), 0, "a complete outline never pages older history");
  assert.equal(await page.locator('#conversationTrace .trace-card.user').count(), turns, "every user message is in the transcript");
  assert.equal(requests.outline.length, 1);
  assert.equal(requests.outline[0].after, undefined, "the first read is the whole outline");

  // Message rail: one tick per user message, the latest active at the bottom.
  const rail = page.locator("#conversationRail");
  await rail.waitFor({ state: "visible" });
  const ticks = rail.locator(".message-rail-tick");
  assert.equal(await ticks.count(), turns);
  await page.waitForFunction(() => document.querySelector('#conversationRail [aria-current="true"]')?.dataset.railIndex === "29");
  await ticks.nth(2).scrollIntoViewIfNeeded();
  await ticks.nth(2).hover();
  const tip = rail.locator(".message-rail-tip");
  await tip.waitFor({ state: "visible" });
  assert.match(await tip.textContent(), /^Question 3: ordinary question/);
  const tipBox = await tip.boundingBox(), railBox = await rail.boundingBox();
  assert.ok(tipBox.x + tipBox.width <= railBox.x + 4, "the preview opens beside the rail, over the reading surface");
  await ticks.nth(2).click();
  // A revealed message starts just below whatever floats over the reading surface.
  const revealed = async seq => page.waitForFunction(seq => {
    const card = document.querySelector(`[data-trace-key="history-${seq}-user"]`);
    const scroller = document.querySelector("#conversationScroll").getBoundingClientRect();
    const overlay = Math.max(scroller.top, document.querySelector(".conversation-notices").getBoundingClientRect().bottom, document.querySelector("#conversationTimeline").hidden ? 0 : document.querySelector("#conversationTimeline").getBoundingClientRect().bottom);
    if (!card) return false;
    const offset = card.getBoundingClientRect().top - overlay;
    return offset >= 0 && offset < 40;
  }, seq);
  await revealed(userSeq(2));
  assert.ok(await page.locator(`[data-trace-key="history-${userSeq(2)}-user"]`).evaluate(card => card.classList.contains("trace-card-flash")));
  await page.waitForFunction(() => document.querySelector('#conversationRail [aria-current="true"]')?.dataset.railIndex === "2");
  // Keyboard: arrows move between ticks, Enter jumps.
  await ticks.nth(2).focus();
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await revealed(userSeq(3));

  // Tool details still load lazily, from the outline's own record range.
  const toolGroup = page.locator(`[data-trace-key="history-${userSeq(3)}-user"] ~ .tool-group`).first();
  await toolGroup.locator("summary").first().click();
  const toolCard = toolGroup.locator(".trace-card").first();
  await toolCard.locator(".trace-detail > summary").click();
  await page.waitForFunction(() => document.querySelector(".tool-group[open] .trace-card .trace-body")?.textContent.includes("README.md"));
  assert.deepEqual(requests.toolRanges, [String(userSeq(3) + 2)]);

  // Timeline: collapsed by default and not read until expanded.
  const toggle = page.locator("#conversationTimelineToggle");
  const panel = page.locator("#conversationTimeline");
  await toggle.waitFor({ state: "visible" });
  assert.equal(await panel.isVisible(), false, "the timeline starts collapsed");
  assert.equal(await toggle.getAttribute("aria-expanded"), "false");
  assert.equal(requests.timeline, 0, "a collapsed timeline makes no request");
  // Opening and closing the floating panel never moves the transcript.
  const readingTop = () => page.evaluate(() => [...document.querySelectorAll("#conversationTrace .trace-card")]
    .map(card => card.getBoundingClientRect().top).find(top => top > 0));
  const before = await readingTop();
  await toggle.click();
  await panel.locator(".timeline-chart").waitFor();
  await page.waitForTimeout(100);
  assert.equal(await readingTop(), before, "opening the timeline keeps the transcript in place");
  assert.equal(await toggle.getAttribute("aria-expanded"), "true");
  assert.equal(requests.timeline, 1);
  assert.match(await panel.locator("[data-timeline-summary]").textContent(), /^2 段 · 活跃 15 分 · 工具 3 次 · 子 Agent 1$/);
  assert.equal(await panel.locator(".timeline-lane-label").count(), 2, "one lane per agent");
  assert.equal(await panel.locator("button.timeline-lane-label").getAttribute("data-timeline-open"), child, "subagent lanes open their conversation");
  assert.equal(await panel.locator(".timeline-break").count(), 1, "the two-hour idle gap becomes one break");
  assert.match(await panel.locator(".timeline-break title").textContent(), /空闲 1 小时 55 分/);
  // Broken axis: both segments together occupy the available width, not 135 minutes of it.
  const geometry = await panel.evaluate(element => {
    const band = element.querySelector(".timeline-break-band").getBBox();
    const svg = element.querySelector(".timeline-chart");
    return { band: band.width, width: Number(svg.getAttribute("width")), viewport: element.querySelector(".timeline-scroll").clientWidth };
  });
  assert.ok(geometry.band < 64 && geometry.width <= geometry.viewport + 1, `fits without a long idle stretch: ${JSON.stringify(geometry)}`);
  assert.ok(await panel.locator(".timeline-span.model").count() >= 2, "model phases fill turn time outside tools");
  assert.equal(await panel.locator(".timeline-span.wait").count(), 1);
  await panel.locator(`.timeline-message.user[data-timeline-seq="${userSeq(4)}"]`).click();
  await revealed(userSeq(4));
  // The fixed last-hour view shows real time without folded idle gaps, and is remembered.
  await panel.locator('[data-timeline-mode="hour"]').click();
  assert.equal(await panel.locator('[data-timeline-mode="hour"]').getAttribute("aria-pressed"), "true");
  assert.equal(await panel.locator(".timeline-break").count(), 0, "no breaks in the hour view");
  assert.match(await panel.locator("[data-timeline-summary]").textContent(), /^最近 1 小时 · /);
  assert.equal(await page.evaluate(() => localStorage.getItem("mira.timeline.mode")), "hour");
  assert.equal(await panel.locator('.timeline-message.user[data-timeline-seq="1"]').count(), 0, "messages outside the hour are hidden");
  assert.equal(await panel.locator(`.timeline-message.user[data-timeline-seq="${userSeq(4)}"]`).count(), 1);
  await panel.locator('[data-timeline-mode="all"]').click();
  assert.equal(await panel.locator(".timeline-break").count(), 1);
  await panel.locator("[data-timeline-zoom=in]").click();
  assert.ok(Number(await panel.locator(".timeline-chart").getAttribute("width")) > geometry.width, "zooming widens the chart");
  const beforeClose = await readingTop();
  await panel.locator("[data-timeline-close]").focus();
  await page.keyboard.press("Escape");
  assert.equal(await panel.isVisible(), false);
  await page.waitForTimeout(100);
  assert.equal(await readingTop(), beforeClose, "closing the timeline keeps the transcript in place");
  assert.equal(await toggle.getAttribute("aria-expanded"), "false");

  // Search replaces the list and jumps to the matching message.
  const search = page.locator("#agentThreadSearch");
  await search.fill("needle");
  const results = page.locator("#agentSearchResults");
  await results.locator(".search-result").first().waitFor();
  assert.equal(await page.locator("#agentThreadList").isVisible(), false, "results replace the conversation list");
  assert.equal(await results.locator(".search-result").count(), 2);
  assert.equal(await results.locator("mark").first().textContent(), "needle");
  assert.match(await results.locator(".search-result-meta").nth(1).textContent(), /子 Agent · Index browser · 已归档 · 2 处/);
  // Open another conversation first; the hit must reopen the original one.
  await search.press("Escape");
  assert.equal(await search.inputValue(), "");
  await page.locator("#agentThreadList").waitFor({ state: "visible" });
  await page.locator(`#agentThreadList button[data-thread-id="${other}"]`).click();
  await page.waitForFunction(() => document.querySelector("#conversationTrace")?.textContent.includes("Only one answer"));
  assert.equal(await rail.isVisible(), false, "a single message needs no rail");
  assert.equal(await panel.isVisible(), false, "each conversation opens with its timeline collapsed");
  await search.fill("needle");
  await results.locator(".search-result-hit").first().click();
  await page.waitForFunction(id => new URL(location.href).searchParams.get("thread") === id, main);
  await revealed(userSeq(6));
  assert.ok(requests.search.at(-1) === "needle");
  // Reopening an outline only reads records after the cached count.
  assert.ok(requests.outline.slice(1).some(query => query.after === String(turns * 10) && query.generation === "1"),
    `incremental reads: ${JSON.stringify(requests.outline)}`);

  // Narrow screens keep the reading width: no rail.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.waitForTimeout(100);
  assert.equal(await rail.isVisible(), false, "the rail is hidden on phones");
  await page.locator("#conversationTimelineToggle").click();
  await panel.locator(".timeline-chart").waitFor();
  assert.ok((await panel.boundingBox()).width <= 390, "the timeline fits a phone screen");
  assert.deepEqual(errors, []);
  console.log("PASS: whole-outline transcript, message rail preview/jump/keyboard, lazy tool ranges, collapsed timeline with folded idle gaps, search jump across conversations and narrow layout");
} finally {
  await browser.close();
}

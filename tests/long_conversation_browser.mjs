// A conversation with thousands of records: reconciliation must be incremental
// and a message inside the reading area must never be left empty.
import assert from "node:assert/strict";
const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const threadId = "31000000-0000-4000-8000-000000000001";
const initialTurns = 260, liveTurn = `turn-${initialTurns}`;
let head = 0;
const trace = [];
const seq = () => ++head;
// Headings, lists, tables and wide characters render much taller or shorter
// than their source suggests, so reserved and real heights differ a lot.
const prose = (turn, part) => Array.from({ length: 1 + (turn * 7 + part * 5) % 8 }, (_, line) => [
  `回复 ${turn}.${part} 第 ${line + 1} 段。${"这是一段完全由中文组成的正文，用来撑开实际高度。".repeat(2 + (turn + line) % 7)}`,
  `| 项目 | 建议值 |\n|---|---|\n${Array.from({ length: 6 }, (_, index) => `| ${turn}.${index} | ${"较长的单元格".repeat(6)} |`).join("\n")}`,
  `### 小标题 ${line}\n\n${Array.from({ length: 7 }, (_, index) => `- 列表项 ${index}\n\n  展开说明`).join("\n")}`,
  "```sh\n" + `echo ${turn}\n`.repeat(9) + "```",
][(turn + line) % 4]).join("\n\n") + `\n\n回复 ${turn}.${part} 完`;
const user = turn => ({ key: `history-${seq()}-user`, kind: "user", title: "你", body: `问题 ${turn}`, sourceItemSeq: head, turnId: `turn-${turn}` });
const tool = (turn, end = null) => { const at = seq(); return { key: `history-tool-turn-${turn}-c${at}`, kind: "tool", title: "exec_command", body: "", status: "完成",
  sourceItemSeq: at, turnId: `turn-${turn}`, activity: { status: "completed", actions: [{ kind: "run", label: `ls ${at}` }] },
  toolFragment: { materialized: false, hasInput: true, hasOutput: true }, end: end ?? at + 1 }; };
const reply = (turn, part) => ({ key: `history-${seq()}-agent`, kind: "assistant", title: "Codex", body: prose(turn, part), sourceItemSeq: head, turnId: `turn-${turn}`,
  completedAt: new Date(Date.parse("2026-11-20T00:00:00Z") + head * 1000).toISOString(), timingScope: "recorded" });
const round = (turn, part) => { trace.push(tool(turn), tool(turn), reply(turn, part)); };
for (let turn = 0; turn < initialTurns; turn++) { trace.push(user(turn)); for (let part = 0; part < 1 + turn % 8; part++) round(turn, part); }
trace.push(user(initialTurns)); round(initialTurns, 0);

// Like the Server: an outline range's cursor names the snapshot it was read from.
const served = item => item.kind !== "tool" ? { ...item } : (({ end, ...rest }) => ({ ...rest,
  toolDetail: { pages: [{ cursor: `t2:1:${end}:${head}`, from: item.sourceItemSeq, limit: 200, loaded: false }] } }))(item);
const row = () => ({ threadId, title: "Long conversation", cwd: "/work", generation: 1, itemCount: head, updatedAt: new Date().toISOString(),
  activity: { state: "running", generation: 1, itemCount: head, turnId: liveTurn } });
const requests = { after: [], details: [] };

const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data: [row()] } }));
  await context.route(/\/v1\/codex\/threads\/[^/?]+(\/[a-z]+)?\?/, route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith("/costs")) return route.fulfill({ json: { threadId, generation: 1, itemCount: head, turnCostEstimates: {} } });
    if (!url.pathname.endsWith("/transcript")) return route.fulfill({ json: row() });
    if (url.searchParams.get("toolDetails") === "1") {
      requests.details.push(url.searchParams.get("from"));
      const item = trace.find(item => String(item.sourceItemSeq) === url.searchParams.get("from"));
      return route.fulfill({ json: { generation: 1, itemCount: head, trace: [{ ...served(item), body: "输入\nls\n\n输出\nREADME.md",
        toolFragment: { materialized: false, input: "ls", output: "README.md" },
        toolDetail: { pages: [{ cursor: url.searchParams.get("cursor"), limit: 200, loaded: true }] } }] } });
    }
    const after = url.searchParams.get("after");
    if (after !== null) requests.after.push(Number(after));
    const items = after === null ? trace : trace.filter(item => item.turnId === liveTurn || item.sourceItemSeq > Number(after));
    return route.fulfill({ json: { generation: 1, itemCount: head, storeVersion: 1000 + head, outline: true, nextCursor: null,
      trace: items.map(served), ...(after === null ? {} : { after: Number(after) }) } });
  });
  const page = await context.newPage(), errors = [];
  page.setDefaultTimeout(20_000);
  page.on("pageerror", error => errors.push(error.message));
  // Asynchronous notifications and layout changes interleave on a slower device.
  await (await context.newCDPSession(page)).send("Emulation.setCPUThrottlingRate", { rate: 4 });
  // Prose inside the reading area whose text is not on screen.
  await page.route("**/app.js*", async route => { const response = await route.fetch();
    await route.fulfill({ response, body: (await response.text()).replace("void bootstrap();", "window.__notify = handleAgentNotification; void bootstrap();") }); });
  await page.addInitScript(() => { window.__empty = () => {
    const view = document.querySelector("#conversationScroll").getBoundingClientRect();
    return [...document.querySelectorAll("#conversationTrace .trace-card")].filter(card => {
      const body = card.querySelector(":scope > .trace-body"), box = card.getBoundingClientRect();
      return body?._miraSource && box.bottom > view.top && box.top < view.bottom && !body.hasChildNodes();
    }).map(card => card.dataset.traceKey);
  }; });
  const empty = () => page.evaluate(() => window.__empty());
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?thread=${threadId}`);
  await page.waitForFunction(turn => document.querySelector("#conversationTrace")?.textContent.includes(`回复 ${turn}.0 完`), initialTurns);

  const frames = () => page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.locator("#conversationTrace .trace-card.user").count(), initialTurns + 1, "the whole conversation is present");
  assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll("#conversationTrace .trace-card.user")].map(card => card.dataset.traceKey)),
    trace.filter(item => item.kind === "user").map(item => item.key), "user messages keep conversation order across sections");
  assert.ok(await page.evaluate(() => document.querySelector("#conversationTrace").childElementCount) < 300,
    "older rows are bundled, so a change does not lay out every row of the conversation");
  assert.ok(await page.evaluate(() => document.querySelectorAll("#conversationTrace .trace-body > *").length) < 400, "prose far from the viewport is not rendered");
  assert.deepEqual(await empty(), []);

  // Background reads keep every unchanged row: no rebuild, no empty message.
  await page.evaluate(() => {
    window.__before = [...document.querySelectorAll("#conversationTrace .trace-card, #conversationTrace .tool-group")];
    window.__removed = 0; window.__added = 0;
    // What each frame left on screen, sampled right after it painted.
    window.__emptyFrames = [];
    const painted = new MessageChannel();
    painted.port1.onmessage = () => { const keys = window.__empty(); if (keys.length) window.__emptyFrames.push(keys); };
    const sample = () => { painted.port2.postMessage(0); requestAnimationFrame(sample); };
    requestAnimationFrame(sample);
    new MutationObserver(records => { for (const record of records) {
      for (const node of record.removedNodes) if (node.matches?.(".trace-card, .tool-group, .trace-section")) window.__removed++;
      for (const node of record.addedNodes) if (node.matches?.(".trace-card, .tool-group, .trace-section")) window.__added++;
    } }).observe(document.querySelector("#conversationTrace"), { childList: true, subtree: true });
  });
  for (let update = 1; update <= 5; update++) {
    round(initialTurns, update); round(initialTurns, update + 100);
    await page.waitForFunction(text => document.querySelector("#conversationTrace").textContent.includes(text), `回复 ${initialTurns}.${update + 100} 完`);
    await page.waitForTimeout(500);
    assert.deepEqual(await page.evaluate(() => window.__emptyFrames.splice(0)), [], `update ${update} paints no empty message`);
    assert.deepEqual(await empty(), [], `update ${update} leaves no message empty`);
    assert.ok(await page.evaluate(() => { const s = document.querySelector("#conversationScroll"); return s.scrollHeight - s.clientHeight - s.scrollTop < 3; }), "the reader keeps following the latest reply");
  }
  const churn = await page.evaluate(() => ({ kept: window.__before.filter(node => node.isConnected).length, total: window.__before.length, removed: window.__removed, added: window.__added }));
  assert.equal(churn.kept, churn.total, "existing rows survive background reads");
  assert.equal(churn.removed, 0, "background reads do not detach existing rows");
  assert.ok(churn.added <= 40, `only new rows are added: ${churn.added}`);

  // The same tool range, re-read by every poll, stays one page to load.
  const group = page.locator(`[data-trace-key="${trace.filter(item => item.kind === "user").at(-1).key}"] ~ .tool-group`).first();
  await group.locator("summary").first().click();
  await group.locator(".trace-card").first().locator(".trace-detail > summary").click();
  await page.waitForFunction(() => document.querySelector(".tool-group[open] .trace-card .trace-body")?.textContent.includes("README.md"));
  assert.equal(requests.details.length, 1, "an expanded tool loads its range once, however often it was re-read");
  round(initialTurns, 300);
  await page.waitForFunction(() => document.querySelector("#conversationTrace").querySelector(".tool-group[open] .trace-card .trace-body")?.textContent.includes("README.md"));
  await group.locator("summary").first().click();

  // A streamed reply keeps its card when its stored record arrives under
  // another key, and is typeset once complete.
  await page.evaluate(() => { const s = document.querySelector("#conversationScroll"); s.scrollTop = s.scrollHeight; });
  await frames();
  const streamed = `回复 stream 完\n\n- 第一项\n- 第二项`;
  await page.evaluate(async ({ threadId, turnId, text }) => {
    const notify = (method, params) => window.__notify({ method, params: { threadId, turnId, ...params } });
    for (const part of text.match(/[\s\S]{1,4}/g)) notify("item/agentMessage/delta", { itemId: "live-reply", delta: part });
    await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    notify("item/completed", { item: { id: "live-reply", type: "agentMessage", text } });
    window.__live = [...document.querySelectorAll("#conversationTrace .trace-card.assistant")].at(-1);
  }, { threadId, turnId: liveTurn, text: streamed });
  await page.waitForFunction(() => window.__live.querySelectorAll("li").length === 2);
  trace.push({ ...reply(initialTurns, 0), body: streamed });
  await page.waitForFunction(key => window.__live.dataset.traceKey === key, trace.at(-1).key);
  assert.equal(await page.evaluate(() => [...document.querySelectorAll("#conversationTrace .trace-card.assistant")].filter(card => card.querySelector(".trace-body")._miraSource?.startsWith("回复 stream")).length), 1);
  assert.ok(await page.evaluate(() => window.__live.isConnected && window.__live.querySelectorAll("li").length === 2));

  // Jumps, steady scrolling and resizing: prose is there as soon as a frame paints.
  for (const fraction of [0.1, 0.93, 0.5, 0.31, 0.02, 0.7, 0.5001, 1]) {
    await page.evaluate(fraction => { const s = document.querySelector("#conversationScroll"); s.scrollTop = s.scrollHeight * fraction; }, fraction);
    await frames();
    assert.deepEqual(await empty(), [], `jump to ${fraction}`);
  }
  const box = await page.locator("#conversationScroll").boundingBox();
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  for (let step = 0; step < 25; step++) {
    await page.mouse.wheel(0, -900);
    await frames();
    assert.deepEqual(await empty(), [], `wheel step ${step}`);
  }
  // An older reading position is not moved by output arriving at the tail.
  const reading = () => page.evaluate(() => { const top = document.querySelector("#conversationScroll").getBoundingClientRect().top;
    const card = [...document.querySelectorAll("#conversationTrace .trace-card")].find(card => card.getBoundingClientRect().bottom > top); return [card.dataset.traceKey, Math.round(card.getBoundingClientRect().top)]; });
  await page.waitForTimeout(300);
  const before = await reading();
  round(initialTurns, 400);
  const reads = requests.after.length;
  await page.waitForFunction(key => document.querySelector(`[data-trace-key="${key}"]`), trace.at(-1).key);
  assert.ok(requests.after.length > reads);
  assert.deepEqual(await reading(), before, "reading position");
  for (const height of [600, 1000, 900]) {
    await page.setViewportSize({ width: 1440, height });
    await page.waitForTimeout(100);
    assert.deepEqual(await empty(), [], `viewport height ${height}`);
  }
  await page.setViewportSize({ width: 412, height: 860 });
  await page.waitForTimeout(200);
  assert.deepEqual(await empty(), [], "phone layout");
  await page.setViewportSize({ width: 1440, height: 900 });

  // A message from the rail, far above in a section.
  await page.evaluate(() => document.querySelector('#conversationRail [data-rail-index="40"]').click());
  await page.waitForFunction(key => { const card = document.querySelector(`[data-trace-key="${key}"]`), view = document.querySelector("#conversationScroll").getBoundingClientRect();
    const top = card.getBoundingClientRect().top - view.top; return top > 0 && top < 160; }, trace.filter(item => item.kind === "user")[40].key);
  await frames();
  assert.deepEqual(await empty(), [], "after a rail jump");

  // Reopening paints the browser copy; nothing in it is empty either.
  await page.evaluate(() => { const s = document.querySelector("#conversationScroll"); s.scrollTop = s.scrollHeight; });
  await page.reload();
  await page.waitForFunction(turn => document.querySelector("#conversationTrace")?.textContent.includes(`回复 ${turn}.400 完`), initialTurns);
  await frames();
  assert.deepEqual(await empty(), [], "reopened conversation");
  assert.deepEqual(errors, []);
  console.log(`long conversation: ${trace.length} items, ${churn.total} rows kept across updates, no empty message`);
} finally {
  await browser.close();
}

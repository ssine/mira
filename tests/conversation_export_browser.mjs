// Exports Codex and Claude conversations from the thread menu and checks the
// downloaded single-file HTML in both brief and detailed modes.
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { startDraftFixture } from "./composer_drafts_fixture.mjs";
const { chromium } = await import(process.argv[2] ?? "playwright");
const fixture = await startDraftFixture();
const codex = "00000000-0000-4000-8000-0000000000a1", other = "00000000-0000-4000-8000-0000000000b2";
const claude = "00000000-0000-4000-8000-0000000000c3", nodeId = "00000000-0000-4000-8000-000000000001";
const png = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==";
const tool = (seq, loaded) => ({ key: `history-tool-${seq}`, kind: "tool", title: "Shell", status: "完成", sourceItemSeq: seq, turnId: "t1",
  activity: { status: "completed", durationMs: 1500, actions: [{ kind: "run", label: "cat notes.txt" }] },
  body: loaded ? "cat notes.txt\n\nTOOL-OUTPUT-MARKER" : "",
  toolFragment: loaded ? { materialized: true } : { materialized: true, hasInput: true, hasOutput: true } });
// Two tail pages: the newest page first, then the older one.
const pages = loaded => ({
  "": { generation: 1, itemCount: 9, trace: [
    { key: "history-6-image-0", kind: "image", title: "图片", body: "", sourceItemSeq: 6, imageIndex: 0, turnId: "t2",
      image: { href: `/v1/codex/threads/${codex}/transcript/image?storeId=personal&generation=1&itemSeq=6&index=0` } },
    { key: "history-7-agent", kind: "assistant", body: "第二轮 **回复**，见 [本地文件](/work/notes.txt)。\n\n```mermaid\ngraph TD\n  Export --> Done\n```", sourceItemSeq: 7, turnId: "t2", turnCompletedAt: "2026-11-16T08:00:00Z" },
  ], nextCursor: "t2:1:5:9" },
  "t2:1:5:9": { generation: 1, itemCount: 9, trace: [
    { key: "history-1-user", kind: "user", body: "第一个问题 <script>alert(1)</script>", sourceItemSeq: 1, turnId: "t1" },
    { key: "history-2-reasoning", kind: "reasoning", title: "推理摘要", body: "REASONING-MARKER", sourceItemSeq: 2, turnId: "t1" },
    tool(3, loaded),
    { key: "history-3-image-0", kind: "image", title: "图片", body: "", sourceItemSeq: 3, imageIndex: 1, turnId: "t1",
      image: { href: `/v1/codex/threads/${codex}/transcript/image?storeId=personal&generation=1&itemSeq=3&index=0` } },
    { key: "history-4-agent", kind: "assistant", body: "第一轮回复", sourceItemSeq: 4, turnId: "t1" },
    { key: "history-5-user", kind: "user", body: "第二个问题", sourceItemSeq: 5, turnId: "t2" },
  ], nextCursor: null },
});
const requests = { transcript: [], images: [] };
const claudeSummary = { threadId: claude, sessionId: claude, engine: "claude", nodeId, runtimeNodeId: nodeId, title: "Claude 导出", cwd: "/work",
  generation: 1, listRoot: true, childCount: 0, itemCount: 4, updatedAt: new Date().toISOString(), activity: { state: "idle" } };
const claudeEvents = [
  { seq: 1, turnId: "c1", payload: { type: "mira_user", uuid: "u1", text: "Claude 问题" } },
  { seq: 2, turnId: "c1", payload: { type: "assistant", uuid: "a1", message: { id: "m1", content: [
    { type: "tool_use", id: "tool-1", name: "Bash", input: { command: "ls -la", description: "列出文件" } }] } } },
  { seq: 3, turnId: "c1", payload: { type: "user", uuid: "r1", message: { content: [{ type: "tool_result", tool_use_id: "tool-1", content: "CLAUDE-TOOL-OUTPUT" }] } } },
  { seq: 4, turnId: "c1", payload: { type: "assistant", uuid: "a2", message: { id: "m2", content: [{ type: "text", text: "Claude 回复" }] } } },
];

async function exportFrom(page, threadId, includeTools) {
  await page.locator(`[data-thread-id="${threadId}"]`).click({ button: "right" });
  await page.locator("#threadExport").click();
  await page.locator("#threadExportDialog").waitFor({ state: "visible" });
  assert.equal(await page.locator("#threadExportTools").isChecked(), false, "tool details are off by default");
  if (includeTools) await page.locator("#threadExportTools").check();
  const [download] = await Promise.all([page.waitForEvent("download"), page.locator("#threadExportConfirm").click()]);
  await page.locator("#threadExportDialog").waitFor({ state: "hidden" });
  const html = await fs.readFile(await download.path(), "utf8");
  if (process.env.MIRA_EXPORT_SAVE_DIR) await fs.writeFile(`${process.env.MIRA_EXPORT_SAVE_DIR}/${threadId.slice(-2)}-${includeTools ? "detailed" : "brief"}.html`, html);
  return { name: download.suggestedFilename(), html };
}

let browser;
try {
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, acceptDownloads: true });
  await context.route(`**/v1/codex/threads/${codex}/transcript?*`, route => {
    const url = new URL(route.request().url());
    if (url.searchParams.get("outline") === "1" || url.searchParams.get("tail") !== "1") return route.fallback();
    requests.transcript.push(Object.fromEntries(url.searchParams));
    route.fulfill({ json: pages(url.searchParams.get("toolDetails") === "1")[url.searchParams.get("cursor") ?? ""] });
  });
  await context.route(`**/v1/codex/threads/${codex}/transcript/image?*`, route => {
    requests.images.push(new URL(route.request().url()).searchParams.get("itemSeq"));
    route.fulfill({ json: { url: png } });
  });
  await context.route("**/v1/claude/conversations?*", route => route.fulfill({ json: { data: [claudeSummary],
    projects: [{ key: JSON.stringify([nodeId, "/work"]), nodeId, cwd: "/work", count: 1 }], paged: true } }));
  await context.route(`**/v1/claude/conversations/${claude}`, route => route.fulfill({ json: claudeSummary }));
  await context.route(`**/v1/claude/sessions/${claude}/events?*`, route => route.fulfill({ json: {
    data: claudeEvents, cursor: 4, earliest: 1, more: false, session: { activeTurn: null } } }));

  const page = await context.newPage(), errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(`${fixture.origin}/?thread=${other}`);
  await page.locator(".trace-card.assistant").waitFor();

  // Brief export: messages and inline user-facing images; tools fold to one line.
  const brief = await exportFrom(page, codex, false);
  assert.match(brief.name, /^Draft a1 \d{4}-\d{2}-\d{2}\.html$/);
  assert.match(brief.html, /<title>Draft a1<\/title>/);
  assert.ok(brief.html.indexOf("第一个问题") < brief.html.indexOf("第一轮回复") && brief.html.indexOf("第一轮回复") < brief.html.indexOf("第二轮"),
    "pages are assembled in conversation order");
  assert.match(brief.html, /<strong>回复<\/strong>/, "Markdown is rendered");
  assert.doesNotMatch(brief.html, /<script|alert\(1\)/i, "message HTML is sanitized; the file contains no script");
  assert.match(brief.html, /<span class="file-ref" title="\/work\/notes.txt">本地文件<\/span>/, "local paths are not links");
  assert.match(brief.html, /工具调用 · 1 次 · 执行 × 1/);
  assert.doesNotMatch(brief.html, /TOOL-OUTPUT-MARKER|REASONING-MARKER|cat notes\.txt/);
  assert.equal(brief.html.match(/<img (?!class="diagram)/g)?.length, 1, "only the conversation image, not the tool screenshot");
  assert.ok(requests.transcript.every(query => query.toolDetails === "0"), "a brief export does not request tool bodies");
  assert.match(brief.html, /<img class="diagram-light" src="data:image\/svg\+xml;charset=utf-8,[^"]+"/, "Mermaid renders to an embedded SVG");
  assert.match(brief.html, /<img class="diagram-dark" src="data:image\/svg\+xml/);
  assert.match(brief.html, /<summary>查看图表源码<\/summary><pre><code class="language-mermaid">graph TD/);
  assert.deepEqual(requests.images, ["6"]);

  // Detailed export: tool input/output, thinking and tool images are expanded.
  requests.transcript.length = 0; requests.images.length = 0;
  const detailed = await exportFrom(page, codex, true);
  assert.ok(requests.transcript.every(query => query.toolDetails === "1"));
  assert.match(detailed.html, /<details class="group">/);
  assert.match(detailed.html, /已执行 cat notes\.txt/);
  assert.match(detailed.html, /TOOL-OUTPUT-MARKER/);
  assert.match(detailed.html, /REASONING-MARKER/);
  assert.equal(detailed.html.match(/<img (?!class="diagram)/g)?.length, 2);
  assert.deepEqual(requests.images.sort(), ["3", "6"]);

  // Claude conversations export from their native history.
  const claudeBrief = await exportFrom(page, claude, false);
  assert.match(claudeBrief.html, /Claude 问题/);
  assert.match(claudeBrief.html, /Claude 回复/);
  assert.match(claudeBrief.html, /工具调用 · 1 次/);
  assert.doesNotMatch(claudeBrief.html, /CLAUDE-TOOL-OUTPUT/);
  const claudeDetailed = await exportFrom(page, claude, true);
  assert.match(claudeDetailed.html, /列出文件/);
  assert.match(claudeDetailed.html, /CLAUDE-TOOL-OUTPUT/);

  // Exporting does not disturb the open conversation.
  assert.equal(new URL(page.url()).searchParams.get("thread"), other);
  assert.match(await page.locator("#conversationTrace").textContent(), /Persisted fixture history/);
  assert.deepEqual(errors, []);
  console.log("PASS: conversation export");
} finally {
  await browser?.close();
  await fixture.close();
}

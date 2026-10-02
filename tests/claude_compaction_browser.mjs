import assert from "node:assert/strict";
import { startQuestionFixture, questionThread } from "./claude_questions_fixture.mjs";
const { chromium } = await import(process.argv[2] ?? "playwright");
const fixture = await startQuestionFixture({ compaction: true });
const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  await page.goto(`${fixture.origin}/?thread=${questionThread}`);
  const card = page.locator("#conversationTrace .trace-card.compaction");
  await card.waitFor({ timeout: 30000 });
  assert.equal(await card.count(), 1);
  const order = await page.$$eval("#conversationTrace .trace-card", cards => cards.map(card => card.dataset.traceKind));
  assert.deepEqual(order, ["user", "compaction", "assistant"]);
  // The summary is collapsed behind the notice, like Codex, and expands on demand.
  assert.match(await card.locator(".compaction-label").innerText(), /167,625 → 5,554 tokens/);
  assert.equal(await card.locator(".trace-body").isVisible(), false);
  await card.locator(".compaction-head").click();
  assert.match(await card.locator(".trace-body").innerText(), /推送 main 并发布新版本/);
  // The summary is not also shown as a user message.
  assert.equal(await page.locator("#conversationTrace .trace-card.user", { hasText: "Summary" }).count(), 0);
  console.log("passed:", order.join(","));
} finally { await browser.close(); await fixture.close(); }

import assert from "node:assert/strict";
import { startQuestionFixture, questionThread } from "./claude_questions_fixture.mjs";
const { chromium } = await import(process.argv[2] ?? "playwright");
const fixture = await startQuestionFixture({ images: true });
const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  await page.goto(`${fixture.origin}/?thread=${questionThread}`);
  // Both the user's image and the tool's screenshot are visible image cards, not hidden card bodies.
  await page.waitForFunction(() => [...document.querySelectorAll("#conversationTrace .trace-card.image img")]
    .filter(img => !img.hidden && img.naturalWidth === 1 && img.getBoundingClientRect().height > 0).length === 2, null, { timeout: 30000 });
  const order = await page.$$eval("#conversationTrace .trace-card", cards => cards.map(card => card.dataset.traceKind));
  assert.deepEqual(order.filter(kind => kind !== "reasoning"), ["user", "image", "tool", "image", "assistant"]);
  console.log("passed:", order.join(","));
} finally { await browser.close(); await fixture.close(); }

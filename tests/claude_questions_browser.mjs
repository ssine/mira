import assert from "node:assert/strict";
import { startQuestionFixture, questionThread } from "./claude_questions_fixture.mjs";
const { chromium } = await import(process.argv[2] ?? "playwright");
const fixture = await startQuestionFixture();
const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage({ viewport: { width: 1200, height: 900 } });
  const errors = []; page.on("pageerror", error => errors.push(error.message));
  for (const phase of [1, 2, 3, 4]) {
    if (phase === 2) await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${fixture.origin}/?thread=${questionThread}`);
    await page.evaluate(phase => {
      if (phase === 2) document.documentElement.dataset.theme = "dark";
      const script = document.createElement("script"); script.type = "module";
      script.src = `/__test/scenarios.mjs?phase=${phase}`; document.head.append(script);
    }, phase);
    await page.waitForFunction(() => document.documentElement.dataset.questionTest, null, { timeout: 30000 });
    assert.equal(await page.evaluate(() => document.documentElement.dataset.questionTest), `phase ${phase} passed`);
  }
  assert.deepEqual(errors, []);
  console.log("PASS: visible native questions, single/multiple/free-text answers, validation, polling, retries, duplicate prevention, completion, reload and narrow layout");
} finally { await browser.close(); await fixture.close(); }

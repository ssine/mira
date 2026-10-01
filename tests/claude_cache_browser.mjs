import assert from "node:assert/strict";
import { startQuestionFixture, questionThread } from "./claude_questions_fixture.mjs";
const { chromium } = await import(process.argv[2] ?? "playwright");
const fixture = await startQuestionFixture({ cacheHistory: true });
const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await page.goto(`${fixture.origin}/?thread=${questionThread}&cacheTest=1`);
  await page.waitForFunction(() => document.documentElement.dataset.cacheTest, null, { timeout: 30000 });
  const result = await page.evaluate(() => document.documentElement.dataset.cacheTest);
  assert.match(result, /^passed:/);
  console.log(result);
} finally { await browser.close(); await fixture.close(); }

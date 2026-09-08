import assert from "node:assert/strict";
import { startTranscriptFixture, threadId } from "./transcript_images_fixture.mjs";
const { chromium } = await import(process.argv[2] ?? "playwright");
const fixture = await startTranscriptFixture();
let browser;
try {
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  const page = await context.newPage(), errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(`${fixture.origin}/?thread=${threadId}`);
  const result = await page.evaluate(async () => (await import("/__test/scenarios.mjs")).runTranscriptScenarios());
  const other = await context.newPage();
  other.on("pageerror", error => errors.push(error.message));
  await other.goto(`${fixture.origin}/?thread=${threadId}`);
  assert.deepEqual((await other.evaluate(async () => (await import("/__test/scenarios.mjs")).verifyImages())).keys, result.keys);
  await other.close();
  await page.bringToFront();
  await page.reload();
  await page.evaluate(async () => (await import("/__test/scenarios.mjs")).verifyImages());
  assert.deepEqual(errors, []);
  console.log("PASS: history-only images, stable order, reload/second window, detail updates and reading position", result);
} finally {
  await browser?.close();
  await fixture.close();
}

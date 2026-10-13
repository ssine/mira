import assert from "node:assert/strict";
import { startTranscriptFixture, threadId } from "./transcript_images_fixture.mjs";
const { chromium } = await import(process.argv[2] ?? "playwright");
const fixture = await startTranscriptFixture();
let browser;
try {
  browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(`${fixture.origin}/?thread=${threadId}`);
  await page.evaluate(async () => (await import("/__test/scroll-scenarios.mjs")).prepareScrollChecks());
  const scroll = page.locator("#conversationScroll"), box = await scroll.boundingBox();
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.wheel(0, -40);
  await page.waitForTimeout(160);
  const top = await scroll.evaluate(element => element.scrollTop);
  const gap = await scroll.evaluate(element => element.scrollHeight - element.clientHeight - element.scrollTop);
  assert.ok(gap > 0 && gap < 96, `one slow wheel step stays within the old follow threshold: ${gap}`);
  await page.evaluate(async () => (await import("/__test/scroll-scenarios.mjs")).queueStreamUpdate());
  await page.evaluate(async top => (await import("/__test/scroll-scenarios.mjs")).checkReadingPosition(top), top);
  for (let step = 0; step < 3; step++) {
    const before = await scroll.evaluate(element => element.scrollTop);
    await page.mouse.wheel(0, -40);
    await page.waitForTimeout(250);
    assert.ok(await scroll.evaluate(element => element.scrollTop) < before, "each slow step scrolls up");
  }
  await page.evaluate(async () => (await import("/__test/scroll-scenarios.mjs")).checkScrollResumes());
  await page.goto(`${fixture.origin}/?thread=${threadId}&scrollTest=1`);
  await page.waitForFunction(() => document.documentElement.dataset.scrollTest);
  const result = await page.evaluate(() => document.documentElement.dataset.scrollTest);
  assert.match(result, /^passed:/);
  assert.deepEqual(errors, []);
  console.log(result);
} finally {
  await browser?.close();
  await fixture.close();
}

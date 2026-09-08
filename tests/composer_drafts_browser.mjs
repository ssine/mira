// Real IndexedDB, real UI, disposable server; reopening reuses a dedicated disk profile.
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { startDraftFixture } from "./composer_drafts_fixture.mjs";
const { chromium } = await import(process.argv[2] ?? "playwright");
const fixture = await startDraftFixture();
const profile = await fs.mkdtemp(path.join(os.tmpdir(), "mira-drafts-"));
const errors = [];
let context;
try {
  let url = `${fixture.origin}/?thread=00000000-0000-4000-8000-0000000000a1`;
  for (const phase of [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]) {
    context = await chromium.launchPersistentContext(profile, { headless: true, viewport: { width: 1440, height: 1000 } });
    const page = await context.newPage();
    page.on("pageerror", error => errors.push(error.message));
    await page.goto(phase === 10 ? `${fixture.origin}/?view=nodes` : url);
    await page.evaluate(phase => {
      const script = document.createElement("script");
      script.type = "module"; script.src = `/__test/scenarios.mjs?phase=${phase}`;
      document.head.append(script);
    }, phase);
    await page.waitForFunction(() => document.documentElement.dataset.draftTest);
    const result = await page.evaluate(() => document.documentElement.dataset.draftTest);
    assert.equal(result, `phase ${phase} passed`);
    console.log(result);
    url = page.url();
    await context.close(); context = null;
  }
  assert.deepEqual(errors, []);
  console.log("composer draft persistence regression passed");
} finally {
  await context?.close();
  await fixture.close();
  await fs.rm(profile, { recursive: true, force: true });
}

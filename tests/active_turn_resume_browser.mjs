import assert from "node:assert/strict";
import { startDraftFixture } from "./composer_drafts_fixture.mjs";

const { chromium } = await import(process.argv[2] ?? "playwright");
const fixture = await startDraftFixture({ resumeTest: true, activityTest: true });
const threadId = "00000000-0000-4000-8000-0000000000a1";
let browser;
const control = async (path, value) => (await fetch(`${fixture.origin}/__test/${path}`, value === undefined ? {} : {
  method: "POST", body: JSON.stringify(value),
})).json();
async function waitFor(read) {
  const deadline = Date.now() + 15_000;
  while (!await read()) {
    assert(Date.now() < deadline, "fixture condition timed out");
    await new Promise(resolve => setTimeout(resolve, 50));
  }
}
try {
  browser = await chromium.launch({ headless: true });
  const page = await browser.newPage();
  page.setDefaultTimeout(15_000);
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  await page.addInitScript(() => {
    const timeout = window.setTimeout.bind(window);
    window.setTimeout = (fn, ms, ...args) => timeout(fn, ms === 25_000 ? 150 : ms, ...args);
  });
  await page.goto(`${fixture.origin}/?thread=${threadId}`);
  await page.locator("#conversationInput").fill("Retained draft");
  await waitFor(async () => (await control("resume")).requests === 1);
  await control("activity", { state: "running", turnId: "active-turn", itemCount: 2 });
  await control("resume", { active: true });
  await page.locator("#resumeProgress").waitFor({ state: "hidden" });
  await page.locator("#agentInterrupt").waitFor({ state: "visible" });
  await waitFor(async () => (await control("activity")).rpcMethods.filter(method => method === "thread/loaded/list").length >= 3);
  const active = await control("activity");
  assert.equal(active.rpcMethods.includes("thread/turns/list"), false, "warm resume and repeated heartbeats use lightweight activity, without history reconstruction");
  assert.equal((await control("resume")).requests, 1, "heartbeat retains the loaded session");
  assert.equal(await page.locator("#conversationInput").inputValue(), "Retained draft");
  await page.locator("#agentInterrupt").click();
  await waitFor(async () => (await control("activity")).interrupts.length === 1);
  assert.deepEqual((await control("activity")).interrupts[0], { threadId, turnId: "active-turn" });
  await control("activity", { state: "idle", turnId: "active-turn", itemCount: 3 });
  await page.locator("#agentInterrupt").waitFor({ state: "hidden" });
  await page.close();

  // A resume can report active just before the durable completion is observed.
  const completed = await browser.newPage();
  await completed.goto(`${fixture.origin}/?thread=${threadId}`);
  await completed.locator("#conversationInput").fill("Next draft");
  await waitFor(async () => (await control("resume")).requests === 2);
  await control("resume", { active: true });
  await completed.locator("#resumeProgress").waitFor({ state: "hidden" });
  await completed.locator("#agentInterrupt").waitFor({ state: "hidden" });
  await completed.locator("#conversationSend").waitFor({ state: "visible" });
  assert.deepEqual(errors, []);
  console.log("Active resume: lightweight state, heartbeat reuse, correct stop turn and completed-state reconciliation passed");
} finally {
  await browser?.close();
  await fixture.close();
}

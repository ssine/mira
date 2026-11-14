import assert from "node:assert/strict";
import { startQuestionFixture, questionThread } from "./claude_questions_fixture.mjs";
const { chromium } = await import(process.argv[2] ?? "playwright");
const turns = 60;
const fixture = await startQuestionFixture({ longHistory: turns });
const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } }), errors = [];
  page.setDefaultTimeout(20_000);
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(`${fixture.origin}/?thread=${questionThread}`);
  await page.waitForFunction(() => document.querySelector("#conversationTrace")?.textContent.includes(`第 60 个回答`));

  // Without scrolling, every older page loads and the rail lists every message.
  const rail = page.locator("#conversationRail"), ticks = rail.locator(".message-rail-tick");
  await page.waitForFunction(count => document.querySelectorAll("#conversationRail .message-rail-tick").length === count, turns);
  assert.equal(await page.locator("#conversationTrace .trace-card.user").count(), turns, "every user message has a card (prose mounts near the viewport)");
  assert.equal(await page.locator("#conversationTrace .history-loader").count(), 0, "nothing is left to load");
  const state = await (await fetch(`${fixture.origin}/__test/state`)).json();
  assert.ok(state.pageRequests.length >= Math.ceil(turns * 3 / 30), `older pages were read: ${state.pageRequests}`);
  assert.ok(await page.evaluate(() => document.querySelector("#conversationScroll").scrollHeight - document.querySelector("#conversationScroll").clientHeight - document.querySelector("#conversationScroll").scrollTop < 4),
    "the reader stays at the latest reply while history fills");
  // Long transcripts mount prose only near the viewport.
  const mounted = await page.locator("#conversationTrace .trace-card.assistant .trace-body .markdown-body, #conversationTrace .trace-card.assistant .trace-body p").count();
  assert.ok(mounted < turns * 6, `offscreen prose stays unmounted: ${mounted}`);

  // The rail spans the reading area minus 10% at each end, without a fixed maximum.
  const geometry = await page.evaluate(() => {
    const scroller = document.querySelector("#conversationScroll").getBoundingClientRect();
    const rail = document.querySelector("#conversationRail").getBoundingClientRect();
    const list = document.querySelector("#conversationRail .message-rail-list");
    return { scroll: scroller.height, railTop: rail.top - scroller.top, railBottom: scroller.bottom - rail.bottom, height: rail.height,
      list: list.getBoundingClientRect().height, overflow: list.scrollHeight - list.clientHeight };
  });
  assert.ok(Math.abs(geometry.height - geometry.scroll * 0.8) < 3, `rail is 80% of the reading area: ${JSON.stringify(geometry)}`);
  assert.ok(Math.abs(geometry.railTop - geometry.railBottom) < 3 && geometry.railTop > geometry.scroll * 0.09, `10% margins: ${JSON.stringify(geometry)}`);
  assert.ok(geometry.list > geometry.height * 0.95 && geometry.overflow <= 1, `60 ticks fill the rail without scrolling: ${JSON.stringify(geometry)}`);

  // Pressing and dragging scrubs like a scrollbar: the transcript follows the pointer.
  const userCard = index => page.locator("#conversationTrace .trace-card.user").nth(index);
  const near = async index => page.waitForFunction(index => {
    const card = document.querySelectorAll("#conversationTrace .trace-card.user")[index];
    const overlay = Math.max(document.querySelector("#conversationScroll").getBoundingClientRect().top, document.querySelector(".conversation-notices").getBoundingClientRect().bottom, document.querySelector("#conversationTimeline").hidden ? 0 : document.querySelector("#conversationTimeline").getBoundingClientRect().bottom);
    const offset = card?.getBoundingClientRect().top - overlay;
    return offset >= 0 && offset < 40;
  }, index);
  const box = index => ticks.nth(index).boundingBox();
  const start = await box(2), middle = await box(30), end = await box(45);
  await page.mouse.move(start.x + start.width / 2, start.y + start.height / 2);
  await page.mouse.down();
  await near(2);
  await page.mouse.move(middle.x + middle.width / 2, middle.y + middle.height / 2, { steps: 6 });
  await near(30);
  assert.match(await rail.locator(".message-rail-tip").textContent(), /^第 31 个问题/, "the preview follows the scrub");
  assert.equal(await rail.getAttribute("data-scrubbing"), "true");
  // Leaving the rail sideways keeps scrubbing (pointer capture).
  await page.mouse.move(end.x - 200, end.y + end.height / 2, { steps: 6 });
  await near(45);
  await page.mouse.up();
  assert.equal(await rail.getAttribute("data-scrubbing"), null);
  assert.ok(await userCard(45).evaluate(card => card.classList.contains("trace-card-flash")), "release highlights the message");
  await page.waitForFunction(() => document.querySelector('#conversationRail [aria-current="true"]')?.dataset.railIndex === "45");
  // A plain click still jumps.
  await ticks.nth(10).click();
  await near(10);
  // A message just sent joins the rail without a reload.
  await page.locator("#conversationInput").fill("新发送的一条消息");
  await page.locator("#conversationSend").click();
  await page.waitForFunction(count => document.querySelectorAll("#conversationRail .message-rail-tick").length === count, turns + 1);
  await page.locator("#conversationNavToggle").click();
  await page.waitForFunction(() => document.querySelector("#conversationNavMenu [data-nav-index='60'] .nav-item-text")?.textContent === "新发送的一条消息");
  await page.keyboard.press("Escape");
  assert.deepEqual(errors, []);
  console.log("PASS: whole Claude history loads on open, adaptive rail height, scrubbing and click jumps, rail grows with a sent message");
} finally { await browser.close(); await fixture.close(); }

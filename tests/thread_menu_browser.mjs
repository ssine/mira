import assert from "node:assert/strict";
import { startDraftFixture } from "./composer_drafts_fixture.mjs";
import { closeSidebar } from "./sidebar_browser_helpers.mjs";
const { chromium } = await import(process.argv[2] ?? "playwright");
const fixture = await startDraftFixture({ menuTest: true });
let browser;
const a = "00000000-0000-4000-8000-0000000000a1", b = "00000000-0000-4000-8000-0000000000b2";
try {
  browser = await chromium.launch({ headless: true });
  for (const viewport of [{ width: 1440, height: 1000 }, { width: 390, height: 844 }]) {
    const mobile = viewport.width === 390;
    const context = await browser.newContext({ viewport, hasTouch: mobile });
    const page = await context.newPage(), errors = [];
    page.on("pageerror", error => errors.push(error.message));
    await page.goto(`${fixture.origin}/?thread=${a}`);
    await page.locator(".trace-card.assistant").waitFor();
    if (mobile) await page.locator("#agentThreadDrawerToggle").click();
    const button = page.locator(`[data-thread-id="${b}"]`), menu = page.locator("#threadOptionsMenu");
    await button.click({ button: "right" });
    await menu.waitFor({ state: "visible" });
    assert.equal(await page.locator("#threadOpenWindow").getAttribute("href"), `/?thread=${b}`);
    assert.equal(new URL(page.url()).searchParams.get("thread"), a);
    await page.keyboard.press("Escape");
    assert.equal(await button.evaluate(element => element === document.activeElement), true, "Escape restores row focus");
    await page.keyboard.press("Shift+F10"); await menu.waitFor({ state: "visible" });
    await page.keyboard.press("Escape");
    await page.keyboard.press("ContextMenu"); await menu.waitFor({ state: "visible" });
    await page.keyboard.press("Escape");
    if (mobile) await closeSidebar(page);
    await page.locator("#conversationMenuToggle").click();
    assert.equal(await page.locator("#threadOpenWindow").getAttribute("href"), `/?thread=${a}`, "The visible header menu operates on the current conversation");
    await page.keyboard.press("Escape");
    const result = await page.evaluate(async () => (await import("/__test/thread-menu.mjs")).runThreadMenuScenarios());
    assert.match(result, /^PASS:/);
    console.log(`${mobile ? "Mobile" : "Desktop"}: ${result}`);
    if (mobile) {
      // Real contact delivery complements the browser-native cancellation cases.
      const session = await context.newCDPSession(page);
      const bounds = await button.boundingBox(), x = bounds.x + bounds.width / 2, y = bounds.y + bounds.height / 2;
      await session.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y, id: 1 }] });
      await menu.waitFor({ state: "visible" });
      await session.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
      assert.equal(new URL(page.url()).searchParams.get("thread"), a, "Trusted long-press release does not navigate");
      await page.keyboard.press("Escape");
      await session.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y, id: 2 }] });
      await session.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x: x - 60, y, id: 2 }] });
      await session.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
      await page.waitForTimeout(600);
      assert.equal(await menu.isVisible(), false, "A sidebar swipe cancels the native long press");
      assert.equal(await page.locator(".drawer-dragging").count(), 0);
    }
    assert.deepEqual(errors, []);
    await context.close();
  }
} finally {
  await browser?.close();
  await fixture.close();
}

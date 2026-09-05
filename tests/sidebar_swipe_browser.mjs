// Trusted touch input in mobile Chromium; no simulated DOM gesture handlers.
// Run against the disposable Server with MIRA_SERVER_URL and its test administrator.
import assert from 'node:assert/strict';
const { chromium, devices } = await import(process.argv[2] ?? 'playwright');
const origin = process.env.MIRA_SERVER_URL ?? 'http://127.0.0.1:8789';
const threadId = '00000000-0000-4000-8000-0000000000a1';
let browser;
try {
  browser = await chromium.launch({ headless: true, ...(process.env.MIRA_BROWSER_EXECUTABLE ? { executablePath: process.env.MIRA_BROWSER_EXECUTABLE } : {}) });
  const context = await browser.newContext({ ...devices['Pixel 7'] });
  const thread = { threadId, title: 'Swipe test', cwd: '/work', generation: 1 };
  await context.route('**/v1/codex/threads?*', route => route.fulfill({ json: { data: [thread] } }));
  await context.route(`**/v1/codex/threads/${threadId}?*`, route => route.fulfill({ json: thread }));
  const body = 'Reading text for a swipe from the middle of the conversation.\n\n'.repeat(50) +
    '\n```text\n' + 'a long horizontal code block '.repeat(50) + '\n```\n\n[Keep link gestures](https://example.com/)';
  await context.route('**/v1/codex/threads/*/transcript?*', route => route.fulfill({ json: { generation: 1, trace: [{ key: 'prose', kind: 'assistant', body, turnId: 'turn' }] } }));
  const page = await context.newPage();
  page.setDefaultTimeout(10_000);
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.goto(origin);
  await page.locator('#password').fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? 'mira-local-admin-password');
  await page.locator('#loginForm button[type=submit]').click();
  await page.locator('#dashboardView:not(.hidden)').waitFor();
  await page.goto(`${origin}/?thread=${threadId}`);
  await page.locator('.trace-card.assistant').waitFor();
  const session = await context.newCDPSession(page);
  await session.send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });
  await page.evaluate(() => { window.touchCount = 0; for (const type of ['touchstart', 'touchmove', 'touchend', 'touchcancel']) document.addEventListener(type, event => { window.touchCount = event.touches.length; }, { passive: true, capture: true }); });
  const scroll = page.locator('#conversationScroll');
  const drawer = page.locator('#agentThreadDrawer');
  const closed = async () => assert.equal(await drawer.getAttribute('aria-hidden'), 'true');
  const touch = (x, y, id = 0) => ({ x, y, id, radiusX: 2, radiusY: 2, force: 1 });
  const swipe = async (x, y, dx, dy = 0, { cancel = false, delay = 12, hold = 0, steps = 8 } = {}) => {
    const points = (xx, yy) => [touch(xx, yy)];
    await session.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: points(x, y) });
    if (hold) await page.waitForTimeout(hold);
    for (let step = 1; step <= steps; step++) {
      await session.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: points(x + dx * step / steps, y + dy * step / steps) });
      await page.waitForTimeout(delay);
    }
    await session.send('Input.dispatchTouchEvent', { type: cancel ? 'touchCancel' : 'touchEnd', touchPoints: [] });
    await page.waitForTimeout(100);
    assert.equal(await page.evaluate(() => window.touchCount), 0, 'all test fingers are lifted');
  };
  const beginDrag = (x, y) => session.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [touch(x, y)] });
  const dragTo = async (x, y) => {
    await session.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [touch(x, y)] });
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => resolve())));
  };
  const endDrag = async () => {
    await session.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    await page.waitForTimeout(250);
  };
  const revealed = () => drawer.evaluate(element => element.getBoundingClientRect().right);
  const follows = async expected => assert.ok(Math.abs(await revealed() - expected) <= 2, `drawer follows the finger to ${expected}px`);
  const readingPosition = async () => {
    await scroll.evaluate(element => { element.scrollTop = 300; });
    await page.waitForTimeout(100);
  };
  await closed(); await readingPosition();
  const before = await scroll.evaluate(element => element.scrollTop);
  await swipe(155, 300, 160, 8);
  assert.equal(await drawer.getAttribute('aria-hidden'), 'false', 'right swipe from the middle opens the drawer');
  assert.equal(await page.locator('#agentThreadDrawerToggle').getAttribute('aria-expanded'), 'true');
  assert.equal(await drawer.evaluate(element => element.inert), false);
  assert.equal(await scroll.evaluate(element => element.scrollTop), before, 'opening does not move the reading position');
  assert.equal(new URL(page.url()).searchParams.get('thread'), threadId, 'right swipe does not navigate browser history');
  await page.locator('#agentThreadDrawerClose').tap(); await closed();
  await readingPosition();
  // Inspect intermediate frames with the finger still down, including reversal
  // and a pause longer than the old 1.5 second gesture timeout.
  await beginDrag(90, 300);
  await dragTo(130, 300); await follows(40);
  await dragTo(200, 308); await follows(110);
  const shade = await page.locator('#agentThreadDrawerBackdrop').evaluate(element => Number(getComputedStyle(element).opacity));
  assert.ok(Math.abs(shade - 110 / 340) < 0.02, 'backdrop tracks the same progress');
  await dragTo(250, 312); await follows(160);
  await page.waitForTimeout(1600);
  await follows(160);
  await endDrag();
  assert.equal(await drawer.getAttribute('aria-hidden'), 'false', 'paused drag settles by distance');
  await beginDrag(250, 300);
  await dragTo(190, 300); await follows(280);
  await dragTo(80, 300); await follows(170);
  await endDrag(); await closed();
  // A fast short flick commits; a slow short drag returns to its starting state.
  await readingPosition();
  await swipe(100, 300, 80, 0, { steps: 4, delay: 0 });
  assert.equal(await drawer.getAttribute('aria-hidden'), 'false', 'short flick opens the drawer');
  await page.locator('#agentThreadDrawerClose').tap();
  await readingPosition(); await beginDrag(100, 300); await dragTo(170, 300);
  await page.waitForTimeout(200); await endDrag(); await closed();
  await readingPosition(); await beginDrag(90, 300); await dragTo(270, 300); await follows(180);
  await dragTo(190, 300); await follows(100);
  await dragTo(120, 300); await follows(30);
  await endDrag(); await closed();
  // There is no narrow start band: both the gutter and right half work.
  for (const x of [20, 245]) {
    await readingPosition(); await swipe(x, 300, 100, 0, { steps: 4, delay: 0 });
    assert.equal(await drawer.getAttribute('aria-hidden'), 'false', `flick from x=${x}`);
    await page.locator('#agentThreadDrawerClose').tap();
  }
  await readingPosition(); await swipe(100, 300, 160, 110);
  assert.equal(await drawer.getAttribute('aria-hidden'), 'false', 'mostly horizontal diagonal drag remains usable');
  assert.equal(await scroll.evaluate(element => element.scrollTop), 300, 'horizontal axis lock preserves reading position');
  await page.locator('#agentThreadDrawerClose').tap();
  await readingPosition();

  await swipe(170, 430, 5, -190);
  await closed();
  assert.ok(await scroll.evaluate(element => element.scrollTop) > 400, 'vertical touch scroll still moves the transcript');
  await readingPosition(); await swipe(170, 300, 100, 125); await closed();
  await readingPosition(); await swipe(170, 300, 30, 0); await closed();
  await readingPosition(); await swipe(260, 300, -150, 5); await closed();
  await readingPosition(); await swipe(170, 300, 130, 0, { cancel: true }); await closed();
  await readingPosition(); await swipe(170, 300, 130, 0, { hold: 550 }); await closed();
  await page.evaluate(() => getSelection().removeAllRanges());
  await readingPosition();
  await page.locator('.trace-body p').first().evaluate(element => { const range = document.createRange(); range.selectNodeContents(element); getSelection().removeAllRanges(); getSelection().addRange(range); });
  await swipe(170, 300, 130, 0); await closed();
  await page.evaluate(() => getSelection().removeAllRanges());
  const code = page.locator('.trace-body pre');
  await code.scrollIntoViewIfNeeded();
  await code.evaluate(element => { element.scrollLeft = 300; });
  const bounds = await code.boundingBox();
  assert.ok(bounds.width < await code.evaluate(element => element.scrollWidth));
  const previousLeft = await code.evaluate(element => element.scrollLeft);
  await swipe(bounds.x + 110, bounds.y + Math.min(25, bounds.height / 2), 140, 0);
  await closed();
  assert.ok(await code.evaluate(element => element.scrollLeft) < previousLeft, 'code blocks keep horizontal touch scrolling');
  const link = page.locator('.trace-body a');
  await link.scrollIntoViewIfNeeded();
  const linkBounds = await link.boundingBox();
  await link.evaluate(element => { window.linkTaps = 0; element.addEventListener('click', event => { event.preventDefault(); window.linkTaps++; }); });
  await swipe(linkBounds.x + 20, linkBounds.y + linkBounds.height / 2, 140);
  assert.equal(await drawer.getAttribute('aria-hidden'), 'false', 'swipes starting on a link also open the drawer');
  assert.equal(await page.evaluate(() => window.linkTaps), 0, 'drag does not activate its starting link');
  assert.equal(new URL(page.url()).searchParams.get('thread'), threadId);
  await page.locator('#agentThreadDrawerClose').tap(); await page.waitForTimeout(250);
  await link.tap(); assert.equal(await page.evaluate(() => window.linkTaps), 1, 'ordinary link taps still work');
  // Tool summaries have the same drag/tap distinction as links.
  await page.locator('.trace-body').evaluate(element => { const details = document.createElement('details'); details.innerHTML = '<summary>Tool details</summary><p>Tool evidence</p>'; element.append(details); });
  const summary = page.locator('.trace-body summary'); await summary.scrollIntoViewIfNeeded();
  const summaryBounds = await summary.boundingBox();
  await swipe(summaryBounds.x + 30, summaryBounds.y + summaryBounds.height / 2, 150);
  assert.equal(await drawer.getAttribute('aria-hidden'), 'false', 'tool rows accept dragging');
  assert.equal(await summary.evaluate(element => element.parentElement.open), false, 'drag does not toggle tool evidence');
  await page.locator('#agentThreadDrawerClose').tap(); await page.waitForTimeout(250);
  await summary.tap(); assert.equal(await summary.evaluate(element => element.parentElement.open), true, 'tool evidence still expands on tap');
  const input = page.locator('#conversationInput');
  await input.fill('Unchanged draft');
  const inputBounds = await input.boundingBox();
  await swipe(inputBounds.x + 40, inputBounds.y + 15, 100, 0); await closed();
  assert.equal(await input.inputValue(), 'Unchanged draft');
  await input.blur();
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await readingPosition();
  await swipe(90, 300, 180, 0);
  assert.equal(await drawer.getAttribute('aria-hidden'), 'false', 'gesture also works with reduced motion');
  await page.locator('#agentThreadDrawerClose').tap();
  await readingPosition(); await beginDrag(90, 300); await dragTo(170, 300); await follows(80);
  await page.setViewportSize({ width: 1200, height: 900 });
  await session.send('Input.dispatchTouchEvent', { type: 'touchCancel', touchPoints: [] });
  await page.waitForFunction(() => !document.querySelector('#agentThreadDrawer').hasAttribute('inert'));
  assert.equal(await drawer.evaluate(element => element.style.transform), '', 'resize clears unfinished drag styles');
  await page.locator('#agentThreadDrawerClose').tap();
  await readingPosition(); await swipe(400, 300, 160, 0); await closed();
  // Multi-touch uses Chromium's native gesture synthesizer on a fresh mobile page.
  await page.setViewportSize(devices['Pixel 7'].viewport);
  await page.reload();
  await page.locator('.trace-card.assistant').waitFor();
  await page.evaluate(() => { window.maxTouches = 0; document.addEventListener('touchstart', event => { window.maxTouches = Math.max(window.maxTouches, event.touches.length); }, { passive:true, capture:true }); });
  await readingPosition();
  await session.send('Input.synthesizePinchGesture', { x:190, y:300, scaleFactor:1.5, gestureSourceType:'touch' });
  await closed();
  assert.equal(await page.evaluate(() => window.maxTouches), 2, 'multi-finger input does not open the drawer');
  assert.deepEqual(errors, []);
  console.log('PASS: continuous opening/closing drag, progress/reversal/pause/flick, broad touch targets, drawer accessibility, stable history/scroll, vertical and code panning, direction/threshold/cancel/multitouch/selection, draft preservation, reduced motion and wide-layout exclusion');
} finally { await browser?.close(); }

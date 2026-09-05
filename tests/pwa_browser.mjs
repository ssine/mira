// Real browser + disposable Server: install metadata, cold launch, mobile viewport,
// HTTP validation, offline recovery and exclusion of private data from SW caches.
// MIRA_SERVER_URL=http://127.0.0.1:8789 node tests/pwa_browser.mjs [playwright-module]
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";

const { chromium, devices } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8789";
const thread = "00000000-0000-4000-8000-0000000000a1";
const other = "00000000-0000-4000-8000-0000000000b2";
const cwd = "/workspace/" + "long-project-directory/".repeat(8);
const profile = await fs.mkdtemp(path.join(os.tmpdir(), "mira-pwa-browser-"));
let context;
try {
  context = await chromium.launchPersistentContext(profile, { ...devices["Pixel 7"], headless: true,
    ...(process.env.MIRA_BROWSER_EXECUTABLE ? { executablePath: process.env.MIRA_BROWSER_EXECUTABLE } : {}) });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error" && /content security policy/i.test(message.text())) errors.push(message.text());
  });
  await context.route("**/v1/codex/threads?*", (route) => route.fulfill({ json: { data: [{ threadId: thread, title: "手机会话", cwd, model: "gpt-6-astra", updatedAt: new Date().toISOString() }] } }));
  await context.route(/\/v1\/codex\/threads\/[^/?]+\?storeId=personal$/, (route) => route.fulfill({ json: { threadId: other, title: "另一个会话", cwd, model: "gpt-6-astra" } }));
  await context.route("**/v1/codex/threads/*/transcript?*", (route) => route.fulfill({ json: {
    generation: 1, trace: [{ key: "message", kind: "assistant", turnId: "turn", body: "手机上的对话正文。\n\n".repeat(100) }],
  } }));

  await page.goto(origin, { waitUntil: "networkidle" });
  await page.evaluate(async () => { await navigator.serviceWorker.ready; });
  await page.waitForFunction(() => Boolean(navigator.serviceWorker.controller));
  const session = await context.newCDPSession(page);
  const manifest = await session.send("Page.getAppManifest");
  assert.deepEqual(manifest.errors, []);
  const metadata = JSON.parse(manifest.data);
  assert.equal(metadata.display, "standalone");
  assert.equal(metadata.start_url, "/?launch=pwa");
  assert.equal(metadata.id, "/");
  for (const icon of metadata.icons) {
    const dimensions = await page.evaluate(async (src) => {
      const image = new Image(); image.src = src; await image.decode(); return `${image.width}x${image.height}`;
    }, icon.src);
    assert.equal(dimensions, icon.sizes);
  }
  const eligibility = await session.send("Page.getInstallabilityErrors");
  assert.deepEqual(eligibility.installabilityErrors, [], "Chrome must consider the page installable");
  const asset = await context.request.get(`${origin}/app.js`);
  const validated = await context.request.get(`${origin}/app.js`, { headers: { "if-none-match": asset.headers().etag } });
  assert.equal(validated.status(), 304);
  assert.equal((await validated.body()).length, 0);
  const head = await context.request.head(`${origin}/manifest.webmanifest`);
  assert.equal(head.status(), 200);
  assert.match(head.headers()["content-type"], /application\/manifest\+json/);

  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator("#loginForm button[type=submit]").click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  // Suppress a native dialog for the fallback-help test, then exercise the
  // user-gesture prompt path independently below.
  await page.evaluate(() => window.dispatchEvent(new Event("appinstalled")));
  await page.evaluate(() => {
    document.querySelector("#dashboardView [data-install-app]").classList.remove("hidden");
  });
  await page.locator("#dashboardView [data-install-app]").click();
  await page.locator(".pwa-install-dialog[open]").waitFor();
  await page.locator(".pwa-install-dialog button").click();
  await page.evaluate(() => {
    const event = new Event("beforeinstallprompt", { cancelable: true });
    event.prompt = async () => { document.body.dataset.installPromptShown = "true"; };
    event.userChoice = Promise.resolve({ outcome: "accepted" });
    window.dispatchEvent(event);
  });
  await page.locator("#dashboardView [data-install-app]").click();
  assert.equal(await page.locator("body").getAttribute("data-install-prompt-shown"), "true");
  await page.evaluate(() => window.dispatchEvent(new Event("appinstalled")));
  assert.equal(await page.locator("#dashboardView [data-install-app]").isVisible(), false);
  await page.goto(`${origin}/?thread=${thread}`, { waitUntil: "networkidle" });
  await page.locator(".trace-card.assistant").waitFor();
  assert.equal(await page.evaluate(() => localStorage.getItem("mira.app.route")), `/?thread=${thread}`);
  await page.setViewportSize({ width: 320, height: 640 });
  const header = await page.locator(".conversation-head").evaluate((node) => {
    const directory = node.querySelector(".conversation-directory");
    const model = node.querySelector(".conversation-model");
    return { height: node.getBoundingClientRect().height, directory: directory.textContent, model: model.textContent,
      directoryTop: directory.getBoundingClientRect().top, modelTop: model.getBoundingClientRect().top,
      directoryClipped: directory.scrollWidth > directory.clientWidth, modelClipped: model.scrollWidth > model.clientWidth,
      meta: node.querySelector("#conversationMeta").textContent };
  });
  assert.equal(header.directory, cwd);
  assert.equal(header.model, "gpt-6-astra");
  assert.ok(header.height <= 48, `compact header: ${JSON.stringify(header)}`);
  assert.equal(header.directoryTop, header.modelTop, "directory and model stay on one line");
  assert.equal(header.directoryClipped, true);
  assert.equal(header.modelClipped, false, "a long directory must leave the model readable");
  assert.equal(header.meta.includes(thread), false);
  assert.equal(await page.locator("#conversationTitle").isVisible(), true);
  assert.ok(await page.locator("#conversationTitle").evaluate((node) => getComputedStyle(node).clipPath === "none" && node.getBoundingClientRect().width > 1));
  await page.setViewportSize({ width: 393, height: 851 });

  await page.locator("#conversationInput").fill("全屏切换时保留的草稿");
  await page.locator("#conversationScroll").evaluate((scroll) => { scroll.scrollTop = 400; });
  await page.locator("#agentThreadDrawerToggle").click();
  const fullscreen = page.locator("#agentFullscreenToggle");
  await fullscreen.click();
  await page.waitForFunction(() => document.fullscreenElement === document.documentElement);
  assert.equal(await fullscreen.getAttribute("aria-pressed"), "true");
  assert.equal(await page.locator("#agentFullscreenLabel").textContent(), "退出全屏");
  await page.locator("#agentThreadDrawerClose").click();
  assert.equal(await page.locator("#conversationInput").inputValue(), "全屏切换时保留的草稿");
  assert.ok(page.url().endsWith(`/?thread=${thread}`));
  assert.ok(Math.abs(await page.locator("#conversationScroll").evaluate((scroll) => scroll.scrollTop) - 400) <= 1);
  for (const viewport of [{ width: 393, height: 430 }, { width: 851, height: 393 }, { width: 393, height: 851 }]) {
    await page.setViewportSize(viewport);
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    const bounds = await page.evaluate(() => ({ bottom: document.querySelector("#conversationForm").getBoundingClientRect().bottom,
      viewport: visualViewport.height + visualViewport.offsetTop, overflow: document.documentElement.scrollWidth > innerWidth + 1 }));
    assert.ok(Math.abs(bounds.bottom - bounds.viewport) <= 1, `fullscreen composer follows viewport: ${JSON.stringify(bounds)}`);
    assert.equal(bounds.overflow, false);
  }
  await page.locator("#agentThreadDrawerToggle").click();
  await fullscreen.click();
  await page.waitForFunction(() => !document.fullscreenElement);
  assert.equal(await fullscreen.getAttribute("aria-pressed"), "false");
  assert.equal(await page.locator("#agentFullscreenLabel").textContent(), "全屏模式");
  await fullscreen.click();
  await page.waitForFunction(() => Boolean(document.fullscreenElement));
  await page.evaluate(() => document.exitFullscreen()); // The browser/system can exit independently.
  await page.waitForFunction(() => document.querySelector("#agentFullscreenToggle").getAttribute("aria-pressed") === "false");
  await page.evaluate(() => {
    window.originalRequestFullscreen = document.documentElement.requestFullscreen;
    document.documentElement.requestFullscreen = () => Promise.reject(new TypeError("Fullscreen denied"));
  });
  await fullscreen.click();
  await page.locator("#agentFullscreenStatus:not(.hidden)").waitFor();
  assert.equal(await fullscreen.isEnabled(), true, "a rejected fullscreen request allows retry");
  assert.equal(await fullscreen.getAttribute("aria-pressed"), "false");
  await page.evaluate(() => { document.documentElement.requestFullscreen = window.originalRequestFullscreen; });
  await fullscreen.click();
  await page.waitForFunction(() => Boolean(document.fullscreenElement));
  assert.equal(await page.locator("#agentFullscreenStatus").isVisible(), false);
  await fullscreen.click();
  await page.waitForFunction(() => !document.fullscreenElement);
  await page.locator("#agentThreadDrawerClose").click();

  // A real cold document load uses the install start URL and restores the route.
  await page.goto(`${origin}/?launch=pwa`, { waitUntil: "networkidle" });
  await page.waitForURL(`**/?thread=${thread}`);
  await page.locator(".trace-card.assistant").waitFor();
  await page.goto(`${origin}/?launch=pwa&thread=${other}`, { waitUntil: "networkidle" });
  await page.waitForURL(`**/?thread=${other}`);
  await page.locator(".trace-card.assistant").waitFor();

  const layout = () => page.evaluate(() => {
    const input = document.querySelector("#conversationInput");
    const box = input.getBoundingClientRect();
    return { bottom: box.bottom, top: box.top, height: visualViewport.height, width: innerWidth,
      scrollWidth: document.documentElement.scrollWidth, font: getComputedStyle(input).fontSize,
      touchTarget: document.querySelector("#conversationSend").getBoundingClientRect().height };
  });
  await page.locator("#conversationInput").fill("未发送的草稿");
  await page.setViewportSize({ width: 393, height: 430 });
  await page.waitForFunction(() => document.querySelector("#conversationInput").getBoundingClientRect().bottom <= visualViewport.height + 1);
  let measure = await layout();
  assert.ok(measure.bottom <= measure.height + 1 && measure.top >= 0);
  assert.ok(measure.scrollWidth <= measure.width + 1);
  assert.equal(measure.font, "14px");
  assert.ok(measure.touchTarget >= 44);
  const inputAlignment = await page.evaluate(() => {
    const input = document.querySelector("#conversationInput").getBoundingClientRect();
    const send = document.querySelector("#conversationSend").getBoundingClientRect();
    return Math.abs((input.top + input.bottom) / 2 - (send.top + send.bottom) / 2);
  });
  assert.ok(inputAlignment <= 1, `single-line text is centered beside the send button: ${inputAlignment}`);
  await page.evaluate(() => {
    const trace = document.querySelector("#conversationTrace");
    for (let i = 0; i < 40; i++) trace.append(trace.querySelector(".trace-card.assistant").cloneNode(true));
  });
  for (const position of [0, .5, 1]) {
    const anchored = await page.locator("#conversationScroll").evaluate(async (scroll, position) => {
      scroll.scrollTop = position * (scroll.scrollHeight - scroll.clientHeight);
      await new Promise(requestAnimationFrame);
      const form = document.querySelector("#conversationForm").getBoundingClientRect();
      return { bottom: form.bottom, viewport: visualViewport.height + visualViewport.offsetTop,
        documentHeight: document.documentElement.scrollHeight, windowHeight: innerHeight };
    }, position);
    assert.ok(Math.abs(anchored.bottom - anchored.viewport) <= 1, `mobile composer anchored at scroll ${position}`);
    assert.ok(anchored.documentHeight <= anchored.windowHeight + 1);
  }
  await page.locator("#conversationInput").fill("多行草稿\n".repeat(20));
  const multiline = await layout();
  assert.ok(multiline.bottom <= multiline.height + 1 && multiline.top >= 0);
  assert.ok(await page.locator("#conversationInput").evaluate((node) => node.getBoundingClientRect().height <= 144));
  await page.locator("#conversationInput").fill("未发送的草稿");
  assert.equal(await page.locator("#conversationInput").inputValue(), "未发送的草稿");
  await page.setViewportSize({ width: 851, height: 393 });
  await page.waitForFunction(() => document.querySelector("#conversationInput").getBoundingClientRect().bottom <= visualViewport.height + 1);
  measure = await layout();
  assert.ok(measure.scrollWidth <= measure.width + 1, "landscape must not scroll horizontally");
  await page.setViewportSize({ width: 393, height: 851 });
  await page.locator("#agentThemeToggle").click();
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  measure = await layout();
  assert.ok(measure.bottom <= measure.height + 1 && measure.top >= 0, `portrait keyboard recovery: ${JSON.stringify(measure)}`);
  assert.equal(await page.locator('meta[name="theme-color"]').getAttribute("content"), "#1f2226");
  if (process.env.MIRA_WEB_SCREENSHOT_DIR) {
    await fs.mkdir(process.env.MIRA_WEB_SCREENSHOT_DIR, { recursive: true });
    await page.screenshot({ path: `${process.env.MIRA_WEB_SCREENSHOT_DIR}/pwa-mobile.png` });
  }

  // Pinch zoom must remain available without shrinking/repositioning the shell.
  const beforeZoom = await page.evaluate(() => document.documentElement.style.getPropertyValue("--app-viewport-height"));
  await session.send("Emulation.setPageScaleFactor", { pageScaleFactor: 2 });
  await page.evaluate(() => new Promise(requestAnimationFrame));
  assert.equal(await page.evaluate(() => document.documentElement.style.getPropertyValue("--app-viewport-height")), beforeZoom);
  await session.send("Emulation.setPageScaleFactor", { pageScaleFactor: 1 });

  const cached = await page.evaluate(async () => {
    const entries = [];
    for (const key of await caches.keys()) {
      if (!key.startsWith("mira-pwa-")) continue;
      for (const request of await (await caches.open(key)).keys()) entries.push(new URL(request.url).pathname);
    }
    return entries.sort();
  });
  assert.deepEqual(cached, ["/icons/mira.svg", "/offline.css", "/offline.html", "/offline.js"]);
  await context.setOffline(true);
  const offline = await page.reload({ waitUntil: "domcontentloaded" });
  assert.equal(offline.status(), 200);
  await page.getByRole("heading", { name: "暂时无法连接 Mira" }).waitFor();
  assert.ok(page.url().endsWith(`/?thread=${other}`), "offline fallback keeps the original deep link");
  assert.equal(await page.locator(".trace-card").count(), 0, "offline page never exposes cached messages");
  await context.setOffline(false);
  await page.locator(".trace-card.assistant").waitFor({ timeout: 20000 });
  assert.ok(page.url().endsWith(`/?thread=${other}`));

  await page.locator("#agentThreadDrawerToggle").click();
  await page.locator("#agentHome").click();
  await page.locator("#logoutButton").click();
  await page.locator("#loginView:not(.hidden)").waitFor();
  assert.equal(await page.evaluate(() => localStorage.getItem("mira.app.route")), null);
  await page.goto(`${origin}/?launch=pwa`);
  await page.locator("#loginView:not(.hidden)").waitFor();
  assert.ok(page.url().endsWith("/?view=agent"));
  assert.deepEqual(errors, []);
  console.log("PASS: Chrome installability, icons, ETag/HEAD, launch/deep links, fullscreen entry/exit/retry, mobile/landscape/zoom, theme, offline recovery, private-cache exclusion, logout");
} finally { await context?.close(); await fs.rm(profile, { recursive: true, force: true }); }

// Layout preferences must govern geometry, navigation and interaction together.
import assert from "node:assert/strict";
import { closeSidebar } from "./sidebar_browser_helpers.mjs";

const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const browser = await chromium.launch({ headless: true });
try {
  const context = await browser.newContext({ viewport: { width: 1080, height: 1440 } });
  const rows = [1, 2].map(index => ({
    threadId: `00000000-0000-4000-8000-00000000000${index}`, title: `Layout conversation ${index}`,
    cwd: "/work", generation: 1, itemCount: 1, updatedAt: new Date().toISOString(),
  }));
  await context.route("**/v1/codex/threads?*", route => route.fulfill({ json: { data: rows } }));
  await context.route(/\/v1\/codex\/threads\/[^?]+\?/, route => {
    const path = new URL(route.request().url()).pathname;
    const row = rows.find(row => path.includes(row.threadId));
    return route.fulfill({ json: path.endsWith("/transcript") ? {
      generation: 1, storeVersion: 1, itemCount: 1,
      trace: [{ key: "message", turnId: "turn", kind: "assistant", body: row.title }],
    } : row });
  });
  const page = await context.newPage(), errors = [];
  page.setDefaultTimeout(10_000);
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?thread=${rows[0].threadId}`);
  await page.locator(".trace-card.assistant").waitFor();
  const drawer = page.locator("#agentThreadDrawer");
  const toggle = page.locator("#agentThreadDrawerToggle");
  const resize = page.locator("#agentSidebarResize");
  const openMenu = async () => {
    if (await drawer.getAttribute("aria-hidden") === "true") await toggle.click();
    await page.locator("#agentNavMenuToggle").click();
  };
  const choose = async mode => {
    await openMenu();
    await page.locator(`[data-sidebar-layout="${mode}"]`).click();
    assert.equal(await page.evaluate(() => localStorage.getItem("mira.sidebar.layout")), mode);
    assert.equal(await page.locator(`[data-sidebar-layout="${mode}"]`).getAttribute("aria-checked"), "true");
  };
  const geometry = async ({ docked, open = true, details = false }) => {
    await page.waitForFunction(({ docked, open }) => {
      const shell = document.querySelector(".chat-shell");
      return shell.classList.contains("sidebar-docked") === docked && shell.classList.contains("sidebar-open") === open;
    }, { docked, open });
    assert.equal(await drawer.getAttribute("aria-hidden"), String(!open));
    assert.equal(await toggle.getAttribute("aria-expanded"), String(open));
    assert.equal(await drawer.evaluate(element => element.inert), !open);
    assert.equal(await page.locator("#agentThreadDrawerBackdrop.open").isVisible(), open && !docked);
    assert.equal(await resize.isVisible(), open && docked);
    const rect = await page.locator(".conversation-card").boundingBox();
    const sidebarWidth = open && docked ? (await drawer.boundingBox()).width : 0;
    assert.ok(Math.abs(rect.x - sidebarWidth) <= 1, "only a docked sidebar reserves a column");
    assert.ok(Math.abs(rect.width - (page.viewportSize().width - sidebarWidth - (details ? 300 : 0))) <= 1);
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), "no page overflow");
  };

  await geometry({ docked: false, open: false });
  await choose("docked");
  await geometry({ docked: true });
  await page.locator(`[data-thread-id="${rows[1].threadId}"]`).click();
  await page.waitForURL(`**/?thread=${rows[1].threadId}`);
  await geometry({ docked: true });
  await resize.focus(); await page.keyboard.press("End");
  assert.equal((await drawer.boundingBox()).width, 480);
  await page.reload(); await page.locator(".trace-card.assistant").waitFor();
  await geometry({ docked: true });
  assert.equal((await drawer.boundingBox()).width, 480, "layout and width survive reload");

  // The old breakpoint must not reset either expanded or explicitly collapsed state.
  await page.setViewportSize({ width: 1280, height: 900 });
  await geometry({ docked: true });
  await closeSidebar(page);
  await page.setViewportSize({ width: 1080, height: 1440 });
  await geometry({ docked: true, open: false });
  await toggle.click();
  await page.setViewportSize({ width: 720, height: 1000 });
  await geometry({ docked: true });
  assert.equal((await drawer.boundingBox()).width, 240, "pinning leaves at least 480px for the conversation");
  await page.setViewportSize({ width: 390, height: 844 });
  await geometry({ docked: false, open: false });
  assert.equal(await page.evaluate(() => localStorage.getItem("mira.sidebar.layout")), "docked", "fallback retains preference");
  await openMenu();
  assert.equal(await page.locator("#agentSidebarLayoutNotice").isVisible(), true);
  await page.keyboard.press("Escape");
  await closeSidebar(page);
  await page.setViewportSize({ width: 1080, height: 1440 });
  await geometry({ docked: true });
  assert.equal((await drawer.boundingBox()).width, 480, "temporary clamping does not replace saved width");

  // A forced drawer stays an overlay at desktop widths, including with details open.
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.locator("#conversationDetailsToggle").click();
  await geometry({ docked: true, details: true });
  await choose("drawer");
  await geometry({ docked: false, details: true });
  await page.locator(`[data-thread-id="${rows[0].threadId}"]`).click();
  await page.waitForURL(`**/?thread=${rows[0].threadId}`);
  assert.equal(await drawer.getAttribute("aria-hidden"), "true", "drawer closes after selecting a conversation on desktop too");
  await page.reload(); await page.locator(".trace-card.assistant").waitFor();
  await geometry({ docked: false, open: false });
  await toggle.click(); await page.keyboard.press("Escape");
  await geometry({ docked: false, open: false });
  assert.equal(await toggle.evaluate(element => element === document.activeElement), true);

  await choose("auto");
  await geometry({ docked: true });
  await page.setViewportSize({ width: 1099, height: 1000 });
  await geometry({ docked: false, open: false });
  await page.setViewportSize({ width: 1100, height: 1000 });
  await geometry({ docked: true });

  // Preferences remain usable through keyboard navigation and unavailable storage.
  await openMenu();
  await page.locator('[data-sidebar-layout="docked"]').focus();
  await page.keyboard.press("Enter");
  await geometry({ docked: true });
  await page.addInitScript(() => {
    Storage.prototype.getItem = () => { throw new Error("Storage disabled"); };
    Storage.prototype.setItem = () => { throw new Error("Storage disabled"); };
  });
  await page.reload(); await page.locator(".trace-card.assistant").waitFor();
  await geometry({ docked: true });
  await openMenu();
  await page.locator('[data-sidebar-layout="drawer"]').click();
  await geometry({ docked: false });
  assert.deepEqual(errors, []);
  console.log("PASS: sidebar layout selection, persistence, fallback/restore, resizing, navigation, desktop drawer, details, keyboard and storage failure");
} finally {
  await browser.close();
}

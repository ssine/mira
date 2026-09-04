// Real-browser shell regression against a disposable Mira Server.
// Usage: MIRA_SERVER_URL=http://127.0.0.1:8787 MIRA_TEST_ADMIN_PASSWORD=... node tests/web_shell_browser.mjs [playwright-module]
import assert from "node:assert/strict";
import fs from "node:fs/promises";

const { chromium } = await import(process.argv[2] ?? "playwright");
const serverUrl = (process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787").replace(/\/$/, "");
const password = process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password";
const screenshotDirectory = process.env.MIRA_WEB_SCREENSHOT_DIR;

let browser;
try {
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  const page = await context.newPage();
  const pageErrors = [];
  const cspErrors = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error" && /content security policy/i.test(message.text())) cspErrors.push(message.text());
  });

  await page.goto(serverUrl, { waitUntil: "networkidle" });
  await page.locator("#loginView:not(.hidden)").waitFor();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "light");
  await page.locator("#password").fill(password);
  await page.locator("#loginForm button[type=submit]").click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  assert.equal(await page.locator("#globalNav").isVisible(), true);
  assert.equal(await page.locator("#globalNodes").getAttribute("aria-current"), "page");
  assert.equal(await page.locator(".metrics article").count(), 4);
  assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--accent").trim()), "#0078d4");

  if (screenshotDirectory) {
    await fs.mkdir(screenshotDirectory, { recursive: true });
    await page.screenshot({ path: `${screenshotDirectory}/dashboard-light.png`, fullPage: true });
  }

  await page.locator("#globalAgent").click();
  await page.locator("#agentView:not(.hidden)").waitFor();
  assert.equal(await page.locator("#globalAgent").getAttribute("aria-current"), "page");
  assert.equal(await page.locator(".agent-layout").count(), 1);
  if (screenshotDirectory) await page.screenshot({ path: `${screenshotDirectory}/agent-light.png`, fullPage: true });

  await page.locator("#themeToggle").click();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "dark");
  assert.equal(await page.evaluate(() => localStorage.getItem("mira.theme")), "dark");
  await page.reload({ waitUntil: "networkidle" });
  assert.equal(await page.locator("html").getAttribute("data-theme"), "dark");
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--accent").trim()), "#6cb8f6");

  await page.setViewportSize({ width: 390, height: 844 });
  assert.equal(await page.locator("#globalNodes").isVisible(), true);
  assert.equal(await page.locator("#logoutButton").isVisible(), true);
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1));
  if (screenshotDirectory) await page.screenshot({ path: `${screenshotDirectory}/dashboard-mobile-dark.png`, fullPage: true });

  assert.deepEqual(pageErrors, []);
  assert.deepEqual(cspErrors, []);
  console.log("PASS: Mira shell login, CSP-safe theme bootstrap, global navigation, theme persistence and narrow layout");
} finally {
  await browser?.close();
}

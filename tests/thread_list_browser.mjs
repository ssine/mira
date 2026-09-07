import assert from "node:assert/strict";

const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
const now = Date.now();
const row = (project, index, ageDays) => ({
  threadId: `${project === "/alpha" ? "10000000" : "20000000"}-0000-4000-8000-${String(index).padStart(12, "0")}`,
  title: `${project} conversation ${index}`,
  cwd: project,
  generation: 1,
  itemCount: 1,
  updatedAt: new Date(now - ageDays * 24 * 60 * 60 * 1000).toISOString(),
});
const threads = [
  ...[0, 1, 2, 3, 4, 5, 8].map((days, index) => row("/alpha", index + 1, days)),
  ...[0, 1, 2, 3, 4].map((days, index) => row("/beta", index + 1, days)),
];

const browser = await chromium.launch({ headless: true,
  ...(process.env.MIRA_BROWSER_EXECUTABLE ? { executablePath: process.env.MIRA_BROWSER_EXECUTABLE } : {}) });
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  await context.route("**/v1/codex/threads?*", route => {
    const archived = new URL(route.request().url()).searchParams.get("archived") === "1";
    return route.fulfill({ json: { data: archived ? [] : threads } });
  });
  const page = await context.newPage(), errors = [];
  page.setDefaultTimeout(10_000);
  page.on("pageerror", error => errors.push(error.message));
  await page.goto(origin);
  await page.locator("#password").fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password");
  await page.locator('#loginForm button[type="submit"]').click();
  await page.locator("#dashboardView:not(.hidden)").waitFor();
  await page.goto(`${origin}/?view=agent`);
  const projects = page.locator(".thread-project");
  await projects.first().waitFor();
  assert.equal(await projects.count(), 2);
  for (const project of [projects.nth(0), projects.nth(1)]) {
    assert.equal(await project.locator(".thread-project-threads > .agent-thread-row").count(), 4);
  }
  const alpha = projects.filter({ has: page.locator(".thread-project-identity strong", { hasText: "alpha" }) });
  const beta = projects.filter({ has: page.locator(".thread-project-identity strong", { hasText: "beta" }) });
  const alphaHistory = alpha.locator(".thread-project-history");
  const betaHistory = beta.locator(".thread-project-history");
  assert.equal(await alphaHistory.locator("summary").textContent(), "展开 3 个隐藏对话");
  assert.equal(await betaHistory.locator("summary").textContent(), "展开 1 个隐藏对话");
  assert.equal(await alphaHistory.locator(".agent-thread-row").first().isVisible(), false);
  await alphaHistory.locator("summary").click();
  await alphaHistory.locator("summary").getByText("收起 3 个较早对话", { exact: true }).waitFor();
  assert.equal(await alphaHistory.locator("summary").textContent(), "收起 3 个较早对话");
  assert.equal(await alphaHistory.locator(".agent-thread-row:visible").count(), 3);
  assert.equal(await betaHistory.locator(".agent-thread-row").first().isVisible(), false);

  // A normal in-page list reload keeps the user's expanded project history.
  await page.locator("#agentNavMenuToggle").click();
  await page.locator("#agentArchiveToggle").click();
  await page.locator(".agent-list-empty").waitFor();
  await page.locator("#agentNavMenuToggle").click();
  await page.locator("#agentArchiveToggle").click();
  await projects.first().waitFor();
  assert.equal(await alpha.locator(".thread-project-history").getAttribute("open"), "");
  assert.deepEqual(errors, []);
  console.log("PASS: each project shows four recent conversations and keeps older/overflow conversations in a persistent collapsed section");
} finally {
  await browser.close();
}

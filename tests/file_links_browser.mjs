// File references in Agent output, end to end: a real Server, a real Node
// worker and a real browser. Only the conversation itself is a fixture.
// MIRA_SERVER_URL=... [MIRA_TEST_BINARY=...] node tests/file_links_browser.mjs [playwright-module]
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { spawn, execFileSync } from "node:child_process";

const { chromium } = await import(process.argv[2] ?? "playwright");
const origin = (process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787").replace(/\/$/, "");
const password = process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password";
const root = await fs.realpath(await fs.mkdtemp(path.join(os.tmpdir(), "mira-file-links-")));
const binary = process.env.MIRA_TEST_BINARY || path.join(root, "mira");
if (!process.env.MIRA_TEST_BINARY) execFileSync("go", ["build", "-o", binary, "./cmd/mira"], { cwd: new URL("../node", import.meta.url) });

const home = path.join(root, "home"), work = path.join(root, "work");
for (const directory of ["home/notes", "work/src", "work/docs", "work/site/assets", "work/my docs"]) await fs.mkdir(path.join(root, directory), { recursive: true });
await fs.writeFile(path.join(home, "notes/todo.md"), "# Home note\n\nfrom the home directory\n");
await fs.writeFile(path.join(work, "src/app.js"), Array.from({ length: 30 }, (_, index) => `const line${index + 1} = ${index + 1};`).join("\n"));
await fs.writeFile(path.join(work, "README.md"), "# Project readme\n");
await fs.writeFile(path.join(work, "docs/guide.md"), "# Guide\n\nguide body\n");
await fs.writeFile(path.join(work, "my docs/a b.txt"), "spaced name\n");
await fs.writeFile(path.join(work, "report.html"), '<!doctype html><title>Report</title><h1 id="report">Report page</h1>');
await fs.writeFile(path.join(work, "site/index.html"), '<!doctype html><link rel="stylesheet" href="/assets/style.css"><h1>Site home</h1><script type="module" src="/assets/main.js"></script>');
await fs.writeFile(path.join(work, "site/assets/style.css"), "h1 { color: rgb(0, 120, 212) }");
await fs.writeFile(path.join(work, "site/assets/main.js"), "document.body.dataset.result = 'site script ran';");

const login = await fetch(`${origin}/v1/admin/login`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ username: "admin", password }) });
assert.equal(login.status, 200, "administrator login");
const auth = await login.json(), cookie = login.headers.get("set-cookie").split(";")[0];
const api = async (route, method = "GET", body) => {
  const response = await fetch(origin + route, { method, headers: { cookie, "x-mira-csrf": auth.csrfToken, "Content-Type": "application/json" }, ...(body ? { body: JSON.stringify(body) } : {}) });
  return { status: response.status, body: await response.json().catch(() => ({})) };
};
const nodeKey = `file-links-${process.pid}`;
let logs = "";
const worker = spawn(binary, ["node-worker"], { stdio: ["ignore", "pipe", "pipe"], env: { ...process.env, HOME: home, USERPROFILE: home,
  MIRA_SERVER_URL: origin, MIRA_NODE_KEY: nodeKey, MIRA_IDENTITY_FILE: path.join(root, "identity.json"), MIRA_NODE_ALLOWED_ROOTS: JSON.stringify([root]),
  MIRA_NODE_HEARTBEAT_SECONDS: "1", APP_SERVER_AUTO_START: "false", CODEX_HOME: path.join(root, "codex-home"), MIRA_NODE_TOKEN: "", CONTROL_SERVER_TOKEN: "" } });
for (const stream of [worker.stdout, worker.stderr]) stream.on("data", chunk => { logs += chunk; });
const until = async (probe, label, timeout = 30_000) => {
  for (const end = Date.now() + timeout; Date.now() < end; await new Promise(resolve => setTimeout(resolve, 100))) {
    const value = await probe();
    if (value) return value;
  }
  throw new Error(`${label} timed out\n${logs.slice(-2000)}`);
};

const thread = "40000000-0000-4000-8000-000000000001";
const message = [
  "改动在 src/app.js:12 和 `README.md`，笔记见 ~/notes/todo.md。",
  "[指南](docs/guide.md) · [尚未生成](docs/later.md) · [报告](report.html) · [站点目录](site/)",
  "目录 `src/` 以及带空格的 `my docs/a b.txt`。",
  "这些不是文件：and/or、TCP/IP、`agent.threadId`、`/v1/files/meta`、https://example.com/a/b.html 和 src/absent.js。",
  "```js\nimport app from \"src/app.js\";\n```",
].join("\n\n");
let browser, nodeId;
try {
  const pending = await until(async () => (await api("/v1/admin/enrollments?status=pending")).body.data?.find(item => item.nodeKey === nodeKey), "Node enrollment");
  const approved = await api(`/v1/admin/enrollments/${pending.enrollmentId}/approve`, "POST", {});
  assert.equal(approved.status, 200);
  nodeId = approved.body.nodeId;
  await until(async () => (await api(`/v1/nodes/${nodeId}`)).body.channelStatus?.connected, "Node connection");

  // The Server contract the page relies on, before any rendering.
  const probed = await api("/v1/files/probe", "POST", { nodeId, cwd: work, paths: ["~/notes/todo.md", "src/app.js", "src/", "nope.txt", "/etc/hostname"] });
  assert.equal(probed.status, 200, JSON.stringify(probed.body));
  assert.deepEqual(probed.body.entries.map(entry => [entry.exists, entry.type ?? entry.reason ?? null, entry.path ?? null]), [
    [true, "file", path.join(home, "notes/todo.md")], [true, "file", path.join(work, "src/app.js")], [true, "directory", path.join(work, "src")],
    [false, null, path.join(work, "nope.txt")], [false, "forbidden", "/etc/hostname"],
  ]);
  assert.equal(probed.body.home, home);
  assert.equal((await fetch(`${origin}/v1/files/probe`, { method: "POST", headers: { cookie, "Content-Type": "application/json" }, body: "{}" })).status, 403, "probe requires CSRF proof");
  assert.equal((await api("/v1/files/probe", "POST", { nodeId, paths: [] })).status, 400);
  assert.equal((await api("/v1/files/reveal", "POST", { nodeId, path: path.join(root, "..", "outside") })).status >= 400, true);

  const item = (seq, kind, body, sourceContext) => ({ key: `history-${seq}-${kind}`, kind, title: kind === "user" ? "你" : "Codex", markdown: true, body, sourceItemSeq: seq, turnId: `turn-${seq > 2 ? 2 : 1}`, sourceContext });
  const trace = [
    item(1, "user", "看看这些文件", { nodeId, cwd: work }),
    item(2, "assistant", message, { nodeId, cwd: work }),
    item(3, "user", "没有记录来源的一轮", { cwd: work }),
    item(4, "assistant", "[指南](docs/guide.md) 和 ~/notes/todo.md", { cwd: work }),
  ];
  const row = { threadId: thread, title: "File links", cwd: work, generation: 1, itemCount: trace.length, runtimeNodeId: nodeId, updatedAt: new Date().toISOString(), activity: { state: "idle", generation: 1, itemCount: 1 } };
  const requests = { probe: [], reveal: [] };
  // A tab may close while one of its requests is in flight; that is not a failure.
  const answer = (route, response) => route.fulfill(response).catch(() => {});
  const install = async context => {
    await context.route("**/v1/codex/threads?*", route => answer(route, { json: { data: [row] } }));
    await context.route(/\/v1\/codex\/threads\/[^/?]+(\/[a-z]+)?\?/, route => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith("/costs")) return answer(route, { json: { threadId: thread, generation: 1, itemCount: trace.length, turnCostEstimates: {} } });
      if (url.pathname.endsWith("/timeline")) return answer(route, { json: { engine: "codex", rootThreadId: thread, partial: false, truncated: false, lanes: [] } });
      if (url.pathname.endsWith("/transcript")) return answer(route, { json: { generation: 1, itemCount: trace.length, storeVersion: 1, outline: true, nextCursor: null,
        trace: url.searchParams.get("after") === null ? trace : [], ...(url.searchParams.get("after") === null ? {} : { after: Number(url.searchParams.get("after")) }) } });
      return answer(route, { json: row });
    });
    // CI Nodes have no desktop: report one, and observe the request instead of
    // starting a file manager. The console reads the list and the single Node.
    await context.route(/\/v1\/nodes(\/[0-9a-f-]+)?(\?[^/]*)?$/, async route => {
      try {
        if (route.request().method() !== "GET") return await route.continue();
        const response = await route.fetch(), body = await response.json();
        for (const node of body.data ?? [body]) if (node.nodeId === nodeId) node.capabilities.desktopRevealV1 = true;
        await route.fulfill({ response, json: body });
      } catch { /* the tab that asked closed while its request was in flight */ }
    });
    await context.route("**/v1/files/reveal", route => { requests.reveal.push(route.request().postDataJSON()); return answer(route, { json: { revealed: true } }); });
    context.on("request", request => { if (request.url().endsWith("/v1/files/probe")) requests.probe.push(request.postDataJSON()); });
  };
  const signIn = async page => {
    await page.goto(origin);
    await page.locator("#password").fill(password);
    await page.locator('#loginForm button[type="submit"]').click();
    await page.locator("#dashboardView:not(.hidden)").waitFor();
  };
  const linkTo = suffix => `#conversationTrace a.node-file-link[data-node-file-path$="${suffix}"]`;

  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, permissions: ["clipboard-read", "clipboard-write"] });
  await install(context);
  const page = await context.newPage(), errors = [];
  page.setDefaultTimeout(20_000);
  page.on("pageerror", error => errors.push(error.message));
  await signIn(page);
  await page.goto(`${origin}/?thread=${thread}`);
  await page.locator(linkTo("src/app.js")).first().waitFor();

  // Recognition: the Node decides, so "~", relative paths and bare prose work
  // and look-alikes stay text.
  const links = await page.evaluate(() => [...document.querySelectorAll('[data-trace-key="history-2-assistant"] [data-node-file-path]')].map(link => ({
    text: link.textContent, path: link.dataset.nodeFilePath, type: link.dataset.nodeFileType ?? null, line: link.dataset.nodeFileLine ?? null,
    missing: link.dataset.nodeFileMissing ?? null, target: link.getAttribute("target"), site: new URL(link.href).searchParams.get("view"),
  })));
  const expected = [
    ["src/app.js:12", path.join(work, "src/app.js"), "file", "12"], ["README.md", path.join(work, "README.md"), "file", null],
    ["~/notes/todo.md", path.join(home, "notes/todo.md"), "file", null], ["指南", path.join(work, "docs/guide.md"), "file", null],
    ["尚未生成", path.join(work, "docs/later.md"), null, null], ["报告", path.join(work, "report.html"), "file", null],
    ["站点目录", path.join(work, "site"), "directory", null], ["src/", path.join(work, "src"), "directory", null],
    ["my docs/a b.txt", path.join(work, "my docs/a b.txt"), "file", null],
  ];
  assert.deepEqual(links.map(link => [link.text, link.path, link.type, link.line]), expected);
  assert.deepEqual(links.filter(link => link.missing).map(link => link.text), ["尚未生成"], "only a Markdown link keeps a missing target, marked as such");
  assert.equal(links.find(link => link.text === "报告").site, "site", "an HTML file opens as a website by default");
  assert.equal(links.find(link => link.text === "站点目录").site, null, "a directory opens in the workspace by default");
  assert.ok(links.filter(link => !link.missing).every(link => link.target === "_blank"), "references are real links");
  const external = page.locator('[data-trace-key="history-2-assistant"] a[href="https://example.com/a/b.html"]');
  assert.equal(await external.getAttribute("target"), "_blank");
  assert.equal(await external.getAttribute("data-node-file-path"), null);
  assert.equal(await page.locator('[data-trace-key="history-2-assistant"] pre a').count(), 0, "fenced code is never linked");
  // One request carries the whole message; the prose look-alikes are not asked about.
  const asked = requests.probe.filter(request => request.nodeId === nodeId).flatMap(request => request.paths);
  assert.equal(requests.probe.filter(request => request.nodeId === nodeId).length, 1, "one batched probe per message");
  assert.ok(!asked.includes("and/or") && !asked.includes("TCP/IP"), `prose look-alikes are not probed: ${asked}`);
  assert.ok(asked.includes("agent.threadId") && asked.includes("/v1/files/meta") && asked.includes("src/absent.js"), "plausible candidates are checked, then left as text");

  // Without a recorded Node nothing can be confirmed: a Markdown link keeps
  // its written target and asks which Node; "~" stays text.
  const unknown = await page.evaluate(() => [...document.querySelectorAll('[data-trace-key="history-4-assistant"] [data-node-file-path]')].map(link => [link.textContent, link.dataset.nodeFileType ?? null]));
  assert.deepEqual(unknown, [["指南", null]]);

  // A plain click on a source reference reads the file beside the conversation.
  await page.locator("#conversationInput").fill("草稿在阅读文件时保留");
  await page.locator(linkTo("src/app.js")).first().click();
  const dock = page.locator("#nodeFileDialog.file-dock");
  await dock.locator('.file-source-line.selected[data-line="12"]').waitFor();
  assert.equal(await page.evaluate(() => document.querySelector("#nodeFileDialog").open && !document.querySelector("#nodeFileDialog").matches(":modal")), true, "the reader does not block the conversation");
  assert.deepEqual(await dock.locator("#nodeFileActions .file-action").allTextContents(), ["文件工作区", "资源管理器", "复制路径", "下载"]);
  assert.ok((await dock.locator("#nodeFileMeta").textContent()).endsWith(path.join(work, "src/app.js")));
  await dock.locator('#nodeFileActions [data-file-action="copy"]').click();
  assert.equal(await page.evaluate(() => navigator.clipboard.readText()), path.join(work, "src/app.js"));
  await dock.locator('#nodeFileActions [data-file-action="reveal"]').click();
  await until(() => requests.reveal.length === 1, "reveal request");
  assert.deepEqual(requests.reveal[0], { nodeId, path: path.join(work, "src/app.js") });
  await page.locator("#nodeFileClose").click();
  assert.equal(await page.locator("#conversationInput").inputValue(), "草稿在阅读文件时保留");

  // The same actions from the reference itself: secondary click, keyboard.
  const readme = page.locator(linkTo("README.md"));
  await readme.click({ button: "right" });
  const menu = page.locator(".file-link-menu:popover-open");
  await menu.waitFor();
  assert.deepEqual(await menu.locator("[role=menuitem]").evaluateAll(items => items.map(item => item.dataset.fileAction)), ["preview", "workspace", "reveal", "copy", "download"]);
  assert.equal(await menu.locator(".primary-action").getAttribute("data-file-action"), "preview");
  await page.keyboard.press("ArrowDown");
  assert.equal(await page.evaluate(() => document.activeElement.dataset.fileAction), "workspace");
  const sidebarOpen = () => page.evaluate(() => document.querySelector("#agentThreadDrawer").classList.contains("open"));
  assert.equal(await sidebarOpen(), true);
  await page.keyboard.press("Escape");
  await menu.waitFor({ state: "hidden" });
  assert.equal(await sidebarOpen(), true, "Escape closes only the menu");
  assert.equal(await page.evaluate(() => document.activeElement.dataset.nodeFilePath?.endsWith("README.md")), true, "focus returns to the reference");
  await readme.click({ button: "right" });
  await menu.locator('[data-file-action="preview"]').click();
  await dock.locator("#nodeFileReader h1").filter({ hasText: "Project readme" }).waitFor();
  await page.locator("#nodeFileClose").click();
  await page.locator(linkTo("site")).click({ button: "right" });
  assert.deepEqual(await menu.locator("[role=menuitem]").evaluateAll(items => items.map(item => item.dataset.fileAction)), ["preview", "workspace", "site", "reveal", "copy"]);
  await page.mouse.click(5, 5);
  await menu.waitFor({ state: "hidden" });
  await readme.click({ modifiers: ["Alt"] });
  await until(() => requests.reveal.length === 2, "Alt+click reveal");
  assert.equal(requests.reveal[1].path, path.join(work, "README.md"));
  assert.equal(await dock.count(), 0, "revealing does not open the reader");

  // A missing Markdown target is rechecked on click and becomes a link once the file exists.
  const later = page.locator('#conversationTrace a[data-node-file-path$="docs/later.md"]');
  await later.click();
  await page.locator("#toast").filter({ hasText: "仍然没有找到" }).waitFor();
  await fs.writeFile(path.join(work, "docs/later.md"), "# Later\n");
  await later.click();
  await page.waitForFunction(() => document.querySelector('a[data-node-file-path$="docs/later.md"]')?.dataset.nodeFileType === "file");
  await later.click();
  await dock.locator("#nodeFileReader h1").filter({ hasText: "Later" }).waitFor();
  await page.locator("#nodeFileClose").click();

  // A reference without a recorded Node asks which Node, as before.
  await page.locator('[data-trace-key="history-4-assistant"] a[data-node-file-path]').click();
  await dock.locator("#nodeFileReader").filter({ hasText: "请选择" }).waitFor();
  assert.equal(await dock.locator("#nodeFileActions").isHidden(), true);
  await page.locator("#nodeFileClose").click();

  // Directory: a new tab with the workspace, opened by the link itself.
  const [workspace] = await Promise.all([context.waitForEvent("page"), page.locator(linkTo("src")).click()]);
  await workspace.locator(".file-tree-row button", { hasText: "app.js" }).click();
  await workspace.locator('.file-tab-pane:not([hidden]) .file-source-line[data-line="30"]').waitFor();
  assert.equal(await workspace.evaluate(() => window.opener), null);
  assert.equal(await workspace.locator("#fileName").textContent(), "src");
  assert.equal(await workspace.locator(".file-tree-row button.selected").textContent(), "app.js");
  assert.deepEqual(await workspace.locator("#fileModes button").allTextContents(), ["文件", "网站"]);
  await workspace.locator(".file-tree-head button", { hasText: "↑" }).click();
  await workspace.locator(".file-tree-row summary", { hasText: /^docs$/ }).waitFor();
  assert.equal(await workspace.locator(".file-tab.selected").textContent(), "app.js×", "changing the tree root keeps open files");
  // "返回对话" closes the tab it was opened in rather than loading a second console.
  await Promise.all([workspace.waitForEvent("close"), workspace.locator("#fileReturn").click()]);

  // HTML: a new tab that becomes the isolated website.
  const [site] = await Promise.all([context.waitForEvent("page"), page.locator(linkTo("report.html")).click()]);
  await site.waitForURL(url => url.hostname.endsWith(".preview.localhost"));
  await site.locator("#report").waitFor();
  assert.equal(await site.evaluate(() => window.opener), null);
  await site.close();
  assert.deepEqual(errors, [], `conversation page errors: ${errors}`);
  await context.close();

  // Phone: nothing depends on a popup, and nothing hides under the status bar.
  const phone = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
  await install(phone);
  const mobile = await phone.newPage(), mobileErrors = [];
  mobile.setDefaultTimeout(20_000);
  mobile.on("pageerror", error => mobileErrors.push(error.message));
  await signIn(mobile);
  const files = extra => `${origin}/files.html?${new URLSearchParams({ nodeId, ...extra })}`;
  // A directory with index.html starts in the same tab.
  await mobile.goto(files({ path: path.join(work, "site"), view: "site" }));
  await mobile.waitForURL(url => url.hostname.endsWith(".preview.localhost"));
  await mobile.waitForFunction(() => document.body.dataset.result === "site script ran");
  assert.equal(phone.pages().length, 1, "the website opened without another window");
  // A directory without index.html explains itself and offers its pages.
  await mobile.goto(files({ path: work, view: "site" }));
  await mobile.locator("#siteState.error").filter({ hasText: "没有找到入口文件 index.html" }).waitFor();
  assert.deepEqual(await mobile.locator("#siteSuggestions button").allTextContents(), ["report.html"]);
  assert.ok(new URL(mobile.url()).pathname === "/files.html", "a failed start stays on the launcher");
  await mobile.locator("#siteSuggestions button").tap();
  await mobile.waitForURL(url => url.hostname.endsWith(".preview.localhost"));
  await mobile.locator("#report").waitFor();
  // Back returns to the launcher, which does not start the website again.
  await mobile.goBack();
  await mobile.waitForURL(url => url.pathname === "/files.html");
  await mobile.locator("#siteStop:not([hidden])").waitFor();
  assert.equal(new URL(mobile.url()).searchParams.get("noauto"), "1");
  await mobile.locator("#siteStop").tap();
  await mobile.locator("#siteState").filter({ hasText: "预览会话已关闭" }).waitFor();
  // Workspace on a narrow screen: the tree is a drawer that closes on selection.
  await mobile.goto(files({ path: work }));
  await mobile.locator("#fileTreeToggle").waitFor();
  await mobile.locator(".file-tree-row").first().waitFor({ state: "attached" });
  const offscreen = () => mobile.evaluate(() => document.querySelector(".file-tree").getBoundingClientRect().right <= 0);
  assert.equal(await offscreen(), true);
  await mobile.locator("#fileTreeToggle").tap();
  await mobile.locator(".file-tree-row button", { hasText: "README.md" }).tap();
  await mobile.locator(".file-tab-pane:not([hidden]) h1").filter({ hasText: "Project readme" }).waitFor();
  await mobile.waitForFunction(() => document.querySelector(".file-tree").getBoundingClientRect().right <= 0);
  const overflow = await mobile.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  assert.ok(overflow <= 0, `the page does not scroll sideways: ${overflow}`);
  // The reader over the conversation starts below the Android status bar.
  await mobile.goto(`${origin}/?thread=${thread}`);
  await mobile.locator(linkTo("src/app.js")).first().waitFor();
  await mobile.evaluate(() => { document.documentElement.classList.add("android-app"); document.documentElement.style.setProperty("--android-status-bar-height", "36px"); });
  await mobile.locator(linkTo("README.md")).tap();
  await mobile.locator("#nodeFileDialog.file-dock #nodeFileReader h1").waitFor();
  const head = await mobile.locator("#nodeFileDialog .node-file-dialog-head").boundingBox();
  assert.ok(head.y >= 36, `the reader header clears the status bar: ${head.y}`);
  await mobile.locator("#nodeFileClose").tap();
  // A held press opens the action menu; the release does not dismiss it.
  const target = await mobile.locator(linkTo("README.md")).boundingBox();
  const client = await phone.newCDPSession(mobile);
  const touch = (type, points) => client.send("Input.dispatchTouchEvent", { type, touchPoints: points });
  await touch("touchStart", [{ x: target.x + target.width / 2, y: target.y + target.height / 2 }]);
  await mobile.locator(".file-link-menu:popover-open").waitFor();
  await touch("touchEnd", []);
  await mobile.waitForTimeout(400);
  assert.equal(await mobile.locator(".file-link-menu:popover-open").count(), 1, "the menu survives lifting the finger");
  assert.equal(await mobile.locator("#nodeFileDialog[open]").count(), 0, "the held press does not also open the reader");
  assert.deepEqual(mobileErrors, [], `mobile page errors: ${mobileErrors}`);
  console.log("File references: Node-confirmed links, actions, workspace, website start and phone layout passed");
} finally {
  await browser?.close();
  if (worker.exitCode === null) { worker.kill(); await new Promise(resolve => worker.once("exit", resolve)); }
  await fs.rm(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 });
}

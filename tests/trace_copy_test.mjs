import assert from "node:assert/strict";
import fs from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

// Exercise the actual browser helpers without starting a Server or requesting
// clipboard permissions on the test host. Rendering/wiring is checked below too.
const app = await fs.readFile(new URL("../server/public/app.js", import.meta.url), "utf8");
const start = app.indexOf("function createTraceCopyButton(");
const end = app.indexOf("function traceNearBottom(", start);
assert.ok(start >= 0 && end > start);

function fixture(clipboard) {
  const notices = [];
  const context = vm.createContext({
    navigator: { clipboard },
    toast: (message) => notices.push(message),
    element: (tag, className, textContent) => ({
      tag, className, textContent, disabled: false,
      attributes: {},
      listeners: {},
      innerHTML: "",
      setAttribute(name, value) { this.attributes[name] = value; },
      addEventListener(name, listener) { this.listeners[name] = listener; },
      click() { return this.listeners.click(); },
    }),
    traceUsesMarkdown: (kind) => ["user", "assistant", "reasoning"].includes(kind),
    marked: { parse: (text) => `<p>${text}</p>` },
    DOMPurify: { sanitize: (html) => html },
    decorateTraceFileReferences: () => {},
    traceStreamRenders: new WeakMap(),
    cancelAnimationFrame: () => {},
  });
  vm.runInContext(app.slice(start, end), context);
  function card(kind, text) {
    const body = { classList: { toggle() {} }, querySelectorAll: () => [] };
    const card = {
      dataset: { traceKind: kind },
      classList: { toggle() {} },
      querySelector: (selector) => selector === ".trace-body" ? body : button,
    };
    const button = context.createTraceCopyButton(card);
    const update = (text) => context.setTraceBody(card, text);
    update(text);
    return { body, button, update };
  }
  return { card, notices };
}

test("copy keeps raw Markdown, code blocks, paths and Unicode without rendering chrome", async () => {
  const copied = [];
  const { card, notices } = fixture({ writeText: async (text) => copied.push(text) });
  const raw = "## 中文 😀\n\n```sh\nprintf '你好'\n```\n\n[文件](/tmp/report.md)\n";
  const { button, body } = card("assistant", raw);
  body.textContent = "Codex 完成 复制 文件 ↗";
  await button.click();
  assert.deepEqual(copied, [raw]);
  assert.deepEqual(notices, ["消息原文已复制"]);
  assert.equal(button.type, "button");
  assert.equal(button.attributes["aria-label"], "复制这条消息的原文");
  assert.match(button.innerHTML, /<svg/);
  assert.equal(button.disabled, false);
});

test("copy uses the latest stream text and remains scoped to its own message", async () => {
  const copied = [];
  const { card } = fixture({ writeText: async (text) => copied.push(text) });
  const user = card("user", "用户消息");
  const assistant = card("assistant", "开始");
  await assistant.button.click();
  assistant.update("开始\n\n**完成**");
  await assistant.button.click();
  await user.button.click();
  const tool = card("tool", "stdout\nexit code: 0");
  await tool.button.click();
  assert.deepEqual(copied, ["开始", "开始\n\n**完成**", "用户消息", "stdout\nexit code: 0"]);
});

test("empty cards hide copy and show it as soon as content arrives", async () => {
  const copied = [];
  const { card, notices } = fixture({ writeText: async (text) => copied.push(text) });
  const message = card("assistant", "");
  assert.equal(message.button.hidden, true);
  await message.button.click();
  assert.deepEqual(copied, []);
  assert.deepEqual(notices, []);
  message.update("内容");
  assert.equal(message.button.hidden, false);
  message.update(undefined);
  assert.equal(message.button.hidden, true);
});

test("permission failures allow retry and never report success", async () => {
  let denied = true;
  const { card, notices } = fixture({ writeText: async () => {
    if (denied) throw new Error("NotAllowedError");
  } });
  const { button } = card("assistant", "正文");
  await button.click();
  assert.deepEqual(notices, ["浏览器未允许复制，请选中消息手动复制"]);
  assert.equal(button.disabled, false);
  denied = false;
  await button.click();
  assert.equal(notices.at(-1), "消息原文已复制");
});

test("unavailable clipboard API gives a manual-copy hint", async () => {
  const { card, notices } = fixture(undefined);
  const { button } = card("user", "正文");
  await button.click();
  assert.deepEqual(notices, ["浏览器未允许复制，请选中消息手动复制"]);
  assert.equal(button.disabled, false);
});

test("pending clipboard writes cannot be duplicated by repeated clicks", async () => {
  let resolve;
  let writes = 0;
  const { card } = fixture({ writeText: () => {
    writes += 1;
    return new Promise((done) => { resolve = done; });
  } });
  const { button } = card("assistant", "正文");
  const pending = button.click();
  assert.equal(button.disabled, true);
  await button.click();
  assert.equal(writes, 1);
  resolve();
  await pending;
  assert.equal(button.disabled, false);
});

test("shared history and live renderer installs copy buttons before rendering the body", () => {
  const renderer = app.slice(app.indexOf("function upsertTrace("), app.indexOf("function nodeFileMimeType("));
  assert.ok(renderer.includes('createTraceCopyButton(card)'));
  assert.ok(renderer.indexOf('createTraceCopyButton(card)') < renderer.indexOf('setTraceBody(card, body, kind)'));
  assert.ok(renderer.includes('head.append(element("span", "trace-kind", title), actions)'));
});

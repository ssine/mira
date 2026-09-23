import assert from "node:assert/strict";
import fs from "node:fs/promises";
import vm from "node:vm";
import test from "node:test";

const workerSource = await fs.readFile(new URL("../server/public/service-worker.js", import.meta.url), "utf8");
const pwaSource = (await fs.readFile(new URL("../server/public/pwa.js", import.meta.url), "utf8")).replaceAll("export function", "function");
const thread = "00000000-0000-4000-8000-0000000000a1";

test("push wakes the worker without a page; repeated delivery replaces the same notification", async () => {
  const handlers = {}, shown = new Map(), opened = [];
  let focused = 0, closed = 0, windows = [];
  vm.runInNewContext(workerSource, {
    URL, self: { location: { origin: "https://mira.example.test" },
      addEventListener: (type, callback) => { handlers[type] = callback; },
      registration: { showNotification: async (title, options) => shown.set(options.tag, { title, ...options }) },
      clients: { matchAll: async () => windows, openWindow: async (url) => opened.push(url) },
    },
  });
  const dispatch = async (type, values) => {
    let done;
    handlers[type]({ ...values, waitUntil: (promise) => { done = promise; } });
    await done;
  };
  const url = `/?thread=${thread}`;
  for (let i = 0; i < 2; i++) await dispatch("push", { data: { json: () => ({ body: "我的对话", url, tag: "same-turn" }) } });
  assert.equal(shown.size, 1);
  const notification = shown.get("mira-completed-same-turn");
  assert.equal(notification.title, "对话已完成");
  assert.equal(notification.body, "我的对话");
  await dispatch("notificationclick", { notification: { ...notification, close: () => closed++ } });
  assert.equal(closed, 1);
  assert.deepEqual(opened, [`https://mira.example.test${url}`]);
  windows = [{ url: opened[0], focus: async () => focused++ }];
  await dispatch("notificationclick", { notification: { ...notification, close: () => {} } });
  assert.equal(focused, 1);
  assert.equal(opened.length, 1);
  windows = [{ url: "https://mira.example.test/?view=agent", navigate: () => assert.fail("must preserve another conversation's draft") }];
  await dispatch("notificationclick", { notification: { ...notification, close: () => {} } });
  assert.equal(opened.length, 2);
  for (const unsafe of ["https://evil.test/", "//evil.test/", "/?thread=invalid", `/?thread=${thread}&next=evil`]) {
    await dispatch("push", { data: { json: () => ({ url: unsafe, tag: "bad" }) } });
    await dispatch("notificationclick", { notification: { data: { url: unsafe }, close: () => {} } });
  }
  await dispatch("push", { data: { json: () => { throw new Error("bad JSON"); } } });
  assert.equal(shown.size, 1);
  assert.equal(opened.length, 2);

});

function pageFixture() {
  const handlers = {}, requests = [], notices = [], storage = new Map();
  let subscribed = 0, unsubscribed = 0, permissionRequests = 0, subscription = null, permission = "granted", rejectSave = false;
  const button = { classList: { remove() {} }, getAttribute() { return null; }, setAttribute() {}, addEventListener: (type, callback) => { handlers[type] = callback; } };
  const worker = { pushManager: {
    getSubscription: async () => subscription,
    subscribe: async () => {
      subscribed++;
      subscription = { endpoint: "https://push.example.test/device", toJSON: () => ({ endpoint: "https://push.example.test/device" }),
        unsubscribe: async () => { unsubscribed++; subscription = null; return true; } };
      return subscription;
    },
  }, getNotifications: async () => [] };
  const api = async (path, options) => {
    requests.push({ path, ...options });
    if (path.endsWith("config")) return { publicKey: "AQ" };
    if (rejectSave && options.method === "POST") throw new Error("offline");
    return { enabled: options.method === "POST" };
  };
  const context = vm.createContext({
    window: { matchMedia: () => ({ matches: true }), isSecureContext: true, PushManager: {}, Notification: {}, addEventListener() {} },
    navigator: { standalone: true, userAgent: "test", serviceWorker: { ready: Promise.resolve(worker), getRegistration: async () => worker } },
    Notification: { get permission() { return permission; }, requestPermission: async () => { permissionRequests++; return permission; } },
    document: { querySelectorAll: () => [button] },
    localStorage: { getItem: (key) => storage.get(key), setItem: (key, value) => storage.set(key, value) },
    setTimeout, clearTimeout, atob, Uint8Array, api, toast: (message) => notices.push(message),
  });
  vm.runInContext(pwaSource + "\nglobalThis.controls = createCompletionNotifications(api, toast);", context);
  return { context, button, requests, notices, handlers, storage,
    stats: () => ({ subscribed, unsubscribed, permissionRequests }),
    permission: (value) => { permission = value; }, fail: () => { rejectSave = true; },
    login: async () => { context.controls.setAuthenticated(true); await new Promise(setImmediate); },
  };
}

test("device notifications require a gesture, persist consent, disable and clear on logout", async () => {
  const page = pageFixture();
  await page.login();
  assert.equal(page.stats().permissionRequests, 0);
  assert.equal(page.button.textContent, "开启完成通知");
  await page.handlers.click();
  assert.equal(page.button.textContent, "关闭完成通知");
  assert.equal(page.stats().subscribed, 1);
  assert.equal(page.requests.at(-1).method, "POST");
  await page.handlers.click();
  assert.equal(page.button.textContent, "开启完成通知");
  assert.equal(page.requests.at(-1).method, "DELETE");
  assert.equal(page.stats().unsubscribed, 1);
  await page.handlers.click();
  await page.context.controls.logout();
  assert.equal(page.stats().unsubscribed, 2);
  assert.equal(page.storage.get("mira.push.enabled"), "false");
});

test("denied permission and failed server registration never report notifications enabled", async () => {
  const page = pageFixture();
  await page.login();
  page.permission("denied");
  await page.handlers.click();
  assert.equal(page.stats().subscribed, 0);
  assert.equal(page.requests.length, 0);
  assert.match(page.notices.at(-1), /通知已被禁止/);
  page.permission("granted"); page.fail();
  await page.handlers.click();
  assert.equal(page.button.textContent, "开启完成通知");
  assert.equal(page.stats().unsubscribed, 1);
  assert.match(page.notices.at(-1), /通知设置失败/);
});

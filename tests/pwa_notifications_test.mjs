import assert from "node:assert/strict";
import fs from "node:fs/promises";
import vm from "node:vm";
import test from "node:test";

const workerSource = await fs.readFile(new URL("../server/public/service-worker.js", import.meta.url), "utf8");
const androidSource = (await fs.readFile(new URL("../server/public/android.js", import.meta.url), "utf8")).replaceAll("export function", "function").replaceAll("export const", "const");
const pwaSource = androidSource + "\n" + (await fs.readFile(new URL("../server/public/pwa.js", import.meta.url), "utf8")).replace(/^import .*;\n/m, "").replaceAll("export function", "function");
const thread = "00000000-0000-4000-8000-0000000000a1";
const otherThread = "00000000-0000-4000-8000-0000000000b2";

test("notification clicks prefer the installed PWA and use its current SPA route", async () => {
  const handlers = {}, opened = [];
  let windows = [];
  vm.runInNewContext(workerSource, { URL, MessageChannel, setTimeout, clearTimeout,
    self: { location: { origin: "https://mira.example.test" },
      addEventListener: (type, callback) => { handlers[type] = callback; },
      clients: { matchAll: async () => windows, openWindow: async url => opened.push(url) },
    },
  });
  const click = async () => {
    let done;
    handlers.notificationclick({ notification: { data: { url: `/?thread=${thread}` }, close() {} }, waitUntil: promise => { done = promise; } });
    await done;
  };
  const page = (standalone, route, accepted = true) => {
    let receive;
    const location = { origin: "https://mira.example.test", href: `https://mira.example.test${route}` };
    const navigation = [], focus = [];
    const context = vm.createContext({ URL, location,
      window: { matchMedia: () => ({ matches: standalone }) },
      navigator: { serviceWorker: { addEventListener: (_, handler) => { receive = handler; } } },
      open: target => { if (accepted) { navigation.push(target); location.href = new URL(target, location.origin).href; } return accepted; },
    });
    vm.runInContext(pwaSource + "\ninstallNotificationNavigation(open);", context);
    return { location, navigation, focus,
      client: { url: "https://mira.example.test/?launch=pwa", focus: async () => focus.push(true),
        postMessage: (data, ports) => receive({ data, ports, source: { scriptURL: "https://mira.example.test/service-worker.js" } }),
      },
    };
  };
  const tab = page(false, `/?view=agent&thread=${thread}`);
  const pwa = page(true, `/?thread=${otherThread}`);
  windows = [tab.client, pwa.client];
  await click();
  assert.deepEqual(pwa.navigation, [`/?thread=${thread}`]);
  assert.equal(pwa.focus.length, 1);
  assert.equal(tab.focus.length, 0, "a browser tab must not win over the installed app");
  assert.equal(opened.length, 0);
  await click();
  assert.equal(pwa.navigation.length, 1, "the live SPA route already shows this conversation");
  assert.equal(pwa.focus.length, 2);

  const busy = page(true, `/?thread=${otherThread}`, false);
  windows = [busy.client, tab.client];
  await click();
  assert.equal(busy.navigation.length, 0);
  assert.equal(tab.focus.length, 1, "busy PWA falls back without interrupting its submission");

  tab.client.url = `https://mira.example.test/?thread=${thread}`;
  tab.location.href = `https://mira.example.test/?thread=${otherThread}`;
  windows = [tab.client];
  await click();
  assert.equal(tab.focus.length, 1, "a stale WindowClient URL must not reopen the wrong conversation");
  assert.equal(opened.length, 1);
});

test("page navigation accepts only unexpired messages from its own service worker", () => {
  let receive;
  const opened = [], replies = [];
  const context = vm.createContext({ URL, location: { origin: "https://mira.example.test", href: "https://mira.example.test/" },
    window: { matchMedia: () => ({ matches: true }) },
    navigator: { serviceWorker: { addEventListener: (_, handler) => { receive = handler; } } },
    open: url => { opened.push(url); return true; },
  });
  vm.runInContext(pwaSource + "\ninstallNotificationNavigation(open);", context);
  const event = { data: { type: "mira:notification-open", url: `/?thread=${thread}`, expiresAt: Date.now() + 10_000 },
    ports: [{ postMessage: value => replies.push(value) }], source: { scriptURL: "https://evil.test/service-worker.js" } };
  receive(event);
  assert.equal(replies.length, 0);
  event.source.scriptURL = "https://mira.example.test/service-worker.js";
  event.data.expiresAt = 0; receive(event);
  assert.equal(replies.at(-1).accepted, false);
  event.data.expiresAt = Date.now() + 10_000;
  event.data.url = `https://evil.test/?thread=${thread}`; receive(event);
  assert.equal(replies.at(-1).accepted, false);
  event.data.url = `/?thread=${thread}`; receive(event);
  assert.equal(replies.at(-1).accepted, true);
  assert.deepEqual(opened, [`/?thread=${thread}`]);
});

test("app notification routing retains text and attachments even when draft storage fails", async () => {
  const app = await fs.readFile(new URL("../server/public/app.js", import.meta.url), "utf8");
  const drafts = (await fs.readFile(new URL("../server/public/composer-drafts.js", import.meta.url), "utf8")).replace("export class", "class");
  const openFunction = app.slice(app.indexOf("function openNotificationConversation("), app.indexOf("\nsyncThemeControl();", app.indexOf("function openNotificationConversation(")));
  const saveFunction = app.slice(app.indexOf("function saveComposerDraft("), app.indexOf("\nfunction nativeImageAttachment("));
  const navigated = [], input = { value: "unfinished text" }, file = { name: "draft.txt" };
  const key = `personal:thread:${otherThread}`;
  const context = vm.createContext({
    window: { history: { pushState: (_, __, url) => navigated.push(url) } },
    document: { querySelector: () => null }, $: () => input,
    agent: { attachments: [file], selectionEpoch: 0 }, csrfToken: "fixture", composerDraftKey: key,
    composerDraftLoading: false, composerDraftReadFailed: false, composerDraftRemoveKey: null, composerSaveRevision: 0,
    setComposerDraftStatus() {}, rememberAppRoute() {},
    restoreBrowserRoute: async () => { input.value = ""; context.agent.attachments = []; },
  });
  vm.runInContext(drafts + "\nglobalThis.composerDrafts = new ComposerDrafts(); composerDrafts.open = () => Promise.reject(new Error('storage blocked'));\n" + saveFunction + openFunction, context);
  assert.equal(context.openNotificationConversation(`/?thread=${thread}`), true);
  await new Promise(setImmediate);
  const saved = context.composerDrafts.pending.get(key);
  assert.equal(saved.text, "unfinished text");
  assert.equal(saved.files[0], file);
  assert.equal(input.value, "");
  for (const busy of ["sendPromise", "forkPromise", "threadActionPromise"]) {
    context.agent[busy] = Promise.resolve();
    assert.equal(context.openNotificationConversation(`/?thread=${otherThread}`), false);
    context.agent[busy] = null;
  }
  context.composerDraftLoading = true;
  assert.equal(context.openNotificationConversation(`/?thread=${otherThread}`), false);
  assert.equal(navigated.length, 1);
});

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

function androidFixture() {
  const events = {}, clicks = {}, requests = [], notices = [], nativeCalls = [];
  let enabled = false, permission = "granted", failSave = false;
  const button = { classList: { remove() {} }, getAttribute() { return null; }, setAttribute() {}, addEventListener: (name, handler) => clicks[name] = handler };
  const bridge = { postMessage(raw) {
    const request = JSON.parse(raw); nativeCalls.push(request);
    if (request.method === "setEnabled") enabled = request.enabled;
    queueMicrotask(() => bridge.onmessage({ data: JSON.stringify({ id: request.id, result: { nodeKey: "test-android", enabled, permission } }) }));
  } };
  const context = vm.createContext({ setTimeout, clearTimeout,
    window: { MiraAndroid: bridge, matchMedia: () => ({ matches: false }), addEventListener: (name, handler) => events[name] = handler },
    document: { querySelectorAll: () => [button], addEventListener() {} }, navigator: {},
    api: async (path, options) => { requests.push({ path, ...options }); if (failSave) throw new Error("offline"); return { enabled: options.method === "POST" }; },
    toast: value => notices.push(value),
  });
  vm.runInContext(pwaSource + "\nglobalThis.controls = createCompletionNotifications(api, toast);", context);
  return { context, clicks, button, requests, nativeCalls, events, notices,
    enabled: () => enabled, deny: () => permission = "denied", fail: () => failSave = true,
    login: async () => { context.controls.setAuthenticated(true); await new Promise(setImmediate); },
  };
}

test("Android opts in over native channel without browser PushManager and disables locally on network loss", async () => {
  const page = androidFixture(); await page.login();
  assert.equal(page.nativeCalls.some(call => call.method === "permission"), false);
  await page.clicks.click();
  assert.equal(page.enabled(), true);
  assert.equal(page.button.textContent, "关闭完成通知");
  assert.equal(page.requests.at(-1).path, "/v1/push/node-subscription");
  assert.deepEqual(JSON.parse(page.requests.at(-1).body), { nodeKey: "test-android" });
  page.fail(); await page.clicks.click();
  assert.equal(page.enabled(), false, "local delivery must stop even if DELETE cannot reach Server");
  assert.equal(page.button.textContent, "开启完成通知");
});

test("Android permission rejection, failed registration and logout never leave native delivery enabled", async () => {
  const denied = androidFixture(); await denied.login(); denied.deny(); await denied.clicks.click();
  assert.equal(denied.requests.length, 0); assert.equal(denied.enabled(), false);
  const offline = androidFixture(); await offline.login(); offline.fail(); await offline.clicks.click();
  assert.equal(offline.enabled(), false);
  const active = androidFixture(); await active.login(); await active.clicks.click();
  await active.context.controls.logout(); assert.equal(active.enabled(), false);
});

test("Android warm notification navigation acknowledges busy editors without discarding their route", async () => {
  const page = androidFixture(); const targets = [];
  page.context.open = target => { targets.push(target); return false; };
  vm.runInContext("installAndroidNavigation(open);", page.context);
  await new Promise(setImmediate);
  page.events["mira:native-notification"]({ detail: thread });
  await new Promise(setImmediate);
  assert.deepEqual(targets, [`/?thread=${thread}`]);
  assert.equal(page.nativeCalls.at(-1).accepted, false);
  page.events["mira:native-notification"]({ detail: "https://evil.test" });
  assert.equal(targets.length, 1);
});

test("Android shell negotiates insets, follows theme changes and opens settings without navigating", async () => {
  const page = androidFixture(), classes = new Set(), style = new Map(), actions = [];
  let themeChanged, settingsClick;
  const root = { dataset: { theme: "light" }, classList: { add: value => classes.add(value), contains: value => classes.has(value) },
    style: { setProperty: (key, value) => style.set(key, value), getPropertyValue: key => style.get(key) } };
  page.context.document.documentElement = root;
  page.context.document.querySelectorAll = () => [{ classList: { remove: name => actions.push(name) },
    addEventListener: (_, handler) => settingsClick = handler, closest: () => ({ hidePopover: () => actions.push("close") }) }];
  page.context.MutationObserver = class { constructor(handler) { themeChanged = handler; } observe() {} };
  page.context.Event = class { constructor(type) { this.type = type; } };
  page.context.window.dispatchEvent = event => actions.push(event.type);
  vm.runInContext("installAndroidShell();", page.context);
  await new Promise(setImmediate);
  assert.equal(classes.size, 0, "older native replies must not remove safe-area protection");
  page.events["mira:native-insets"]({ detail: { integrated: true, statusBarHeight: 32 } });
  assert.equal(classes.has("android-app"), true);
  assert.equal(style.get("--android-status-bar-height"), "32px");
  page.events["mira:native-insets"]({ detail: { integrated: true, statusBarHeight: -1 } });
  assert.equal(style.get("--android-status-bar-height"), "32px");
  root.dataset.theme = "dark"; themeChanged(); await new Promise(setImmediate);
  assert.equal(page.nativeCalls.at(-1).method, "shell");
  assert.equal(page.nativeCalls.at(-1).dark, true);
  settingsClick(); await new Promise(setImmediate);
  assert.equal(page.nativeCalls.at(-1).method, "settings");
  assert.ok(actions.includes("close"));
  page.events["mira:native-insets"]({ detail: { integrated: true, statusBarHeight: 24 } });
  assert.equal(style.get("--android-status-bar-height"), "24px", "fold/rotation updates the current inset");
});

// Only a public offline page is stored here. Conversation/authentication/API
// responses and app bundles always use the network and normal HTTP validation.
// The Server substitutes a content hash so each deployment refreshes the fallback.
const cacheName = "mira-pwa-offline-__MIRA_OFFLINE_VERSION__";
const offlineAssets = ["/offline.html", "/offline.css", "/offline.js", "/icons/mira.svg"];

self.addEventListener("install", (event) => {
  event.waitUntil((async () => {
    const cache = await caches.open(cacheName);
    await cache.addAll(offlineAssets.map((url) => new Request(url, { cache: "reload" })));
    await self.skipWaiting();
  })());
});

self.addEventListener("activate", (event) => {
  event.waitUntil((async () => {
    for (const key of await caches.keys()) {
      if (key.startsWith("mira-pwa-offline-") && key !== cacheName) await caches.delete(key);
    }
    // Safe to claim immediately: the worker never serves a cached app bundle and
    // never reloads an active conversation when an update becomes available.
    await self.clients.claim();
  })());
});

self.addEventListener("fetch", (event) => {
  const { request } = event;
  const url = new URL(request.url);
  if (request.method !== "GET" || url.origin !== self.location.origin) return;
  if (request.mode === "navigate" && url.pathname === "/") {
    event.respondWith(fetch(request).catch(async () => {
      const cache = await caches.open(cacheName);
      return await cache.match("/offline.html") ?? Response.error();
    }));
  } else if (offlineAssets.includes(url.pathname) && !url.search) {
    event.respondWith((async () => {
      const cache = await caches.open(cacheName);
      return await cache.match(request) ?? fetch(request);
    })());
  }
});

self.addEventListener("push", (event) => {
  event.waitUntil((async () => {
    let message;
    try { message = event.data?.json(); } catch { return; }
    const target = notificationTarget(message?.url);
    if (!target || typeof message?.tag !== "string") return;
    await self.registration.showNotification("对话已完成", {
      body: String(message.body ?? "Mira 对话").slice(0, 160),
      icon: "/icons/mira-192.png",
      tag: `mira-completed-${message.tag.slice(0, 64)}`,
      data: { url: target },
    });
  })());
});

function notificationTarget(value) {
  if (typeof value !== "string") return null;
  const id = "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}";
  return new RegExp(`^/\\?thread=${id}$`, "i").test(value) ? value : null;
}

function clientConversation(value) {
  try {
    const url = new URL(value);
    if (url.origin !== self.location.origin || url.pathname !== "/") return null;
    return notificationTarget(`/?thread=${url.searchParams.get("thread")}`);
  } catch { return null; }
}

function askClient(client, type, url) {
  if (!client.postMessage) return Promise.resolve(null);
  return new Promise((resolve) => {
    const channel = new MessageChannel();
    const finish = (value) => {
      clearTimeout(timer);
      channel.port1.close(); channel.port2.close();
      resolve(value);
    };
    const timer = setTimeout(() => finish(null), 1000);
    channel.port1.onmessage = (event) => finish(event.data);
    try { client.postMessage({ type, url, expiresAt: Date.now() + 1000 }, [channel.port2]); }
    catch { finish(null); }
  });
}

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const target = notificationTarget(event.notification.data?.url);
  if (!target) return;
  event.waitUntil((async () => {
    const absolute = new URL(target, self.location.origin).href;
    const windows = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    const pages = await Promise.all(windows.slice(0, 32).map(async (client) => ({
      client, context: await askClient(client, "mira:notification-context"),
    })));
    // WindowClient.url can lag behind SPA navigation. Ask the live page for its
    // route and display mode, and prefer the installed app over browser tabs.
    const sameConversation = ({ client, context }) => clientConversation(context?.url ?? client.url) === target;
    const installed = pages.filter(({ context }) => context?.standalone === true);
    installed.sort((left, right) => Number(sameConversation(right)) - Number(sameConversation(left)));
    for (const page of installed) {
      try {
        await page.client.focus();
        if (sameConversation(page)) return;
        const reply = await askClient(page.client, "mira:notification-open", target);
        if (reply?.accepted === true) return;
      } catch { /* window closed or could not be focused */ }
    }
    // Older pages can still reuse the same conversation. Never hard-navigate a
    // different page: only the app can preserve its draft and check active work.
    for (const page of pages.filter(sameConversation)) {
      try { await page.client.focus(); return; } catch { /* window closed meanwhile */ }
    }
    // With no reusable app, the browser decides whether this opens a standalone
    // installed app or a tab. The Web API has no way to force a specific mode.
    await self.clients.openWindow(absolute);
  })());
});

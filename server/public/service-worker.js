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

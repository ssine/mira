// Real IndexedDB in Chromium: version binding, lazy writes, LRU eviction and clearing.
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import http from "node:http";
const { chromium } = await import(process.argv[2] ?? "playwright");

const source = await fs.readFile(new URL("../server/public/client-cache.js", import.meta.url));
const server = http.createServer((request, response) => {
  if (request.url === "/client-cache.js") response.writeHead(200, { "content-type": "text/javascript" }).end(source);
  else response.writeHead(200, { "content-type": "text/html" }).end("<!doctype html><title>cache</title>");
});
await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage();
  await page.goto(`http://127.0.0.1:${server.address().port}/`);
  const result = await page.evaluate(async () => {
    const { ClientCache } = await import("/client-cache.js");
    const out = {};
    const cache = new ClientCache({ budgetBytes: 2_500, entryBytes: 1_500, maxEntries: 10, delayMs: 5 });
    cache.setVersion("1.0.0");
    cache.write("a", { text: "a".repeat(1_000) });
    out.pendingRead = (await cache.read("a"))?.text.length;
    await cache.flush();
    out.storedRead = (await cache.read("a"))?.text.length;

    // Lazy values decide at flush time; undefined keeps the previous copy.
    let state = "first";
    cache.write("lazy", () => state === "skip" ? undefined : { state });
    state = "settled";
    await cache.flush();
    out.lazy = (await cache.read("lazy"))?.state;
    cache.write("lazy", () => undefined);
    await cache.flush();
    out.lazyKept = (await cache.read("lazy"))?.state;

    // Oversized entries are not stored; the budget evicts the least recently used.
    cache.write("huge", { text: "h".repeat(2_000) });
    await cache.flush();
    out.huge = await cache.read("huge");
    await new Promise(resolve => setTimeout(resolve, 5));
    await cache.read("a"); // "a" becomes more recent than "lazy"
    await new Promise(resolve => setTimeout(resolve, 5));
    cache.write("b", { text: "b".repeat(1_000) });
    await cache.flush();
    await cache.pruning;
    out.afterEviction = { a: Boolean(await cache.read("a")), b: Boolean(await cache.read("b")), lazy: Boolean(await cache.read("lazy")) };
    // a(~1KB) + b(~1KB) + lazy(small) fits; another 1KB entry evicts the oldest use.
    await new Promise(resolve => setTimeout(resolve, 5));
    await cache.read("b");
    cache.write("c", { text: "c".repeat(1_000) });
    await cache.flush();
    await cache.pruning;
    out.afterSecondEviction = { a: Boolean(await cache.read("a")), b: Boolean(await cache.read("b")), c: Boolean(await cache.read("c")) };

    cache.writeSnapshot("nodes", [{ nodeId: "n" }]);
    await cache.flush();
    out.snapshot = (await cache.readSnapshot("nodes"))?.[0]?.nodeId;

    // Another Server version never sees these copies.
    const upgraded = new ClientCache();
    upgraded.setVersion("1.0.1");
    out.otherVersion = { entry: await upgraded.read("b"), snapshot: await upgraded.readSnapshot("nodes") };

    cache.remove("b");
    await cache.flush();
    out.removed = await cache.read("b");
    await cache.clear();
    out.cleared = { entry: await cache.read("c"), snapshot: await cache.readSnapshot("nodes") };
    return out;
  });
  assert.equal(result.pendingRead, 1_000);
  assert.equal(result.storedRead, 1_000);
  assert.equal(result.lazy, "settled");
  assert.equal(result.lazyKept, "settled");
  assert.equal(result.huge, undefined);
  assert.deepEqual(result.afterEviction, { a: true, b: true, lazy: true });
  assert.deepEqual(result.afterSecondEviction, { a: false, b: true, c: true });
  assert.equal(result.snapshot, "n");
  assert.deepEqual(result.otherVersion, { entry: undefined, snapshot: undefined });
  assert.equal(result.removed, undefined);
  assert.deepEqual(result.cleared, { entry: undefined, snapshot: undefined });
  console.log("client cache browser regression passed");
} finally {
  await browser.close();
  server.close();
}

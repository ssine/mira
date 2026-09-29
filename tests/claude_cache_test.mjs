import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { createHash } from "node:crypto";
import { cacheMaintenance, loadTranscript, DEFAULT_CACHE_BYTES } from "../node/internal/clauderuntime/assets/session-cache.mjs";

const sha = value => createHash("sha256").update(value).digest("hex");
async function fixture(t, limit = DEFAULT_CACHE_BYTES) {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "mira-claude-cache-"));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const cache = { root: path.join(dir, "data"), config: path.join(dir, "config.json") };
  const settings = (maxBytes = limit) => cacheMaintenance({ ...cache, action: "cache-configure", maxBytes });
  await settings();
  let records = [{ uuid: "one", text: "中文🎉\u0000", unknown: { future: 42 } }];
  const calls = [];
  let legacy = false, failure = false, truncate = false;
  const request = async url => {
    if (failure) throw new Error("HTTP 409 ownership changed");
    const query = new URL(url, "http://test").searchParams;
    let start = Number(query.get("after") || 0);
    if (start > records.length || (start && sha(JSON.stringify(records[start - 1])) !== query.get("prefix"))) start = 0;
    if (legacy) start = 0;
    calls.push({ start, after: query.get("after"), subpath: query.get("subpath") });
    const suffix = records.slice(start).map(e => JSON.stringify(e) + "\n").join("");
    // One byte per chunk exercises decoder state for all Unicode boundaries.
    const data = Buffer.from(truncate ? "" : suffix);
    let cursor = 0;
    const body = new ReadableStream({ pull(controller) { if (cursor < data.length) controller.enqueue(data.subarray(cursor, ++cursor)); else controller.close(); } });
    return new Response(body, { headers: legacy ? {} : {
      "X-Mira-Claude-Cache-Version": "1", "X-Mira-Claude-Cache-Start": String(start),
      "X-Mira-Claude-Cache-End": String(records.length), "X-Mira-Claude-Cache-Prefix": records.length ? sha(JSON.stringify(records.at(-1))) : "",
    } });
  };
  const load = (extra = {}) => loadTranscript({ cache, endpoint: "https://server.test", sessionId: "session-a", request, ...extra });
  const files = async suffix => (await fs.readdir(cache.root)).filter(n => n.endsWith(suffix)).map(n => path.join(cache.root, n));
  return { cache, settings, calls, load, files, records,
    replace(value) { records = value; }, legacy() { legacy = true; }, fail() { failure = true; }, truncate() { truncate = true; } };
}
test("cold, unchanged, incremental and reset reads preserve raw fields and Unicode", async t => {
  const f = await fixture(t);
  assert.deepEqual(await f.load(), f.records);
  const [file] = await f.files(".jsonl"), before = await fs.stat(file);
  assert.deepEqual(await f.load(), f.records);
  assert.equal(f.calls.at(-1).start, 1);
  f.records.push({ uuid: "two", content: "追加" });
  assert.deepEqual(await f.load(), f.records);
  assert.equal(f.calls.at(-1).start, 1);
  assert.equal((await fs.stat(file)).ino, before.ino, "append the suffix without rewriting the prefix");
  f.replace([{ uuid: "replacement", content: "restored database" }]);
  assert.deepEqual(await f.load(), [{ uuid: "replacement", content: "restored database" }]);
  assert.equal(f.calls.at(-1).start, 0);
  assert.equal((await fs.stat(file)).mode & 0o777, 0o600);
});
test("corruption, truncated cache and missing files fall back to Server", async t => {
  const f = await fixture(t);
  await f.load();
  for (const damage of ["modified", "truncated", "missing"]) {
    const [file] = await f.files(".jsonl");
    if (damage === "modified") await fs.writeFile(file, (await fs.readFile(file, "utf8")).replace("one", "two"));
    else if (damage === "truncated") await fs.truncate(file, 4);
    else await fs.unlink(file);
    assert.deepEqual(await f.load(), f.records);
    assert.equal(f.calls.at(-1).after, "0");
  }
});
test("crash tails are ignored and removed before the next incremental write", async t => {
  const f = await fixture(t);
  await f.load();
  const [file] = await f.files(".jsonl");
  await fs.appendFile(file, '{"partial":');
  f.records.push({ uuid: "two" });
  assert.deepEqual(await f.load(), f.records);
  assert.equal(f.calls.at(-1).after, "1");
  assert.equal(await fs.readFile(file, "utf8"), f.records.map(e => JSON.stringify(e) + "\n").join(""));
});
test("legacy Server reads fully; ownership and incomplete network responses never use offline cache", async t => {
  const f = await fixture(t);
  await f.load();
  f.legacy();
  assert.deepEqual(await f.load(), f.records);
  f.fail();
  await assert.rejects(f.load(), /ownership/);
  const g = await fixture(t);
  await g.load();
  g.records.push({ uuid: "two" });
  g.truncate();
  await assert.rejects(g.load(), /Incomplete/);
});
test("LRU quota, oversized sessions, disabled cache and persistent configuration", async t => {
  const f = await fixture(t, 5000);
  f.records.push({ text: "x".repeat(450) });
  await f.load();
  await f.load({ sessionId: "session-b" });
  // Reservation includes atomic metadata replacements; only one transcript fits.
  assert.equal((await f.files(".json")).length, 1);
  await f.load();
  assert.equal(f.calls.at(-1).after, "0");
  f.records.push({ text: "x".repeat(6000) });
  assert.deepEqual(await f.load(), f.records, "budget never limits resumable history");
  assert.equal((await f.files(".json")).length, 0);
  await f.settings(0);
  assert.deepEqual(await f.load(), f.records);
  assert.equal((await f.files(".jsonl")).length, 0);
  assert.equal((await cacheMaintenance({ ...f.cache, action: "cache-status" })).maxBytes, 0);
  for (const maxBytes of [-1, 0.1, NaN, 2 ** 53, "4"])
    await assert.rejects(f.settings(maxBytes), /nonnegative/);
});
test("session, endpoint and native subpath are independent cache namespaces", async t => {
  const f = await fixture(t);
  const variants = [{}, { subpath: "subagents/child.jsonl" }, { sessionId: "other" }, { endpoint: "https://other.test" }];
  for (const variant of variants) { await f.load(variant); assert.equal(f.calls.at(-1).after, "0"); }
  for (const variant of variants) { await f.load(variant); assert.equal(f.calls.at(-1).after, "1"); }
});
test("concurrent writes stay bounded; active and abandoned locks never block a read", async t => {
  const f = await fixture(t, 5000);
  await Promise.all(Array.from({ length: 8 }, (_, i) => f.load({ sessionId: `parallel-${i}` })));
  const state = await cacheMaintenance({ ...f.cache, action: "cache-status" });
  assert.ok(state.usedBytes <= 5000);
  await fs.writeFile(path.join(f.cache.root, "writer.lock"), String(process.pid));
  assert.deepEqual(await f.load(), f.records);
  assert.equal((await f.settings(0)).cleanupPending, true);
  await fs.writeFile(path.join(f.cache.root, "writer.lock"), "2147483647");
  assert.equal((await cacheMaintenance({ ...f.cache, action: "cache-status" })).usedBytes, 0);
  await fs.writeFile(path.join(f.cache.root, "writer.lock"), "");
  const old = new Date(Date.now() - 60_000);
  await fs.utimes(path.join(f.cache.root, "writer.lock"), old, old);
  await cacheMaintenance({ ...f.cache, action: "cache-status" });
  await assert.rejects(fs.stat(path.join(f.cache.root, "writer.lock")), { code: "ENOENT" });
});

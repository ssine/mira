import test from "node:test";
import assert from "node:assert/strict";
import { serverClient } from "../node/internal/clauderuntime/assets/store.mjs";

function fixture(t, respond) {
  let elapsed = 0;
  const calls = [], delays = [], timeouts = [];
  t.mock.method(performance, "now", () => elapsed);
  t.mock.method(globalThis, "setTimeout", (callback, delay) => {
    delays.push(delay); elapsed += delay; queueMicrotask(callback);
  });
  t.mock.method(AbortSignal, "timeout", ms => { timeouts.push(ms); return new AbortController().signal; });
  t.mock.method(globalThis, "fetch", async (url, options) => {
    calls.push({ url: String(url), body: options.body, elapsed });
    return respond(calls.length, { advance: ms => elapsed += ms, options });
  });
  const client = serverClient({ endpoint: "http://localhost", credential: "fixture", sessionId: "session", turnId: "turn", revision: 1 });
  return { client, calls, delays, timeouts };
}

test("storage survives a 12-second outage with exactly the same event", async t => {
  const payload = { type: "assistant", text: "original" };
  const f = fixture(t, count => {
    payload.text = "mutated";
    if (f.calls.at(-1).elapsed < 12_000) return new Response(null, { status: count % 2 ? 502 : 503 });
    return new Response(null);
  });
  await f.client.event(payload);
  assert.ok(f.calls.at(-1).elapsed >= 12_000);
  assert.ok(f.calls.length > 3);
  assert.equal(new Set(f.calls.map(c => c.body)).size, 1);
  assert.equal(JSON.parse(f.calls[0].body).payload.text, "original");
  assert.ok(f.delays.every(delay => delay <= 2000));
});

test("lost append responses and SDK retries retain UUID-less entries and operation IDs", async t => {
  const entries = [{ text: "without UUID", unknown: "\u0000" }];
  const f = fixture(t, count => {
    if (count === 1) { entries.push({ text: "later mutation" }); throw new TypeError("fetch failed"); }
    return new Response(null);
  });
  await f.client.store.append({ sessionId: "session", subpath: "subagents/child.jsonl" }, entries);
  await f.client.store.append({ sessionId: "session", subpath: "subagents/child.jsonl" }, entries);
  assert.equal(f.calls.length, 3);
  assert.equal(new Set(f.calls.map(c => c.url)).size, 1);
  assert.equal(new Set(f.calls.map(c => c.body)).size, 1);
  assert.deepEqual(JSON.parse(f.calls[0].body), { text: "without UUID", unknown: "\u0000" });
  assert.equal(new URL(f.calls[0].url).searchParams.get("subpath"), "subagents/child.jsonl");
});

test("permanent storage errors fail immediately; safe transient statuses retry", async t => {
  for (const status of [400, 401, 403, 404, 409, 413, 422, 501]) {
    await t.test(String(status), async t => {
      const f = fixture(t, () => new Response(null, { status }));
      await assert.rejects(f.client.event({ type: "mira_user" }), new RegExp(`HTTP ${status}`));
      assert.equal(f.calls.length, 1);
      assert.equal(f.delays.length, 0);
    });
  }
  for (const status of [408, 429, 500, 502, 503, 504]) {
    await t.test(String(status), async t => {
      const f = fixture(t, count => new Response(null, { status: count === 1 ? status : 200 }));
      await f.client.event({ type: "mira_user" });
      assert.equal(f.calls.length, 2);
    });
  }
});

test("quick failures and hung requests share a deadline below the SDK's 60-second limit", async t => {
  const f = fixture(t, () => new Response(null, { status: 503 }));
  await assert.rejects(f.client.store.append({ sessionId: "session" }, [{}]), /HTTP 503/);
  assert.ok(f.calls.at(-1).elapsed > 40_000 && f.calls.at(-1).elapsed < 45_000);
  for (let i = 0; i < f.calls.length; i++) assert.equal(f.calls[i].elapsed + f.timeouts[i], 45_000);
  t.mock.restoreAll();
  const hung = fixture(t, (_count, { advance }) => {
    advance(45_000);
    throw new DOMException("timed out", "TimeoutError");
  });
  await assert.rejects(hung.client.event({ type: "mira_completed" }), /timed out/);
  assert.equal(hung.calls.length, 1);
});

test("resume reads still fail explicitly instead of falling back to local history", async t => {
  const f = fixture(t, () => new Response(null, { status: 503 }));
  await assert.rejects(f.client.store.load({ sessionId: "session" }), /HTTP 503/);
  assert.equal(f.calls.length, 1);
});

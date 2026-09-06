import assert from "node:assert/strict";
import test from "node:test";
import { AccountReader } from "../server/account-reader.mjs";
import { quotaSegments } from "../server/public/account-history.js";

test("account sampling only reads metadata, ignores unrelated tools and closes its tunnel", async () => {
  const calls = [], envelopes = [];
  const account = { type: "chatgpt", email: "test@example.test", planType: "pro" };
  const reader = new AccountReader((nodeId, message) => {
    envelopes.push(message);
    if (message.type === "appserver.message") {
      const request = JSON.parse(message.payload);
      if (request.id === undefined) return true;
      calls.push(request);
      queueMicrotask(() => {
        const respond = payload => reader.handle(nodeId, { type: "appserver.message", sessionId: message.sessionId, payload: JSON.stringify(payload) });
        respond({ id: request.id, method: "item/tool/call", params: { namespace: "home_nodes" } });
        respond({ id: request.id, result: request.method === "account/read" ? { account } : {} });
      });
    }
    return true;
  });
  assert.deepEqual((await reader.read("node")).account, account);
  assert.deepEqual(calls.map(call => call.method), ["initialize", "account/read", "account/rateLimits/read", "account/read"]);
  assert.ok(calls.filter(call => call.method === "account/read").every(call => call.params.refreshToken === false));
  assert.equal(envelopes.at(-1).type, "appserver.close");
  assert.equal(reader.sessions.size, 0);
});

test("timeout, cancellation and wrong-node replies cannot leak account sessions", async () => {
  let envelope;
  const reader = new AccountReader((_node, message) => { envelope = message; return true; }, { timeoutMs: 15 });
  const timed = reader.read("node");
  assert.equal(reader.handle("other", { type: "appserver.closed", sessionId: envelope.sessionId }), false);
  assert.equal(reader.sessions.size, 1);
  await assert.rejects(timed);
  assert.equal(reader.sessions.size, 0);
  const controller = new AbortController();
  const aborted = reader.read("node", { signal: controller.signal });
  controller.abort();
  await assert.rejects(aborted);
  assert.equal(reader.sessions.size, 0);
});

test("account replacement between identity and limits rejects the sample", async () => {
  let identities = 0;
  const reader = new AccountReader((nodeId, message) => {
    if (message.type === "appserver.message") {
      const request = JSON.parse(message.payload);
      if (request.id !== undefined) queueMicrotask(() => reader.handle(nodeId, { type: "appserver.message", sessionId: message.sessionId,
        payload: JSON.stringify({ id: request.id, result: request.method === "account/read" ? { account: { type: "chatgpt", email: `${++identities}@example.test` } } : {} }) }));
    }
    return true;
  });
  await assert.rejects(reader.read("node"), /account changed/);
  assert.equal(reader.sessions.size, 0);
});

test("chart preserves zeros and reset jumps while breaking failed and offline samples", () => {
  const points = [{ at: 0, remaining: 20 }, { at: 300, remaining: 0 }, { at: 600, remaining: 100 },
    { at: 900, remaining: null }, { at: 1200, remaining: 90 }, { at: 3000, remaining: 70 }];
  assert.deepEqual(quotaSegments(points, 300).map(part => part.map(point => point.remaining)), [[20, 0, 100], [90], [70]]);
});

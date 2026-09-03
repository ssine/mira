import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { setImmediate } from "node:timers/promises";
import test from "node:test";
import { NodeChannel } from "../server/node-channel.mjs";

class Socket extends EventEmitter {
  readyState = 1;
  sent = [];
  close() { this.readyState = 2; }
  finishClose() { this.readyState = 3; this.emit("close"); }
  send(payload) { this.sent.push(JSON.parse(payload)); }
  respond(requestId, result) {
    this.emit("message", JSON.stringify({ type: "response", requestId, ok: true, result }));
  }
}

test("late replaced close cannot disconnect the successor or reject its requests/proxies", async () => {
  const statuses = [];
  const channel = new NodeChannel({server: new EventEmitter(), authService: {}, pool: {
    query: async (_sql, [, json]) => statuses.push(JSON.parse(json)),
  }});
  const first = new Socket();
  channel.attachNode("node-1", first);
  const oldWork = channel.invoke("node-1", "status", {});
  const oldRejected = assert.rejects(oldWork, { code: "node_offline" });
  const oldProxy = new Socket();
  channel.attachProxy("node-1", oldProxy, { kind: "admin" });
  const second = new Socket();
  channel.attachNode("node-1", second);
  await oldRejected;
  assert.equal(oldProxy.readyState, 2);
  const newWork = channel.invoke("node-1", "status", {});
  const requestId = second.sent.at(-1).requestId;
  const newProxy = new Socket();
  channel.attachProxy("node-1", newProxy, { kind: "admin" });
  first.finishClose();
  first.respond(requestId, "stale result");
  assert.equal(channel.isConnected("node-1"), true);
  assert.equal(channel.pending.size, 1);
  assert.equal(newProxy.readyState, 1);
  second.respond(requestId, "current result");
  assert.equal(await newWork, "current result");
  await setImmediate();
  assert.equal(statuses.at(-1).connected, true);
  assert.ok(statuses.every(status => status.connected));
  second.finishClose();
  await setImmediate();
  assert.equal(statuses.at(-1).connected, false);
  assert.equal(newProxy.readyState, 2);
  assert.equal(channel.statusWrites.size, 0);
  channel.close();
});

test("slow database disconnect commits before a subsequent connection and queues are released", async () => {
  const writes = [];
  const channel = new NodeChannel({server: new EventEmitter(), authService: {}, pool: {
    query: (_sql, [, json]) => new Promise(resolve => writes.push({status: JSON.parse(json), resolve})),
  }});
  const first = new Socket();
  channel.attachNode("node-1", first);
  await setImmediate();
  assert.equal(writes.length, 1);
  first.finishClose();
  const second = new Socket();
  channel.attachNode("node-1", second);
  await setImmediate();
  assert.equal(writes.length, 1);
  writes[0].resolve();
  await setImmediate();
  assert.equal(writes.length, 2);
  assert.equal(writes[1].status.connected, false);
  writes[1].resolve();
  await setImmediate();
  assert.equal(writes.length, 3);
  assert.equal(writes[2].status.connected, true);
  writes[2].resolve();
  await setImmediate();
  assert.equal(channel.statusWrites.size, 0);
  assert.equal(channel.isConnected("node-1"), true);
  channel.close();
});

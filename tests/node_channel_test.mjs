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

test("App Server proxy persists its store-scoped thread runtime binding", async () => {
  const writes = [];
  const channel = new NodeChannel({
    server: new EventEmitter(), authService: {},
    pool: { query: async (sql, params) => writes.push({ sql, params }) },
  });
  const proxy = {
    storeId: "team-store", targetNodeId: "00000000-0000-4000-8000-000000000001", ws: new Socket(),
  };
  await channel.forwardAppServerMessage(proxy, JSON.stringify({
    method: "turn/started",
    params: { threadId: "01a06b06-41a3-7aa2-8c46-b406591f8f0a", turn: { id: "turn-1" } },
  }));
  await channel.forwardAppServerMessage(proxy, JSON.stringify({
    method: "item/started",
    params: { threadId: "01a06b06-41a3-7aa2-8c46-b406591f8f0a", item: { id: "subagent-item" } },
  }));
  await channel.forwardAppServerMessage(proxy, JSON.stringify({
    method: "turn/started",
    params: { threadId: "01a06b07-0000-7000-8000-000000000002", turn: { id: "subagent-turn" } },
  }));
  await setImmediate();
  assert.equal(proxy.threadId, "01a06b06-41a3-7aa2-8c46-b406591f8f0a");
  assert.match(writes[0].sql, /INSERT INTO mira_codex_thread_runtimes/);
  assert.deepEqual(writes[0].params, [
    "team-store", "01a06b06-41a3-7aa2-8c46-b406591f8f0a", "00000000-0000-4000-8000-000000000001",
  ]);
  assert.equal(writes.length, 2, "repeated events rewrote a binding or the subagent thread was not bound");
  assert.deepEqual(writes[1].params, [
    "team-store", "01a06b07-0000-7000-8000-000000000002", "00000000-0000-4000-8000-000000000001",
  ]);
  channel.close();
});

test("App Server proxy tells Codex to use the absolute Mira CLI without adding SSH tools", async () => {
  const channel = new NodeChannel({
    server: new EventEmitter(), authService: {},
    pool: { query: async () => ({ rowCount: 1 }) },
  });
  const node = new Socket();
  channel.attachNode("node-1", node);
  const client = new Socket();
  channel.attachProxy("node-1", client, { kind: "admin" }, "personal", {
    platform: "linux",
    capabilities: { appServer: true },
    desiredAppServer: { defaultCwd: "/srv/mira-workspace" },
    reportedAppServer: {
      codexPath: "/opt/mira/versions/0.11.2/mira-codex-package/bin/codex",
    },
  });

  client.emit("message", JSON.stringify({
    id: 1,
    method: "thread/start",
    params: {
      developerInstructions: "Keep the user's existing instruction.",
      dynamicTools: [{ name: "client_tool", description: "client tool", inputSchema: {} }],
    },
  }));
  const start = JSON.parse(node.sent.at(-1).payload);
  assert.equal(start.method, "thread/start");
  assert.equal(start.params.cwd, "/srv/mira-workspace");
  assert.match(start.params.developerInstructions, /^Keep the user's existing instruction\./);
  assert.match(start.params.developerInstructions, /'\/opt\/mira\/versions\/0\.11\.2\/mira' nodes list --json/);
  assert.match(start.params.developerInstructions, /SSH, SCP, and SFTP are CLI-only operations/);
  assert.equal(start.params.dynamicTools.filter((tool) => tool.name === "home_nodes").length, 1);
  assert.equal(start.params.dynamicTools.filter((tool) => tool.name === "client_tool").length, 1);
  assert.equal(start.params.dynamicTools.some((tool) => /ssh|scp|sftp/i.test(tool.name)), false);

  client.emit("message", JSON.stringify({
    id: 2,
    method: "thread/resume",
    params: {
      threadId: "thread-1",
      developerInstructions: [
        "Keep this resume instruction.",
        "MIRA_CLI_INSTRUCTIONS_V1_BEGIN",
        "obsolete path",
        "MIRA_CLI_INSTRUCTIONS_V1_END",
      ].join("\n"),
    },
  }));
  const resume = JSON.parse(node.sent.at(-1).payload);
  assert.match(resume.params.developerInstructions, /^Keep this resume instruction\./);
  assert.equal(resume.params.cwd, undefined, "resume must preserve the thread's persisted cwd");
  assert.doesNotMatch(resume.params.developerInstructions, /obsolete path/);
  assert.equal(resume.params.developerInstructions.match(/MIRA_CLI_INSTRUCTIONS_V1_BEGIN/g)?.length, 1);

  channel.close();
});

test("App Server proxy emits native Windows CLI and cwd syntax", () => {
  const channel = new NodeChannel({
    server: new EventEmitter(), authService: {},
    pool: { query: async () => ({ rowCount: 1 }) },
  });
  const node = new Socket();
  channel.attachNode("windows-node", node);
  const client = new Socket();
  channel.attachProxy("windows-node", client, { kind: "admin" }, "personal", {
    platform: "windows",
    capabilities: { appServer: true },
    desiredAppServer: { defaultCwd: "C:\\code\\mira" },
    reportedAppServer: { miraCliPath: "C:\\Program Files\\Mira\\mira.exe" },
  });
  client.emit("message", JSON.stringify({ id: 1, method: "thread/start", params: {} }));
  const start = JSON.parse(node.sent.at(-1).payload);
  assert.equal(start.params.cwd, "C:\\code\\mira");
  assert.match(start.params.developerInstructions,
    /& 'C:\\Program Files\\Mira\\mira\.exe' nodes list --json/);
  channel.close();
});

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

class ThreadStartPool {
  records = new Map();
  runtimeWrites = [];
  deleted = new Set();

  async query(sql, params = []) {
    if (sql.startsWith("SELECT thread_id FROM mira_thread_actions")) return { rowCount: params[1].some(id => this.deleted.has(id)) ? 1 : 0, rows: [] };
    const key = params.length >= 3 ? params.slice(0, 3).join("\n") : null;
    if (sql.includes("INSERT INTO mira_appserver_thread_start_requests")) {
      if (this.records.has(key)) return { rowCount: 0, rows: [] };
      this.records.set(key, { request_sha256: params[4], status: "pending", thread_id: null, response: null });
      return { rowCount: 1, rows: [{ client_request_id: params[2] }] };
    }
    if (sql.includes("FROM mira_appserver_thread_start_requests")) {
      const row = this.records.get(key);
      return { rowCount: row ? 1 : 0, rows: row ? [row] : [] };
    }
    if (sql.includes("SET status = 'completed'")) {
      Object.assign(this.records.get(key), {
        status: "completed", thread_id: params[3], response: JSON.parse(params[4]),
      });
      return { rowCount: 1, rows: [] };
    }
    if (sql.includes("SET status = 'failed'")) {
      Object.assign(this.records.get(key), { status: "failed", thread_id: null, response: null });
      return { rowCount: 1, rows: [] };
    }
    if (sql.includes("SET target_node_id") && this.records.get(key)?.status === "failed") {
      Object.assign(this.records.get(key), { status: "pending", thread_id: null, response: null });
      return { rowCount: 1, rows: [{ client_request_id: params[2] }] };
    }
    if (sql.includes("INSERT INTO mira_codex_thread_runtimes")) this.runtimeWrites.push(params);
    return { rowCount: 1, rows: [] };
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
    pool: { query: async sql => ({ rowCount: sql.includes("mira_thread_actions") ? 0 : 1 }) },
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
  assert.equal(start.params.approvalPolicy, "never");
  assert.equal(start.params.sandbox, "danger-full-access");
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
  await setImmediate();
  const resume = JSON.parse(node.sent.at(-1).payload);
  assert.equal(resume.params.approvalPolicy, "never");
  assert.equal(resume.params.sandbox, "danger-full-access");
  assert.match(resume.params.developerInstructions, /^Keep this resume instruction\./);
  assert.equal(resume.params.cwd, undefined, "resume must preserve the thread's persisted cwd");
  assert.doesNotMatch(resume.params.developerInstructions, /obsolete path/);
  assert.equal(resume.params.developerInstructions.match(/MIRA_CLI_INSTRUCTIONS_V1_BEGIN/g)?.length, 1);

  client.emit("message", JSON.stringify({ id: 4, method: "thread/fork", params: { threadId: "source-thread", excludeTurns: true } }));
  await setImmediate();
  const fork = JSON.parse(node.sent.at(-1).payload);
  assert.equal(fork.params.cwd, undefined, "fork inherits its source directory");
  assert.equal(fork.params.approvalPolicy, "never");
  assert.equal(fork.params.sandbox, "danger-full-access");
  assert.equal(fork.params.dynamicTools, undefined, "fork protocol inherits tools rather than accepting dynamicTools");
  assert.match(fork.params.developerInstructions, /MIRA_CLI_INSTRUCTIONS_V1_BEGIN/);

  client.emit("message", JSON.stringify({
    id: 3,
    method: "thread/start",
    params: { approvalPolicy: "on-request", sandbox: "read-only" },
  }));
  const explicit = JSON.parse(node.sent.at(-1).payload);
  assert.equal(explicit.params.approvalPolicy, "on-request");
  assert.equal(explicit.params.sandbox, "read-only");

  channel.close();
});

for (const method of ["thread/start", "thread/fork"]) test(`App Server proxy coalesces and durably replays idempotent ${method} requests`, async () => {
  const baseParams = method === "thread/fork" ? { threadId: "source-thread" } : {};
  const pool = new ThreadStartPool();
  const channel = new NodeChannel({ server: new EventEmitter(), authService: {}, pool });
  const node = new Socket();
  channel.attachNode("node-1", node);
  const caller = { kind: "admin", subjectId: "admin-1" };
  const first = new Socket();
  channel.attachProxy("node-1", first, caller, "personal");
  const firstSessionId = node.sent.at(-1).sessionId;
  const requestId = "8c043d32-a487-4b37-959f-4ec51673b1eb";
  first.emit("message", JSON.stringify({
    id: 10, method, params: { ...baseParams, cwd: "/work", miraRequestId: requestId },
  }));
  await setImmediate();
  const forwarded = node.sent.filter((message) => message.type === "appserver.message");
  assert.equal(forwarded.length, 1);
  assert.equal(JSON.parse(forwarded[0].payload).params.miraRequestId, undefined);

  const second = new Socket();
  channel.attachProxy("node-1", second, caller, "personal");
  second.emit("message", JSON.stringify({
    id: 20, method, params: { ...baseParams, miraRequestId: requestId, cwd: "/work" },
  }));
  await setImmediate();
  assert.equal(node.sent.filter((message) => message.type === "appserver.message").length, 1,
    "a concurrent duplicate reached Codex");

  first.finishClose();
  assert.equal(channel.proxies.has(firstSessionId), true,
    "the App Server tunnel was not retained long enough to capture a lost response");
  node.emit("message", JSON.stringify({
    type: "appserver.message",
    sessionId: firstSessionId,
    payload: JSON.stringify({ id: 10, result: { thread: { id: "thread-1" }, cwd: "/work" } }),
  }));
  await setImmediate();
  await setImmediate();
  assert.equal(first.sent.length, 0, "a closed client unexpectedly received the response");
  assert.equal(second.sent.at(-1).id, 20);
  assert.equal(second.sent.at(-1).result.thread.id, "thread-1");

  const third = new Socket();
  channel.attachProxy("node-1", third, caller, "personal");
  third.emit("message", JSON.stringify({
    id: 30, method, params: { ...baseParams, cwd: "/work", miraRequestId: requestId },
  }));
  await setImmediate();
  assert.equal(third.sent.at(-1).id, 30);
  assert.equal(third.sent.at(-1).result.thread.id, "thread-1");
  assert.equal(node.sent.filter((message) => message.type === "appserver.message").length, 1,
    "a completed duplicate was not replayed from the idempotency record");
  third.emit("message", JSON.stringify({ id: 40, method: method === "thread/fork" ? "thread/start" : "thread/fork", params: { ...baseParams, cwd: "/work", miraRequestId: requestId } }));
  await setImmediate();
  assert.match(third.sent.at(-1).error.message, /reused/, "creation request IDs cannot cross methods");
  assert(pool.runtimeWrites.some((values) => values[1] === "thread-1"), "created thread is bound to its execution Node");
  pool.records.values().next().value.response = { deleted: true };
  third.emit("message", JSON.stringify({ id: 50, method, params: { ...baseParams, cwd: "/work", miraRequestId: requestId } }));
  await setImmediate();
  assert.equal(third.sent.at(-1).error.code, -32004, "deleted creation cannot be replayed from cached content");
  pool.deleted.add("thread-1");
  third.emit("message", JSON.stringify({ id: 60, method: "thread/resume", params: { threadId: "thread-1" } }));
  await setImmediate();
  assert.equal(third.sent.at(-1).error.code, -32004, "deleted threads cannot be restored from runtime memory");
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

test('tool-free ephemeral threads retain isolation and never acquire durable runtime bindings', async () => {
  const pool = new ThreadStartPool();
  const channel = new NodeChannel({ server: new EventEmitter(), authService: {}, pool });
  const node = new Socket(), client = new Socket();
  channel.attachNode('node-1', node);
  channel.attachProxy('node-1', client, { kind: 'admin' }, 'personal', { platform: 'linux' });
  const proxy = [...channel.proxies.values()][0];
  await channel.forwardProxyClientMessage(proxy, JSON.stringify({ id: 1, method: 'thread/start', params: {
    ephemeral: true, dynamicTools: [], sandbox: 'read-only', developerInstructions: 'Title only',
  } }));
  const request = JSON.parse(node.sent.at(-1).payload);
  assert.deepEqual(request.params.dynamicTools, []);
  assert.equal(request.params.developerInstructions, 'Title only');
  assert.equal(request.params.sandbox, 'read-only');
  await channel.forwardAppServerMessage(proxy, JSON.stringify({ method: 'thread/started', params: { thread: { id: 'temporary', ephemeral: true } } }));
  await channel.forwardAppServerMessage(proxy, JSON.stringify({ id: 1, result: { thread: { id: 'temporary', ephemeral: true } } }));
  await channel.forwardProxyClientMessage(proxy, JSON.stringify({ id: 2, method: 'turn/start', params: { threadId: 'temporary' } }));
  await channel.forwardAppServerMessage(proxy, JSON.stringify({ id: 2, result: { turn: { id: 'title-turn' } } }));
  await channel.forwardAppServerMessage(proxy, JSON.stringify({ method: 'turn/started', params: { threadId: 'temporary' } }));
  assert.equal(pool.runtimeWrites.length, 0);
  assert.equal(proxy.threadId, null);
  let dispatched = false;
  channel.capabilityService = { invoke: () => { dispatched = true; } };
  await channel.forwardAppServerMessage(proxy, JSON.stringify({ id: 3, method: 'item/tool/call', params: {
    threadId: 'temporary', namespace: 'home_nodes', tool: 'process', arguments: {},
  } }));
  assert.equal(dispatched, false);
  assert.equal(JSON.parse(node.sent.at(-1).payload).error.code, -32601);
  channel.close();
});

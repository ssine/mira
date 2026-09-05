// Real Codex + PostgreSQL, loopback-only model/store fault injection. No production identity.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import readline from "node:readline";
import { spawn } from "node:child_process";
import pg from "../server/node_modules/pg/lib/index.js";
import { initializeDatabase } from "../server/db.mjs";
import { commitDelta, getStoreHead, getThreadHistory, getSnapshot } from "../server/thread-store.mjs";

const binary = process.env.CODEX_TEST_BINARY;
assert(binary && path.isAbsolute(binary), "CODEX_TEST_BINARY must name the candidate Codex binary");
const base = new URL(process.env.MIRA_TEST_DATABASE_URL ?? "postgresql://mira:mira-local@127.0.0.1:55432/mira");
assert(["127.0.0.1", "localhost", "[::1]"].includes(base.hostname), "fault injection requires a local test PostgreSQL");
const database = `mira_retry_${process.pid}_${crypto.randomBytes(4).toString("hex")}`;
const admin = new pg.Pool({ connectionString: base.toString() });
const directory = await fs.mkdtemp(path.join(os.tmpdir(), "mira-runtime-reliability-"));
let pool, created = false, current;
const children = [];
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const bound = (promise, label, ms = 30_000) => {
  let timer;
  return Promise.race([promise, new Promise((_, reject) => {
    timer = setTimeout(() => reject(Error(`timeout: ${label}`)), ms);
  })]).finally(() => clearTimeout(timer));
};
const sendJSON = (res, status, value) => {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(value));
};
const fixture = http.createServer(async (req, res) => {
  try {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const encoded = Buffer.concat(chunks).toString("utf8");
    const url = new URL(req.url, "http://localhost");
    if (url.pathname.endsWith("/responses")) {
      current.modelRequests++;
      if (current.mode === "malformed") {
        return sendJSON(res, 400, { error: { message: "MIRA_MOCK_MODEL_REACHED", type: "invalid_request_error" } });
      }
      const body = JSON.parse(encoded);
      const hasOutput = body.input.some((item) => item.type === "function_call_output" && item.call_id === "retry-probe-call");
      if (hasOutput) {
        const stored = await getThreadHistory(pool, current.store, current.thread, null, null);
        assert.equal(stored.status, 200);
        assert(stored.body.items.some((item) => item.payload?.call_id === "retry-probe-call" && item.payload?.type === "function_call_output"), "model contacted before output was durable");
      }
      const item = hasOutput ? {
        type: "message", role: "assistant", id: "msg-ok", content: [{ type: "output_text", text: "RETRY_OK" }],
      } : {
        type: "function_call", call_id: "retry-probe-call", name: "reliability_probe", arguments: "{}",
      };
      const events = [
        { type: "response.created", response: { id: `resp-${current.modelRequests}` } },
        { type: "response.output_item.done", item },
        { type: "response.completed", response: { id: `resp-${current.modelRequests}`, usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } } },
      ];
      res.writeHead(200, { "content-type": "text/event-stream" });
      return res.end(events.map((event) => `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`).join(""));
    }
    const parts = url.pathname.split("/").filter(Boolean);
    assert.equal(parts[1], "stores");
    const store = parts[2];
    if (req.method === "GET") {
      if (parts[3] === "threads") {
        const result = await getThreadHistory(pool, store, parts[4], Number(url.searchParams.get("generation")) || null, Number(url.searchParams.get("throughVersion")) || null);
        return sendJSON(res, result.status, result.body);
      }
      return sendJSON(res, 200, await getStoreHead(pool, store));
    }
    const body = JSON.parse(encoded);
    const toolOutput = body.historyChanges.some((change) => change.items?.some((item) => item.payload?.type === "function_call_output" && item.payload?.call_id === "retry-probe-call"));
    if (toolOutput) {
      current.attempts.push({ id: req.headers["x-codex-operation-id"], encoded });
      if (current.mode === "denied") return sendJSON(res, 403, { error: "fixture denies writes" });
      if (current.attempts.length === 1) return sendJSON(res, 502, { error: "fixture restart" });
    }
    const result = await commitDelta(pool, store, body, req.headers);
    if (toolOutput && current.attempts.length === 2) {
      assert.equal(result.status, 200);
      req.socket.destroy(); // PostgreSQL committed; client never received acknowledgement.
      return;
    }
    if (toolOutput && current.attempts.length === 3) assert.equal(result.body.duplicate, true);
    sendJSON(res, result.status, result.body);
  } catch (error) {
    current.fixtureError = error;
    sendJSON(res, 500, { error: "fixture failure" });
  }
});

async function client(mode, inMemory = false, store = `retry-${mode}`) {
  current = { mode, store, modelRequests: 0, attempts: [], toolCalls: 0, events: [] };
  const context = current;
  const home = path.join(directory, mode);
  await fs.mkdir(home);
  const endpoint = `http://127.0.0.1:${fixture.address().port}`;
  const overrides = [
    'model="gpt-5.4"', 'model_provider="fixture"',
    `model_providers.fixture={name="Loopback fixture",base_url="${endpoint}/v1",wire_api="responses",request_max_retries=0,stream_max_retries=0}`,
    ...(inMemory ? [
      'experimental_thread_store.type="in_memory"',
      `experimental_thread_store.id="${context.store}"`,
    ] : [
      'experimental_thread_store.type="remote_http"',
      `experimental_thread_store.endpoint="${endpoint}"`,
      `experimental_thread_store.store_id="${context.store}"`,
      'experimental_thread_store.bearer_token="local-test-only"',
    ]),
    'features.code_mode=false',
  ];
  const proc = spawn(binary, ["app-server", ...overrides.flatMap((value) => ["-c", value])], {
    cwd: directory, env: { ...process.env, CODEX_HOME: home }, stdio: ["pipe", "pipe", "pipe"],
  });
  children.push(proc);
  let stderr = "", next = 0;
  proc.stderr.on("data", (data) => { stderr = (stderr + data).slice(-12_000); });
  proc.on("error", (error) => { context.fixtureError = error; });
  const pending = new Map();
  proc.on("close", (code) => {
    for (const { reject } of pending.values()) reject(Error(`runtime exited (${code}): ${stderr}`));
    pending.clear();
  });
  proc.stdin.on("error", () => {}); // close reports startup failures with stderr to pending RPCs.
  const notify = (message) => proc.stdin.write(JSON.stringify(message) + "\n");
  readline.createInterface({ input: proc.stdout }).on("line", (line) => {
    let message;
    try { message = JSON.parse(line); } catch { return; }
    if (message.method === "item/tool/call") {
      context.toolCalls++;
      notify({ id: message.id, result: { contentItems: [{ type: "inputText", text: "TOOL_RESULT_ONCE" }], success: true } });
    } else if (message.id !== undefined && pending.has(message.id)) {
      const { resolve, reject } = pending.get(message.id);
      pending.delete(message.id);
      message.error ? reject(Error(JSON.stringify(message.error))) : resolve(message.result);
    } else context.events.push(message);
  });
  const call = (method, params) => bound(new Promise((resolve, reject) => {
    const id = ++next;
    pending.set(id, { resolve, reject });
    notify({ id, method, params });
  }), method);
  const wait = async (turn) => bound((async () => {
    while (true) {
      if (context.fixtureError) throw context.fixtureError;
      if (proc.exitCode !== null) throw Error(`runtime exited: ${stderr}`);
      const event = context.events.find((event) => event.method === "turn/completed" && event.params.turn.id === turn);
      if (event) return event.params.turn;
      await delay(20);
    }
  })(), "turn/completed");
  const close = async () => {
    proc.stdin.end();
    await bound(new Promise((resolve) => proc.once("close", resolve)), "runtime drain", 15_000);
  };
  await call("initialize", { clientInfo: { name: "mira_reliability_test", version: "1" }, capabilities: { experimentalApi: true } });
  notify({ method: "initialized" });
  return { context, call, wait, close };
}

try {
  await admin.query(`CREATE DATABASE ${database}`); created = true;
  const connection = new URL(base); connection.pathname = `/${database}`;
  pool = new pg.Pool({ connectionString: connection.toString() });
  await initializeDatabase(pool);
  await new Promise((resolve) => fixture.listen(0, "127.0.0.1", resolve));
  for (const mode of ["retry", "denied"]) {
    const app = await client(mode);
    const started = await app.call("thread/start", { cwd: directory, approvalPolicy: "never", sandbox: "read-only",
      dynamicTools: [{ name: "reliability_probe", description: "Read-only test fixture", inputSchema: { type: "object", properties: {} } }],
    });
    const threadId = started.thread.id; app.context.thread = threadId;
    const turn = await app.call("turn/start", { threadId, input: [{ type: "text", text: "Call the read-only probe, then reply." }] });
    const done = await app.wait(turn.turn.id);
    assert.equal(done.status, mode === "retry" ? "completed" : "failed");
    assert.equal(app.context.toolCalls, 1, "a storage retry must not re-execute a tool");
    if (mode === "retry") {
      assert.equal(app.context.attempts.length, 3);
      assert.deepEqual(app.context.attempts, Array(3).fill(app.context.attempts[0]));
      const stored = await getThreadHistory(pool, app.context.store, threadId, null, null);
      assert.equal(stored.status, 200);
      const history = stored.body;
      assert.equal(history.items.filter((item) => item.payload?.type === "function_call_output" && item.payload?.call_id === "retry-probe-call").length, 1);
      const v1 = await getSnapshot(pool, app.context.store);
      assert.deepEqual(v1.snapshot.histories[threadId], history.items, "V1 snapshot must preserve V2 canonical history");
      await app.close();
      const resumed = await client("resume", false, app.context.store);
      // A fresh process/home must resume directly from PostgreSQL, without a local JSONL.
      const restored = await resumed.call("thread/resume", { threadId, cwd: directory });
      assert(restored.thread.turns.length > 0);
      await resumed.close();
    } else {
      assert.equal(app.context.attempts.length, 1);
      assert(app.context.events.some((event) => event.method === "error" && event.params.threadId === threadId && event.params.turnId === turn.turn.id));
      await app.close();
    }
    console.log(`PASS ${mode}: correct terminal state, durable acknowledgement and no tool replay`);
  }
  const malformed = await client("malformed", true);
  const started = await malformed.call("thread/resume", {
    threadId: crypto.randomUUID(), cwd: directory,
    history: [{ type: "custom_tool_call", call_id: "missing-output", name: "exec", input: "read-only fixture" }],
  });
  const turn = await malformed.call("turn/start", { threadId: started.thread.id, input: [{ type: "text", text: "Only reply OK." }] });
  assert.equal((await malformed.wait(turn.turn.id)).status, "failed");
  assert(malformed.context.events.some((event) => event.method === "error"));
  await malformed.close();
  console.log("PASS malformed history: no endless in-progress turn (debug panic or release model error)");
} finally {
  for (const child of children) {
    if (child.exitCode !== null) continue;
    child.kill("SIGTERM");
    await Promise.race([new Promise((resolve) => child.once("close", resolve)), delay(2000)]);
    if (child.exitCode === null) child.kill("SIGKILL");
  }
  fixture.closeAllConnections();
  await new Promise((resolve) => fixture.close(resolve));
  await pool?.end();
  if (created) await admin.query(`DROP DATABASE ${database} WITH (FORCE)`);
  await admin.end();
  await fs.rm(directory, { recursive: true, force: true });
}

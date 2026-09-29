import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { mkdtemp, mkdir, readFile, rm } from "node:fs/promises";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { query } from "@anthropic-ai/claude-agent-sdk";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import pg from "pg";
import { createMiraTools } from "./mira-tools.mjs";
import { ProbeSessionStore } from "./postgres-store.mjs";

const catalog = JSON.parse(await readFile(new URL("../../node/internal/agenttools/tools.json", import.meta.url)));
const databaseURL = process.env.MIRA_CLAUDE_TEST_DATABASE_URL;

async function listen(t, handler) {
  const server = http.createServer((request, response) => {
    Promise.resolve(handler(request, response)).catch(error => {
      response.writeHead(500, { "content-type": "application/json" });
      response.end(JSON.stringify({ error: error.message }));
    });
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise(resolve => { server.close(resolve); server.closeAllConnections(); }));
  return `http://127.0.0.1:${server.address().port}`;
}

async function jsonBody(request) {
  let body = "";
  for await (const chunk of request) body += chunk;
  return body ? JSON.parse(body) : {};
}

function json(response, body, status = 200) {
  response.writeHead(status, { "content-type": "application/json" });
  response.end(JSON.stringify(body));
}

async function fixtureTools(t) {
  const calls = [];
  const credential = "disposable-test-credential";
  const endpoint = await listen(t, async (request, response) => {
    assert.equal(request.headers.authorization, `Bearer ${credential}`);
    if (request.url === "/v1/agent-tools" && request.method === "GET") {
      return json(response, { namespace: "home_nodes", tools: catalog });
    }
    assert.equal(request.url, "/v1/agent-tools/call");
    const body = await jsonBody(request);
    calls.push(body);
    if (body.tool === "process") return json(response, { secret: "must not reach the model" }, 503);
    if (body.tool === "file") {
      response.writeHead(200, { "content-type": "application/json" });
      response.end("must not reach the model: invalid JSON");
      return;
    }
    const content = body.tool === "screen"
      ? [{ type: "image", data: "cG5n", mimeType: "image/png" }]
      : [{ type: "text", text: JSON.stringify({ nodes: [{ alias: "probe-node" }] }) }];
    json(response, { content, isError: false });
  });
  return { endpoint, credential, calls };
}

test("MCP bridge preserves schemas/images and never retries failed tool calls", async t => {
  const fixture = await fixtureTools(t);
  const tools = createMiraTools(fixture);
  const client = new Client({ name: "probe", version: "1" });
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  t.after(async () => { await client.close(); await tools.instance.close(); });
  await tools.instance.connect(serverTransport);
  await client.connect(clientTransport);
  assert.deepEqual((await client.listTools()).tools, catalog);
  const image = await client.callTool({ name: "screen", arguments: { nodeId: "probe", action: "screenshot" } });
  assert.deepEqual(image.content, [{ type: "image", data: "cG5n", mimeType: "image/png" }]);
  const failure = await client.callTool({ name: "process", arguments: { nodeId: "probe", action: "start", command: "unused" } });
  assert.equal(failure.isError, true);
  assert.match(failure.content[0].text, /HTTP 503/);
  assert.doesNotMatch(JSON.stringify(failure), /must not reach|disposable-test-credential/);
  assert.equal(fixture.calls.filter(call => call.tool === "process").length, 1);
  const malformed = await client.callTool({ name: "file", arguments: { nodeId: "probe", action: "read", path: "unused" } });
  assert.equal(malformed.isError, true);
  assert.doesNotMatch(JSON.stringify(malformed), /must not reach|invalid JSON/);
  assert.equal(fixture.calls.filter(call => call.tool === "file").length, 1);
});

async function storage(t) {
  const url = new URL(databaseURL);
  assert(["127.0.0.1", "localhost", "[::1]"].includes(url.hostname), "Use a disposable loopback PostgreSQL for this probe");
  const pool = new pg.Pool({ connectionString: databaseURL });
  const store = new ProbeSessionStore(pool, `claude_probe_${randomUUID().replaceAll("-", "")}`);
  t.after(async () => { try { await store.dispose(); } finally { await pool.end(); } });
  await store.initialize();
  return store;
}

test("PostgreSQL mirror preserves raw entries, retries, and subagent keys", { skip: !databaseURL }, async t => {
  const store = await storage(t);
  const key = { projectKey: "project", sessionId: randomUUID() };
  const first = { type: "user", uuid: randomUUID(), future: { text: "nul\u0000你好" } };
  const second = { type: "assistant", uuid: randomUUID(), unknown: [1, false] };
  assert.equal(await store.load(key), null);
  await store.append(key, [first]);
  await store.append(key, [first, second]);
  assert.deepEqual(await store.load(key), [first, second]);
  const metadata = { type: "custom-title", customTitle: "same metadata, separate writes" };
  await store.append(key, [metadata, metadata]);
  assert.deepEqual((await store.load(key)).slice(-2), [metadata, metadata]);
  const child = { ...key, subpath: "subagents/agent-probe" };
  await store.append(child, [first]);
  assert.deepEqual(await store.listSubkeys(key), [child.subpath]);
  assert.deepEqual(await store.load(child), [first]);
  assert.equal((await store.listSessions(key.projectKey))[0].sessionId, key.sessionId);
});

function respondModel(response, body, content) {
  const reason = content.some(block => block.type === "tool_use") ? "tool_use" : "end_turn";
  const message = { id: `msg_${randomUUID()}`, type: "message", role: "assistant", model: body.model,
    content, stop_reason: reason, stop_sequence: null, usage: { input_tokens: 10, output_tokens: 4 } };
  if (!body.stream) return json(response, message);
  response.writeHead(200, { "content-type": "text/event-stream" });
  const emit = event => response.write(`event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`);
  emit({ type: "message_start", message: { ...message, content: [], stop_reason: null } });
  for (const [index, block] of content.entries()) {
    emit({ type: "content_block_start", index, content_block: block.type === "text" ? { type: "text", text: "" } : { ...block, input: {} } });
    emit({ type: "content_block_delta", index, delta: block.type === "text"
      ? { type: "text_delta", text: block.text } : { type: "input_json_delta", partial_json: JSON.stringify(block.input) } });
    emit({ type: "content_block_stop", index });
  }
  emit({ type: "message_delta", delta: { stop_reason: reason, stop_sequence: null }, usage: { output_tokens: 4 } });
  emit({ type: "message_stop" });
  response.end();
}

// Real SDK and bundled Claude Code subprocess; only the model and Mira HTTP
// responses are fixtures. No real model request, login, or user workspace.
test("Claude SDK streams tools, mirrors to PostgreSQL, and resumes without local history", { skip: !databaseURL, timeout: 120_000 }, async t => {
  const store = await storage(t);
  const root = await mkdtemp(path.join(os.tmpdir(), "mira-claude-probe-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const cwd = path.join(root, "workspace");
  const homeA = path.join(root, "config-a");
  const homeB = path.join(root, "config-b");
  await Promise.all([mkdir(cwd), mkdir(homeA), mkdir(homeB)]);
  const fixture = await fixtureTools(t);
  const marker = `STORED_${randomUUID()}`;
  const requests = [];
  let resumed = false;
  const endpoint = await listen(t, async (request, response) => {
    const body = await jsonBody(request);
    if (request.url.includes("count_tokens")) return json(response, { input_tokens: 10 });
    if (!request.url.startsWith("/v1/messages")) return json(response, {});
    requests.push(body);
    if (resumed) {
      assert(JSON.stringify(body.messages).includes(marker), "restored context is missing");
      return respondModel(response, body, [{ type: "text", text: "RESTORED_OK" }]);
    }
    if (JSON.stringify(body.messages).includes("probe-node")) {
      return respondModel(response, body, [{ type: "text", text: marker }]);
    }
    assert(body.tools.some(tool => tool.name === "mcp__home_nodes__status"), "Mira tool missing from model request");
    respondModel(response, body, [{ type: "tool_use", id: "toolu_probe", name: "mcp__home_nodes__status", input: { action: "list" } }]);
  });
  const keys = new Map();
  const append = store.append.bind(store);
  store.append = async (key, entries) => { keys.set(JSON.stringify(key), key); await append(key, entries); };
  const options = config => ({
    cwd, model: "claude-sonnet-4-6", tools: [], allowedTools: ["mcp__home_nodes__status"],
    permissionMode: "dontAsk", settingSources: [], systemPrompt: "You are a protocol test assistant.",
    includePartialMessages: true, sessionStore: store, sessionStoreFlush: "eager", maxTurns: 4,
    mcpServers: { home_nodes: createMiraTools(fixture) },
    env: { PATH: process.env.PATH, CLAUDE_CONFIG_DIR: config,
      ANTHROPIC_API_KEY: "local-probe-only", ANTHROPIC_BASE_URL: endpoint,
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1", ENABLE_TOOL_SEARCH: "false" },
  });
  async function run(prompt, opts) {
    const events = [];
    const debugFile = path.join(root, `debug-${randomUUID()}.log`);
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 45_000);
    const stream = query({ prompt, options: { ...opts, debugFile, abortController: controller } });
    try {
      for await (const event of stream) events.push(event);
    } catch (error) {
      t.diagnostic(`SDK events: ${events.map(event => event.type + ":" + (event.subtype ?? "")).join(", ")}; model requests: ${requests.length}; tool calls: ${fixture.calls.length}`);
      if (process.env.MIRA_CLAUDE_PROBE_DEBUG === "1") t.diagnostic((await readFile(debugFile, "utf8").catch(() => "no debug log")).slice(-12000));
      throw error;
    } finally {
      clearTimeout(timer);
      stream.close();
      await opts.mcpServers.home_nodes.instance.close();
    }
    assert(events.some(event => event.type === "result" && event.subtype === "success"), "query did not succeed");
    return events;
  }
  const events = await run("List Mira nodes and remember the marker.", options(homeA));
  assert(events.some(event => event.type === "stream_event" && event.event.type === "content_block_delta"));
  const result = events.find(event => event.type === "result");
  const key = [...keys.values()].find(key => key.sessionId === result.session_id && !key.subpath);
  assert(key, "SDK did not mirror its transcript");
  const entries = await store.load(key);
  assert(JSON.stringify(entries).includes(marker), "final answer not mirrored");
  assert.equal(fixture.calls.filter(call => call.tool === "status").length, 1);
  // Simulate an execution Node with no copy of the previous local transcript.
  await rm(homeA, { recursive: true, force: true });
  resumed = true;
  const continued = await run("Continue the stored conversation.", { ...options(homeB), resume: result.session_id });
  assert.equal(continued.find(event => event.type === "result").session_id, result.session_id);
  assert(JSON.stringify(continued).includes("RESTORED_OK"));
  assert(requests.length >= 3);

  // Accepted Claude durability boundary: a mirror outage is observable, while
  // the underlying agent may finish successfully. Never label it fully saved.
  let attempts = 0;
  resumed = false;
  const degraded = await run("Exercise the unavailable mirror.", {
    ...options(homeB),
    sessionStore: { append: async () => { attempts++; throw new Error("probe storage unavailable"); }, load: async () => null },
  });
  assert(attempts >= 3, "expected bounded SDK mirror retries");
  assert(degraded.some(event => event.type === "system" && event.subtype === "mirror_error"), "mirror failure was silent");
});

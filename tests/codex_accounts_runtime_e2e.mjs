// Disposable Mira Server only. Real Node/App Server, synthetic Responses provider;
// no production credentials, external model requests or user conversations.
import assert from "node:assert/strict";
import http from "node:http";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { adminRequest, approvePendingNode, loginAdmin } from "./auth_helpers.mjs";

const origin = process.env.MIRA_SERVER_URL;
const mira = process.env.MIRA_NODE_TEST_BINARY, codex = process.env.CODEX_TEST_BINARY;
assert(origin && mira && codex, "Set MIRA_SERVER_URL, MIRA_NODE_TEST_BINARY and CODEX_TEST_BINARY");
assert.equal(new URL(origin).hostname, "127.0.0.1", "Use a disposable loopback Server");
const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-accounts-runtime-"));
const identity = path.join(temporary, "identity.json"), defaultHome = path.join(temporary, "default");
await fs.mkdir(defaultHome);
const quote = value => "'" + value.replaceAll("'", "'\\''") + "'";
const runtimeLog = path.join(temporary, "runtime.log"), wrapper = path.join(temporary, "codex-test");
await fs.writeFile(wrapper, `#!/bin/sh\nRUST_LOG=warn exec ${quote(codex)} "$@" 2>>${quote(runtimeLog)}\n`, { mode: 0o700 });
const nodeKey = `accounts-runtime-${randomUUID()}`, store = `accounts-runtime-${randomUUID()}`;
const session = await loginAdmin(origin);
const admin = (url, body, method = "POST") => adminRequest(origin, session, url,
  body === undefined ? {} : { method, body: JSON.stringify(body) });
let nodeProcess, nodeId, token, rejectOld = false;
let holdNextResponse = false, releaseResponse;
const requests = [], sockets = [], logs = [];
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
async function waitFor(read, description, timeout = 60_000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (nodeProcess?.exitCode !== null && nodeProcess?.exitCode !== undefined) throw Error("Node exited");
    const value = await read(); if (value) return value;
    await delay(150);
  }
  throw Error(`Timed out: ${description}`);
}
const encrypted = items => items.filter(item => item.encrypted_content);
const isChildFollowup = body => body.input.some(item => item.type === "agent_message" &&
  item.recipient === "/root/account_child" && JSON.stringify(item.content).includes("ACCOUNT_CHILD_SECOND"));
const mock = http.createServer(async (request, response) => {
  const parts = []; for await (const part of request) parts.push(part);
  if (request.method !== "POST" || !request.url.endsWith("/responses")) {
    response.writeHead(404); response.end(); return;
  }
  const body = JSON.parse(Buffer.concat(parts));
  const key = request.headers.authorization;
  requests.push({ key, body });
  if (rejectOld && encrypted(body.input).some(item => item.encrypted_content.startsWith("old-"))) {
    response.writeHead(400, { "content-type": "application/json" });
    response.end(JSON.stringify({ error: { message: "The encrypted content could not be verified. Encrypted content could not be decrypted or parsed.",
      type: "invalid_request_error", code: "invalid_encrypted_content", param: null } })); return;
  }
  const n = requests.length, id = `resp-${n}`;
  const latestUser = body.input.filter(item => item.role === "user").at(-1);
  const prompt = JSON.stringify(latestUser?.content ?? "");
  const spawnChild = prompt.includes("SPAWN_ACCOUNT_CHILD") && !body.input.some(item => item.call_id === "spawn-account-child");
  const followChild = prompt.includes("FOLLOWUP_ACCOUNT_CHILD") && !body.input.some(item => item.call_id === "follow-account-child");
  const items = spawnChild || followChild ? [{ type: "function_call", namespace: "collaboration",
    name: spawnChild ? "spawn_agent" : "followup_task", call_id: spawnChild ? "spawn-account-child" : "follow-account-child",
    arguments: JSON.stringify(spawnChild ? { task_name: "account_child", message: "ACCOUNT_CHILD_FIRST", fork_turns: "none" }
      : { target: "/root/account_child", message: "ACCOUNT_CHILD_SECOND" }) }] : [
    { type: "reasoning", id: `rs-${n}`, summary: [], encrypted_content: n === 1 ? "old-account-A" : "new-account-B" },
    { type: "message", id: `msg-${n}`, role: "assistant", content: [{ type: "output_text", text: isChildFollowup(body) ? "CHILD_FOLLOWUP_OK" : `ACCOUNTS_OK_${n}` }] },
  ];
  const events = [{ type: "response.created", response: { id } },
    ...items.map(item => ({ type: "response.output_item.done", item })),
    { type: "response.completed", response: { id, usage: { input_tokens: 10, output_tokens: 10, total_tokens: 20 } } }];
  response.writeHead(200, { "content-type": "text/event-stream", connection: "close" });
  const chunks = events.map(event => `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`);
  if (holdNextResponse) {
    holdNextResponse = false;
    response.write(chunks.shift());
    releaseResponse = () => response.end(chunks.join(""));
  } else {
    response.end(chunks.join(""));
  }
});
await new Promise(resolve => mock.listen(0, "127.0.0.1", resolve));

async function connect(binding, selectedStore = store) {
  const socket = new WebSocket(`${origin.replace(/^http/, "ws")}/v1/nodes/${nodeId}/app-server?storeId=${selectedStore}&nodeAccountId=${binding}`,
    ["mira-client-v1", `auth.${Buffer.from(token).toString("base64url")}`]);
  sockets.push(socket);
  let next = 1;
  const pending = new Map(), events = [];
  socket.addEventListener("message", ({ data }) => {
    const message = JSON.parse(data), request = pending.get(message.id);
    if (!request) { events.push(message); return; }
    pending.delete(message.id); clearTimeout(request.timer);
    if (message.error) request.reject(Error(`${request.method}: ${JSON.stringify(message.error)}`)); else request.resolve(message.result);
  });
  socket.addEventListener("close", () => {
    for (const p of pending.values()) { clearTimeout(p.timer); p.reject(Error("App Server closed")); }
    pending.clear();
  });
  await new Promise((resolve, reject) => { socket.addEventListener("open", resolve, { once: true }); socket.addEventListener("error", reject, { once: true }); });
  const client = { socket, events, call(method, params) {
    const id = next++;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { pending.delete(id); reject(Error(`RPC timed out: ${method}`)); }, 60_000);
      pending.set(id, { resolve, reject, timer, method }); socket.send(JSON.stringify({ id, method, params }));
    });
  } };
  await client.call("initialize", { clientInfo: { name: "mira_account_runtime_test", version: "1" }, capabilities: { experimentalApi: true } });
  socket.send(JSON.stringify({ method: "initialized" }));
  return client;
}
async function turn(client, threadId, text, expected = "completed") {
  const { turn } = await client.call("turn/start", { threadId, input: [{ type: "text", text }], effort: "medium" });
  const event = await waitFor(() => client.events.find(event => event.method === "turn/completed" && event.params.turn.id === turn.id), "turn completion");
  assert.equal(event.params.turn.status, expected, JSON.stringify(event));
}
const account = id => admin(`/v1/nodes/${nodeId}/codex-accounts/${id}`);
const running = id => waitFor(async () => {
  const a = await account(id); return a.reportedAppServer.status === "running" && a;
}, "account running");
async function history(threadId) {
  return admin(`/v2/stores/${store}/threads/${threadId}/history`);
}

try {
  nodeProcess = spawn(mira, ["node-worker"], { cwd: temporary, stdio: ["ignore", "pipe", "pipe"], env: {
    ...process.env, MIRA_SERVER_URL: origin, MIRA_NODE_KEY: nodeKey, MIRA_IDENTITY_FILE: identity,
    CODEX_BINARY: wrapper, CODEX_HOME: defaultHome, APP_SERVER_CODEX_HOME: defaultHome,
    APP_SERVER_AUTO_START: "false", APP_SERVER_CONFIG_OVERRIDES: "[]", MIRA_NODE_HEARTBEAT_SECONDS: "1",
    MIRA_NODE_ALLOWED_ROOTS: JSON.stringify([temporary]), MIRA_NODE_TOKEN: "", CONTROL_SERVER_TOKEN: "",
    // Unrelated process credentials must not leak into an additional profile.
    UNRELATED_API_SECRET: "synthetic-unrelated-secret",
  } });
  for (const stream of [nodeProcess.stdout, nodeProcess.stderr]) stream.on("data", data => {
    logs.push(data.toString()); if (logs.length > 100) logs.shift();
  });
  await approvePendingNode(origin, session, nodeKey);
  const online = await waitFor(async () => (await admin("/v1/nodes")).data.find(n => n.nodeKey === nodeKey && n.channelStatus?.connected), "Node enrollment");
  nodeId = online.nodeId; token = JSON.parse(await fs.readFile(identity, "utf8")).token;
  console.log("Runtime fixture Node connected");
  const bindings = [];
  for (const name of ["A", "B"]) {
    const { nodeAccountId: id } = await admin(`/v1/nodes/${nodeId}/codex-accounts`, { name, provider: "fixture", authType: "providerConfig" });
    bindings.push(id);
    await waitFor(async () => (await account(id)).reportedAppServer.status === "stopped", "stopped profile");
    await admin(`/v1/nodes/${nodeId}/codex-accounts/${id}/configure`, {
      provider: { id: "fixture", name: "Fixture", baseUrl: `http://127.0.0.1:${mock.address().port}/v1`, envKey: "FIXTURE_KEY" }, apiKey: `synthetic-${name}`,
    });
    await admin(`/v1/codex/runtimes/${nodeId}/start`, { nodeAccountId: id, storeId: store });
    await running(id);
    console.log(`Account ${name} running`);
    assert.equal((await admin(`/v1/nodes/${nodeId}/codex-accounts/${id}/quota`)).quotaSupported, false);
  }
  const [a, b] = bindings;
  const first = await connect(a);
  const threadId = (await first.call("thread/start", { cwd: temporary, model: "gpt-5.1-codex", approvalPolicy: "never", sandbox: "danger-full-access" })).thread.id;
  await turn(first, threadId, "Initial request");
  console.log("Initial account turn completed");
  assert.equal(requests.at(-1).key, "Bearer synthetic-A");
  await waitFor(async () => JSON.stringify((await history(threadId)).items).includes("old-account-A"), "encrypted reasoning persisted");
  // Handoff drains A, then B must receive the original encrypted input unchanged.
  let second = await connect(b);
  await second.call("thread/resume", { threadId, model: "gpt-5.1-codex", cwd: temporary });
  await turn(second, threadId, "Compatible account switch");
  console.log("Compatible account handoff completed");
  assert.equal(requests.at(-1).key, "Bearer synthetic-B");
  assert(encrypted(requests.at(-1).body.input).some(item => item.encrypted_content === "old-account-A"));

  // Match the Web composer: native turn/start appends input to the active turn.
  // Hold model sampling open so all submissions happen before turn completion.
  const steeringId = (await second.call("thread/start", { cwd: temporary, model: "gpt-5.1-codex" })).thread.id;
  holdNextResponse = true;
  const active = (await second.call("turn/start", { threadId: steeringId, input: [{ type: "text", text: "STEERING_INITIAL" }] })).turn;
  await waitFor(() => releaseResponse && second.events.some(event => event.method === "turn/started" && event.params.turn.id === active.id), "active model response");
  for (const text of ["STEERING_APPEND_ONE", "STEERING_APPEND_TWO"]) {
    const appended = await second.call("turn/start", { threadId: steeringId, input: [{ type: "text", text }] });
    assert.equal(appended.turn.id, active.id, "follow-up must keep the active turn ID");
  }
  const reconnected = await connect(b);
  await reconnected.call("thread/resume", { threadId: steeringId });
  const appended = await reconnected.call("turn/start", { threadId: steeringId, input: [{ type: "text", text: "STEERING_RECONNECTED" }] });
  assert.equal(appended.turn.id, active.id);
  await assert.rejects(reconnected.call("turn/steer", { threadId: steeringId, expectedTurnId: randomUUID(),
    input: [{ type: "text", text: "STEERING_WRONG_TURN" }] }), /expected|mismatch/i);
  const steered = await reconnected.call("turn/steer", { threadId: steeringId, expectedTurnId: active.id,
    input: [{ type: "text", text: "STEERING_EXPLICIT" }] });
  assert.equal(steered.turnId, active.id);
  const otherAccount = await connect(a);
  await assert.rejects(otherAccount.call("turn/start", { threadId: steeringId, input: [{ type: "text", text: "STEERING_WRONG_ACCOUNT" }] }), /切换到其他账号/);
  await assert.rejects(otherAccount.call("thread/resume", { threadId: steeringId }), /仍有对话|busy/i);
  releaseResponse(); releaseResponse = undefined;
  await waitFor(() => second.events.some(event => event.method === "turn/completed" && event.params.turn.id === active.id && event.params.turn.status === "completed"), "steered turn completion");
  const steeredInput = JSON.stringify(requests.at(-1).body.input);
  for (const text of ["STEERING_APPEND_ONE", "STEERING_APPEND_TWO", "STEERING_RECONNECTED", "STEERING_EXPLICIT"]) assert(steeredInput.includes(text), `${text} must reach model input`);
  for (const text of ["STEERING_WRONG_TURN", "STEERING_WRONG_ACCOUNT"]) assert(!steeredInput.includes(text));
  assert.equal(second.events.filter(event => event.method === "turn/started" && event.params.turn.id === active.id).length, 1);
  await turn(reconnected, steeringId, "Next turn after steering completes");
  console.log("Active-turn steering passed: repeated input, reconnect, explicit steer, account isolation and completion");

  const compaction = process.env.MIRA_ACCOUNTS_TEST_COMPACTION === "1";
  if (compaction) {
    await admin(`/v1/codex/runtimes/${nodeId}/stop`, { nodeAccountId: b });
    await waitFor(async () => (await account(b)).reportedAppServer.status === "stopped", "stopped before canonical compaction fixture");
    const head = await admin(`/v2/stores/${store}?threadId=${threadId}`), manifest = head.historyManifest[threadId];
    await adminRequest(origin, session, `/v2/stores/${store}/commits`, { method: "POST", headers: { "X-Codex-Operation-Id": randomUUID() }, body: JSON.stringify({
      expectedVersion: head.version, stateChanges: [], historyChanges: [{ threadId, mode: "append", expectedGeneration: manifest.generation, expectedItemCount: manifest.itemCount,
        items: [{ type: "compacted", payload: { message: "", replacement_history: [{ type: "compaction", encrypted_content: "old-compaction" }] } }] }],
    }) });
    await admin(`/v1/codex/runtimes/${nodeId}/start`, { nodeAccountId: b, storeId: store }); await running(b);
    second = await connect(b); await second.call("thread/resume", { threadId, cwd: temporary, model: "gpt-5.1-codex" });
  }
  rejectOld = true;
  await turn(second, threadId, "Incompatible encrypted context", "failed");
  const endpoint = `/v1/codex/threads/${threadId}/input-recovery?storeId=${store}&nodeAccountId=${b}`;
  const plan = await waitFor(async () => { try { return await admin(endpoint); } catch { return null; } }, "recovery evidence");
  assert(plan.recoverable); assert.equal(plan.policy, compaction ? "rebuildContext" : "omitReasoning");
  const canonical = (await history(threadId)).items;
  const requestCount = requests.length;
  const decision = { confirm: true, decisionId: randomUUID(), failureId: plan.failureId, generation: plan.generation, itemCount: plan.itemCount };
  const confirmed = await admin(endpoint, decision);
  assert.equal(confirmed.canonicalHistoryUnchanged, true);
  assert.equal((await admin(endpoint, decision)).duplicate, true, "lost acknowledgement must be replayable");
  assert.equal(requests.length, requestCount, "consent must never retry a model request");
  assert.deepEqual((await history(threadId)).items.slice(0, canonical.length), canonical);
  await waitFor(async () => {
    const current = await account(b);
    return current.reportedAppServer.status === "running" && current.reportedAppServer.runtimeId !== confirmed.retiredRuntimeId;
  }, "new account process after retirement");
  const recovered = await connect(b);
  await recovered.call("thread/resume", { threadId, model: "gpt-5.1-codex", cwd: temporary });
  await turn(recovered, threadId, "Continue after explicit consent");
  assert(!encrypted(requests.at(-1).body.input).some(item => item.encrypted_content.startsWith("old-")));
  await turn(recovered, threadId, "Keep newly generated reasoning");
  assert(encrypted(requests.at(-1).body.input).some(item => item.encrypted_content === "new-account-B"));
  assert.deepEqual((await history(threadId)).items.slice(0, canonical.length), canonical);
  const forkId = (await recovered.call("thread/fork", { threadId, cwd: temporary, excludeTurns: true, deferGoalContinuation: true })).thread.id;
  assert(JSON.stringify((await history(forkId)).items).includes("old-account-A"), "fork must copy canonical history");
  const parentClient = await connect(a);
  console.log("Testing persisted subagent handoff");
  const parentId = (await parentClient.call("thread/start", { cwd: temporary, model: "gpt-5.1-codex", config: { "features.multi_agent_v2": true }, approvalPolicy: "never", sandbox: "danger-full-access" })).thread.id;
  await turn(parentClient, parentId, "SPAWN_ACCOUNT_CHILD");
  const childId = await waitFor(async () => {
    const state = (await admin(`/v2/stores/${store}`)).state;
    return Object.entries(state.created_threads).find(([, value]) => value.parent_thread_id === parentId)?.[0];
  }, "persisted child thread");
  await waitFor(async () => JSON.stringify((await history(childId)).items).includes("ACCOUNTS_OK_"), "child completion");
  // Persisted assistant output precedes turn completion. Wait for both live
  // statuses so the test does not request handoff while a child is still active.
  await waitFor(async () => {
    for (const id of [parentId, childId]) {
      const { thread } = await parentClient.call("thread/read", { threadId: id, includeTurns: false });
      if (!["idle", "notLoaded"].includes(thread.status.type)) return false;
    }
    return true;
  }, "idle parent and child before handoff");
  const parentOnB = await connect(b);
  await parentOnB.call("thread/resume", { threadId: parentId, cwd: temporary, model: "gpt-5.1-codex", config: { "features.multi_agent_v2": true } });
  console.log("Parent resumed on B");
  // Upstream requires explicit parent-then-child loading for persisted V2
  // children; followup_task only accepts a live child path.
  await parentOnB.call("thread/resume", { threadId: childId, excludeTurns: true });
  console.log("Child resumed on B");
  await turn(parentOnB, parentId, "FOLLOWUP_ACCOUNT_CHILD");
  await waitFor(async () => JSON.stringify((await history(childId)).items).includes("CHILD_FOLLOWUP_OK"), "child followup after account handoff");
  assert.equal(requests.findLast(request => isChildFollowup(request.body)).key, "Bearer synthetic-B");
  console.log("Subagent identity and followup survived parent account handoff");
  const cliStore = `${store}-cli`, cliHome = path.join(temporary, "cli"); await fs.mkdir(cliHome);
  await fs.writeFile(path.join(cliHome, "config.toml"), [
    'model="gpt-5.1-codex"', 'model_provider="fixture"', '[model_providers.fixture]', 'name="Fixture"',
    `base_url="http://127.0.0.1:${mock.address().port}/v1"`, 'wire_api="responses"', 'env_key="FIXTURE_KEY"',
    '[experimental_thread_store]', 'type="remote_http"', `endpoint="${origin}"`, `store_id="${cliStore}"`,
  ].join("\n"));
  const cli = spawn(codex, ["exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "-C", temporary, "CLI_ACCOUNT_SHARED_HISTORY"], {
    env: { ...process.env, CODEX_HOME: cliHome, FIXTURE_KEY: "synthetic-CLI", MIRA_NODE_TOKEN: token, MIRA_NODE_CODEX_ACCOUNT_ID: "", MIRA_NODE_CODEX_RUNTIME_ID: "" },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let cliOutput = "", cliError = "";
  cli.stdout.on("data", data => { cliOutput += data; }); cli.stderr.on("data", data => { cliError = (cliError + data).slice(-2000); });
  const timer = setTimeout(() => cli.kill("SIGTERM"), 60_000);
  const code = await new Promise(resolve => cli.once("exit", resolve)); clearTimeout(timer);
  assert.equal(code, 0, cliError);
  const cliThread = cliOutput.split("\n").filter(Boolean).map(line => JSON.parse(line)).find(event => event.type === "thread.started").thread_id;
  const oldRuntime = (await account(b)).reportedAppServer.runtimeId;
  await admin(`/v1/codex/runtimes/${nodeId}/start`, { nodeAccountId: b, storeId: cliStore });
  await waitFor(async () => { const current = await account(b); return current.reportedAppServer.status === "running" && current.reportedAppServer.runtimeId !== oldRuntime; }, "CLI store runtime");
  const cliReader = await connect(b, cliStore);
  await cliReader.call("thread/resume", { threadId: cliThread, cwd: temporary, model: "gpt-5.1-codex" });
  await turn(cliReader, cliThread, "Resume CLI history in App Server");
  assert(JSON.stringify(requests.at(-1).body.input).includes("CLI_ACCOUNT_SHARED_HISTORY"));
  console.log("CLI creation and App Server resume share canonical PostgreSQL history");
  console.log("Account runtime E2E passed: isolated keys, live handoff, compatible reasoning, provider error, consent replay, input projection, canonical fork");
} catch (error) {
  console.error(error.stack, logs.join("").slice(-1500), (await fs.readFile(runtimeLog, "utf8").catch(() => "")).slice(-5000)); process.exitCode = 1;
} finally {
  releaseResponse?.();
  for (const socket of sockets) socket.close();
  if (nodeProcess?.exitCode === null) {
    const exited = new Promise(resolve => nodeProcess.once("exit", resolve)); nodeProcess.kill("SIGTERM"); await exited;
  }
  if (nodeId) await admin(`/v1/admin/nodes/${nodeId}/revoke`, {}).catch(() => {});
  await new Promise(resolve => mock.close(resolve));
  await fs.rm(temporary, { recursive: true, force: true });
}

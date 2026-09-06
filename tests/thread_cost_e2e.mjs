import assert from "node:assert/strict";
import crypto from "node:crypto";
import pg from "../server/node_modules/pg/lib/index.js";
import { putSnapshot, getSnapshot, getStoreHead, commitDelta, rebuildSnapshot } from "../server/thread-store.mjs";
import { listImportedThreads } from "../server/codex-session-import.mjs";
import { getThreadCostEstimate, getThreadTurnCostEstimates } from "../server/thread-cost-estimate.mjs";

if (!process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL) throw new Error("a disposable database is required");
const pool = new pg.Pool({ connectionString: process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL });
const store = `costs-${crypto.randomUUID()}`, id = crypto.randomUUID(), child = crypto.randomUUID();
const fork = crypto.randomUUID(), missingBoundary = crypto.randomUUID();
const headers = () => ({ "x-codex-operation-id": crypto.randomUUID() });
const usage = (input, cached, output) => ({ input_tokens: input, cached_input_tokens: cached, output_tokens: output });
const event = (total, last = total) => ({ type: "event_msg", payload: { type: "token_count", info: { total_token_usage: total, last_token_usage: last }, future: "raw\u0000field" } });
const context = (model, turnId = null) => ({ type: "turn_context", payload: { model, ...(turnId ? {turn_id: turnId} : {}) } });
const message = (turnId, text) => ({type:"event_msg",payload:{type:"agent_message",turn_id:turnId,message:text}});
const settings = (threadId, model) => ({ type: "event_msg", payload: { type: "thread_settings_applied",
  thread_id: threadId, thread_settings: { model } } });
const first = event(usage(100000, 80000, 1000));
const history = [context("gpt-6-astra", "turn-a"), ...Array.from({ length: 520 }, () => first), message("turn-a", "First priced turn"),
  context("gpt-5.6-sol", "turn-b"), event(usage(150000, 120000, 1500), usage(50000, 40000, 500)),
  message("turn-b", "Second priced turn"), { type: "future_tool", payload: { type: "token_count", info: first.payload.info } }];
let queryCount = 0;
const countedPool = { query: (...args) => { queryCount++; return pool.query(...args); } };
const read = async threadId => (await listImportedThreads(pool, store, 1, threadId))[0];
try {
  assert.equal((await putSnapshot(pool, store, { expectedVersion: 0, snapshot: {
    created_threads: { [child]: { source: "subagent", parent_thread_id: id },
      [fork]: { source: "cli", forked_from_id: id }, [missingBoundary]: { source: "cli", forked_from_id: id } },
    metadata_updates: { [id]: { token_usage: usage(150000, 120000, 1500) },
      [fork]: { token_usage: usage(200000, 160000, 2000) },
      [missingBoundary]: { token_usage: usage(150000, 120000, 1500) } },
    histories: { [id]: history, [child]: [context("gpt-5.6-luna"), first],
      [fork]: [...history, settings(fork, "gpt-5.6-sol"), context("gpt-5.6-sol", "fork-turn"),
        event(usage(200000, 160000, 2000), usage(50000, 40000, 500))],
      [missingBoundary]: history },
  } }, headers())).status, 200);
  let thread = await read(id);
  assert.equal(thread.model, "gpt-5.6-sol", "latest recorded model is exposed in details and lists");
  const before = await getStoreHead(pool, store);
  const estimates = await Promise.all([getThreadCostEstimate(countedPool, store, thread), getThreadCostEstimate(countedPool, store, thread)]);
  assert.equal(queryCount, 3, "concurrent readers share the same bounded history scan");
  assert.deepEqual(estimates[0], estimates[1]);
  assert.equal(estimates[0].amount, 0.396); assert.equal(estimates[0].pricedRequests, 2);
  assert.equal(estimates[0].status, "complete");
  const turnCosts = await getThreadTurnCostEstimates(countedPool, store, thread);
  assert.equal(turnCosts["turn-a"].amount, 0.33); assert.equal(turnCosts["turn-b"].amount, 0.066);
  assert.equal(queryCount, 3, "thread and turn estimates share one cached cost projection");
  assert.equal((await getThreadCostEstimate(pool, store, await read(child))).amount, 0.0068, "subagent cost stays independent");
  const forkThread = await read(fork);
  assert.equal(forkThread.forkedFromId, id, "fork identity is exposed without treating it as a subagent");
  const forkCost = await getThreadCostEstimate(pool, store, forkThread);
  assert.equal(forkCost.amount, 0.066, "copied parent requests are excluded from fork cost");
  assert.equal(forkCost.scope, "fork"); assert.deepEqual(forkCost.models, ["gpt-5.6-sol"]);
  assert.deepEqual(Object.keys(await getThreadTurnCostEstimates(pool, store, forkThread)), ["fork-turn"],
    "fork turn costs exclude copied parent turns");
  const missingForkCost = await getThreadCostEstimate(pool, store, await read(missingBoundary));
  assert.equal(missingForkCost.amount, null); assert.equal(missingForkCost.status, "unavailable");
  assert.deepEqual(missingForkCost.reasons, ["fork_boundary_missing"]);
  const cachedQueries = queryCount;
  await getThreadCostEstimate(countedPool, store, thread); assert.equal(queryCount, cachedQueries);
  assert.deepEqual(await getStoreHead(pool, store), before, "calculating costs never writes history");
  assert.equal((await commitDelta(pool, store, { expectedVersion: before.version, stateChanges: [{
    path: ["metadata_updates", id, "token_usage"], mode: "set", conflictPolicy: "lastWriteWins", expected: { exists: true, value: usage(150000, 120000, 1500) }, value: usage(200000, 160000, 2000),
  }], historyChanges: [] }, headers())).status, 200);
  thread = await read(id);
  assert.equal((await getThreadCostEstimate(countedPool, store, thread)).status, "partial", "metadata ahead of history is not reported as complete");
  const head = await getStoreHead(pool, store);
  assert.equal((await commitDelta(pool, store, { expectedVersion: head.version, stateChanges: [], historyChanges: [{ threadId: id, mode: "append", expectedGeneration: thread.generation, expectedItemCount: thread.itemCount, items: [event(usage(200000, 160000, 2000), usage(50000, 40000, 500))] }] }, headers())).status, 200);
  thread = await read(id);
  const updated = await getThreadCostEstimate(countedPool, store, thread);
  assert.equal(updated.amount, 0.462); assert.equal(updated.status, "complete");
  assert.equal(queryCount, cachedQueries + 1, "only newly appended events are scanned");
  await rebuildSnapshot(pool, store);
  assert.deepEqual(await getThreadCostEstimate(pool, store, await read(id)), updated);
  const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
  const login = await fetch(`${origin}/v1/admin/login`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ username: "admin", password: process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password" }) });
  const cookie = login.headers.getSetCookie().map(value => value.split(";")[0]).join("; ");
  const endpoint = `${origin}/v1/codex/threads/${id}?storeId=${store}`;
  assert.equal((await fetch(endpoint + "&includeCost=1")).status, 401);
  assert.equal((await (await fetch(endpoint, { headers: { cookie } })).json()).costEstimate, undefined, "ordinary reads don't scan cost history");
  const response = await fetch(endpoint + "&includeCost=1", { headers: { cookie } });
  assert.equal(response.status, 200); assert.deepEqual((await response.json()).costEstimate, updated);
  const transcript = await (await fetch(`${origin}/v1/codex/threads/${id}/transcript?storeId=${store}&tail=1&includeCost=1`, {headers:{cookie}})).json();
  const pricedTurn = transcript.trace.find(item => item.turnId === "turn-b" && item.kind === "assistant");
  assert.equal(pricedTurn.turnCostEstimate.amount, 0.132, "transcript pages attach only their assistant turn's estimate");
  const previous = await getSnapshot(pool, store);
  previous.snapshot.histories[id] = [context("gpt-6-astra"), event(usage(0, 0, 0))];
  previous.snapshot.metadata_updates[id].token_usage = usage(0, 0, 0);
  assert.equal((await putSnapshot(pool, store, { expectedVersion: previous.version, snapshot: previous.snapshot }, headers())).status, 200);
  assert.equal((await read(id)).model, "gpt-6-astra");
  assert.equal((await getThreadCostEstimate(countedPool, store, await read(id))).amount, 0, "new generations discard the previous cost projection");
  console.log("PASS: paginated/deduplicated costs, model changes, copied-fork boundary, raw NUL, subagents, shared/incremental cache, metadata lag, rebuild/generations and opt-in authenticated API");
} finally { await pool.end(); }

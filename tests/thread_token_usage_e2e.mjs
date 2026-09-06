import assert from "node:assert/strict";
import crypto from "node:crypto";
import pg from "../server/node_modules/pg/lib/index.js";
import { putSnapshot, getSnapshot, getStoreHead, commitDelta, rebuildSnapshot } from "../server/thread-store.mjs";
import { listImportedThreads } from "../server/codex-session-import.mjs";

const databaseUrl = process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL;
if (!databaseUrl) throw new Error("a disposable Server database is required");
const pool = new pg.Pool({ connectionString: databaseUrl });
const store = `tokens-${crypto.randomUUID()}`;
const ids = Object.fromEntries(["native", "imported", "child", "unknown"].map(key => [key, crypto.randomUUID()]));
const usage = (input, cached, output) => ({ input_tokens: input, cached_input_tokens: cached, output_tokens: output });
const event = total => ({ type: "event_msg", payload: { type: "token_count", info: { total_token_usage: total,
  last_token_usage: usage(9, 8, 1) }, future: "raw\u0000field" } });
const headers = () => ({ "x-codex-operation-id": crypto.randomUUID() });
const read = async () => new Map((await listImportedThreads(pool, store)).map(thread => [thread.threadId, thread]));
try {
  const snapshot = { metadata_updates: { [ids.native]: { token_usage: usage(125000, 100000, 8000) } },
    created_threads: { [ids.child]: { source: "subagent", parent_thread_id: ids.native } }, histories: {
      [ids.native]: [event(usage(1000, 800, 50))],
      [ids.imported]: [event(usage(100, 80, 10)), event(usage(200, 160, 20)), event(usage(200, 160, 20)),
        { type: "future_tool", payload: { type: "token_count", info: { total_token_usage: usage(9999, 9999, 9999) } } },
        { type: "event_msg", payload: { type: "token_count", info: null } }],
      [ids.child]: [event(usage(20, 10, 5))], [ids.unknown]: [{ type: "event_msg", payload: { type: "user_message", message: "hello" } }],
    } };
  const initial = await putSnapshot(pool, store, { expectedVersion: 0, snapshot }, headers());
  assert.equal(initial.status, 200, JSON.stringify(initial.body));
  let rows = await read();
  assert.deepEqual(rows.get(ids.native).tokenUsage, { inputTokens: 125000, cachedInputTokens: 100000, outputTokens: 8000 });
  assert.deepEqual(rows.get(ids.imported).tokenUsage, { inputTokens: 200, cachedInputTokens: 160, outputTokens: 20 }, "read the last cumulative snapshot without summing repeated snapshots or unrelated nested payloads");
  assert.deepEqual(rows.get(ids.child).tokenUsage, { inputTokens: 20, cachedInputTokens: 10, outputTokens: 5 });
  assert.equal(rows.get(ids.child).parentThreadId, ids.native);
  assert.equal(rows.get(ids.unknown).tokenUsage, null);
  const head = await getStoreHead(pool, store);
  assert.equal((await commitDelta(pool, store, { expectedVersion: head.version, stateChanges: [{
    path: ["metadata_updates", ids.native, "token_usage"], mode: "set", conflictPolicy: "lastWriteWins", expected: { exists: true, value: usage(125000, 100000, 8000) }, value: usage(250000, 200000, 16000),
  }], historyChanges: [{ threadId: ids.imported, mode: "append", expectedGeneration: 1, expectedItemCount: 5,
    items: [event(usage(300, 250, 30))] }] }, headers())).status, 200);
  rows = await read();
  assert.equal(rows.get(ids.native).tokenUsage.inputTokens, 250000, "metadata-only updates bypass immutable-history cache");
  assert.equal(rows.get(ids.imported).tokenUsage.inputTokens, 300, "appended history invalidates the fallback cache");
  const beforeRead = await getStoreHead(pool, store);
  await read(); assert.deepEqual(await getStoreHead(pool, store), beforeRead, "usage reads never mutate canonical history or recency");
  await rebuildSnapshot(pool, store);
  assert.deepEqual((await read()).get(ids.imported).tokenUsage, rows.get(ids.imported).tokenUsage);
  const previous = await getSnapshot(pool, store);
  previous.snapshot.histories[ids.imported] = [event(usage(0, 0, 0))];
  assert.equal((await putSnapshot(pool, store, { expectedVersion: previous.version, snapshot: previous.snapshot }, headers())).status, 200);
  assert.deepEqual((await read()).get(ids.imported).tokenUsage, { inputTokens: 0, cachedInputTokens: 0, outputTokens: 0 }, "a replacement generation must not reuse the old counters");
  const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8787";
  const login = await fetch(`${origin}/v1/admin/login`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ username: "admin", password: process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password" }) });
  assert.equal(login.status, 200);
  const cookie = login.headers.getSetCookie().map(value => value.split(";")[0]).join("; ");
  const response = await fetch(`${origin}/v1/codex/threads/${ids.native}?storeId=${store}`, { headers: { cookie } });
  assert.equal(response.status, 200);
  assert.equal((await response.json()).tokenUsage.inputTokens, 250000);
  assert.equal((await fetch(`${origin}/v1/codex/threads/${ids.native}?storeId=${store}`)).status, 401);
  console.log("PASS: native/imported cumulative usage, zero/unknown, cached-input subset, subagent isolation, metadata updates, canonical generations/rebuild and authenticated API");
} finally { await pool.end(); }

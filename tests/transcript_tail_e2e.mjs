// Use a disposable PostgreSQL database, never the production store.
// MIRA_TRANSCRIPT_TEST_DATABASE_URL=postgresql://... node tests/transcript_tail_e2e.mjs
import assert from "node:assert/strict";
import crypto from "node:crypto";
import pg from "../server/node_modules/pg/lib/index.js";
import { initializeDatabase } from "../server/db.mjs";
import { putSnapshot, commitDelta, getSnapshot } from "../server/thread-store.mjs";
import { getCodexTranscript } from "../server/codex-transcript.mjs";

if (!process.env.MIRA_TRANSCRIPT_TEST_DATABASE_URL) throw new Error("MIRA_TRANSCRIPT_TEST_DATABASE_URL is required");
const pool = new pg.Pool({ connectionString: process.env.MIRA_TRANSCRIPT_TEST_DATABASE_URL });
const storeId = `transcript-tail-${crypto.randomUUID()}`;
const threadId = crypto.randomUUID();
const childId = crypto.randomUUID();
const headers = () => ({ "x-codex-operation-id": crypto.randomUUID() });
const event = (type, payload) => ({ timestamp: "2026-09-05T10:00:00Z", type, payload });
const history = [];
for (let turn = 0; turn < 300; turn++) {
  history.push(event("event_msg", { type: "task_started", turn_id: `turn-${turn}` }));
  history.push(event("event_msg", { type: "user_message", message: `question-${turn}` }));
  history.push(event("response_item", { type: "custom_tool_call", name: "exec", call_id: "reused-call", input: `inspect-${turn}` }));
  history.push(event("response_item", { type: "custom_tool_call_output", call_id: "reused-call", output: `answer-${turn}\u0000` }));
  history.push(event("event_msg", { type: "agent_message", message: `response-${turn}` }));
}

try {
  await initializeDatabase(pool);
  const seed = await putSnapshot(pool, storeId, {
    expectedVersion: 0,
    snapshot: {
      created_threads: {
        [threadId]: { source: "cli" },
        [childId]: { source: { subagent: { thread_spawn: { parent_thread_id: threadId } } } },
      },
      histories: { [threadId]: history, [childId]: [event("event_msg", { type: "user_message", message: "child" })] },
    },
  }, headers());
  assert.equal(seed.status, 200);
  const queries = [];
  const reader = { query: async (sql, values) => {
    const result = await pool.query(sql, values);
    queries.push({ sql, rows: result.rowCount });
    return result;
  } };
  const started = performance.now();
  const first = await getCodexTranscript(reader, storeId, threadId, { tail: true, limit: 10 });
  assert.equal(first.status, 200);
  assert.equal(first.body.trace.at(-1).body, "response-299");
  assert.equal(first.body.trace.length, 10);
  assert.match(first.body.nextCursor, /^t2:1:/);
  assert.equal(first.body.totalTraceItems, null);
  assert.ok(queries.every((query) => query.rows <= 120), "first paint must read a bounded tail");
  assert.ok(queries.every((query) => !query.sql.includes("history_manifest")), "tail must not fetch store-wide state");
  console.log(`TAIL first page: ${queries.reduce((sum, query) => sum + query.rows, 0)} rows, ${(performance.now() - started).toFixed(1)} ms`);

  const append = await commitDelta(pool, storeId, {
    expectedVersion: 1, stateChanges: [],
    historyChanges: [{ threadId, mode: "append", expectedGeneration: 1, expectedItemCount: history.length,
      items: [event("event_msg", { type: "agent_message", message: "appended-later" })] }],
  }, headers());
  assert.equal(append.status, 200);
  const seen = new Set(first.body.trace.filter((item) => item.kind !== "tool").map((item) => item.body));
  let cursor = first.body.nextCursor;
  let pages = 1;
  while (cursor) {
    const page = await getCodexTranscript(pool, storeId, threadId, { tail: true, limit: 10, cursor });
    assert.equal(page.status, 200);
    assert.ok(!page.body.trace.some((item) => item.body === "appended-later"), "older paging keeps its snapshot boundary");
    for (const item of page.body.trace.filter((item) => item.kind !== "tool")) seen.add(item.body);
    assert.notEqual(page.body.nextCursor, cursor);
    cursor = page.body.nextCursor;
    assert.ok(++pages < 400);
  }
  assert.equal(seen.size, 600, "paging must not skip old questions or responses");
  const legacy = await getCodexTranscript(pool, storeId, threadId, { limit: 10 });
  assert.equal(legacy.status, 200);
  assert.equal(legacy.body.trace.at(-1).body, "appended-later");
  assert.equal((await getSnapshot(pool, storeId)).snapshot.histories[childId][0].payload.message, "child");

  const replacement = await commitDelta(pool, storeId, {
    expectedVersion: 2, stateChanges: [], historyChanges: [{
      threadId, mode: "replace", expectedGeneration: 1, expectedItemCount: history.length + 1,
      items: [event("event_msg", { type: "user_message", message: "new-generation" })],
    }],
  }, headers());
  assert.equal(replacement.status, 200);
  assert.equal((await getCodexTranscript(pool, storeId, threadId, { tail: true, cursor: first.body.nextCursor })).status, 409);
  for (const cursor of ["junk", "t2:2:999:1", "t2:2:0:1", "t2:99999999999999999999:1:1"]) {
    assert.equal((await getCodexTranscript(pool, storeId, threadId, { tail: true, cursor })).status, 400);
  }
  assert.equal((await getCodexTranscript(pool, storeId, crypto.randomUUID(), { tail: true })).status, 404);
  console.log("PASS: bounded PostgreSQL tail, all-page coverage, append stability, generation isolation, NUL preservation, v1/v2 compatibility and independent child history");
} finally {
  await pool.end();
}

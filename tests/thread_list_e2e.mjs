// Run against a disposable PostgreSQL database, never production.
// MIRA_THREAD_LIST_TEST_DATABASE_URL=postgresql://... node tests/thread_list_e2e.mjs
import assert from "node:assert/strict";
import crypto from "node:crypto";
import pg from "../server/node_modules/pg/lib/index.js";
import { initializeDatabase } from "../server/db.mjs";
import { putSnapshot, commitDelta } from "../server/thread-store.mjs";
import { listImportedThreads } from "../server/codex-session-import.mjs";

if (!process.env.MIRA_THREAD_LIST_TEST_DATABASE_URL) throw new Error("MIRA_THREAD_LIST_TEST_DATABASE_URL is required");
const pool = new pg.Pool({ connectionString: process.env.MIRA_THREAD_LIST_TEST_DATABASE_URL });
const storeId = `thread-list-${crypto.randomUUID()}`;
const ids = Array.from({ length: 7 }, () => crypto.randomUUID());
const headers = () => ({ "x-codex-operation-id": crypto.randomUUID() });
const metadata = [
  { updated_at: "2026-09-01T10:00:00Z" },
  { updated_at: "2026-09-05T09:00:00+08:00" },
  { updated_at: "2026-09-05T02:00:00Z" },
  { updated_at: "invalid", advance_recency_at: "2026-09-03T02:00:00Z" },
  { updated_at: "2026-99-99T00:00:00Z", created_at: "2026-09-02T02:00:00Z" },
  { updated_at: "infinity" },
  { updated_at: "2026-09-05T02:00:00Z" },
];
try {
  await initializeDatabase(pool);
  const seed = await putSnapshot(pool, storeId, {
    expectedVersion: 0, snapshot: {
      metadata_updates: Object.fromEntries(ids.map((id, index) => [id, metadata[index]])),
      histories: Object.fromEntries(ids.map((id) => [id, []])),
    },
  }, headers());
  assert.equal(seed.status, 200);
  const tied = [ids[2], ids[6]].sort().reverse();
  const expected = [...tied, ids[1], ids[3], ids[4], ids[0], ids[5]];
  const list = await listImportedThreads(pool, storeId);
  assert.deepEqual(list.map((thread) => thread.threadId), expected);
  assert.equal(list[2].updatedAt, "2026-09-05T01:00:00.000Z", "timestamps compare as instants across time zones");
  assert.equal(list.at(-1).updatedAt, null, "missing activity time must not use projection refresh time");
  assert.deepEqual((await listImportedThreads(pool, storeId, 2)).map((thread) => thread.threadId), tied, "sort before applying the result limit");

  const update = await commitDelta(pool, storeId, {
    expectedVersion: 1,
    stateChanges: [{ mode: "set", path: ["metadata_updates", ids[0], "updated_at"], conflictPolicy: "lastWriteWins",
      expected: { exists: true, value: metadata[0].updated_at }, value: "2026-09-06T01:00:00Z" }],
    historyChanges: [],
  }, headers());
  assert.equal(update.status, 200, JSON.stringify(update.body));
  assert.deepEqual((await listImportedThreads(pool, storeId)).map((thread) => thread.threadId), [ids[0], ...expected.filter((id) => id !== ids[0])],
    "only the active thread moves to the top when a store commit rebuilds all projections");
  console.log("PASS: conversation activity order, limits, time zones, missing/invalid dates and projection rebuilds");
} finally {
  await pool.end();
}

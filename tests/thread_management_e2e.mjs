// Run against a disposable DB: MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL=postgresql://...
import assert from "node:assert/strict";
import crypto from "node:crypto";
import pg from "../server/node_modules/pg/lib/index.js";
import { initializeDatabase } from "../server/db.mjs";
import { putSnapshot, getSnapshot, getStoreHead, commitDelta, rebuildSnapshot } from "../server/thread-store.mjs";
import { listImportedThreads } from "../server/codex-session-import.mjs";
import { renameThread } from "../server/thread-management.mjs";

if (!process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL) throw new Error("a disposable test database is required");
const pool = new pg.Pool({ connectionString: process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL });
const storeId = `thread-management-${crypto.randomUUID()}`;
const parent = crypto.randomUUID(), child = crypto.randomUUID();
const headers = () => ({ "x-codex-operation-id": crypto.randomUUID() });
const rename = (name, expectedName = null, generation = 1) => ({ name, expectedName, generation, operationId: crypto.randomUUID() });
try {
  await initializeDatabase(pool);
  const snapshot = {
    created_threads: { [parent]: { source: "cli", metadata: { cwd: "/workspace" } }, [child]: { source: "subagent", parent_thread_id: parent } },
    metadata_updates: { [parent]: { title: "Original preview", cwd: "/workspace", updated_at: "2026-09-01T10:00:00Z" }, [child]: { title: "Child", cwd: "/workspace" } },
    histories: { [parent]: [{ type: "unknown_future_item", payload: { untouched: "正文\u0000", extra: true } }], [child]: [{ type: "response_item", payload: { role: "assistant", text: "child" } }] },
  };
  assert.equal((await putSnapshot(pool, storeId, { expectedVersion: 0, snapshot }, headers())).status, 200);
  for (const name of ["", " ", "a\nb", "a".repeat(201)]) assert.equal((await renameThread(pool, storeId, parent, rename(name))).status, 400);
  const first = rename(" 项目讨论 ");
  assert.equal((await renameThread(pool, storeId, parent, first)).status, 200);
  const renamedHead = await getStoreHead(pool, storeId);
  assert.equal(renamedHead.state.names[parent], "项目讨论");
  const duplicate = await renameThread(pool, storeId, parent, first);
  assert.equal(duplicate.body.duplicate, true);
  assert.equal((await getStoreHead(pool, storeId)).version, renamedHead.version);
  const [row] = await listImportedThreads(pool, storeId, 1, parent);
  assert.equal(row.title, "项目讨论"); assert.equal(row.name, "项目讨论");
  assert.equal(row.updatedAt, "2026-09-01T10:00:00.000Z", "renaming does not reorder by projection writes");
  assert.equal((await renameThread(pool, storeId, parent, rename("stale edit"))).status, 409);
  const results = await Promise.all([renameThread(pool, storeId, parent, rename("Window A", "项目讨论")), renameThread(pool, storeId, parent, rename("Window B", "项目讨论"))]);
  assert.deepEqual(results.map(r => r.status).sort(), [200, 409], "concurrent title edits must not silently overwrite");
  const head = await getStoreHead(pool, storeId);
  const winner = head.state.names[parent];
  const appended = await commitDelta(pool, storeId, { expectedVersion: head.version, stateChanges: [{ path: ["metadata_updates", parent, "title"], mode: "set", expected: { exists: true, value: "Original preview" }, conflictPolicy: "compareAndSwap", value: "Updated preview" }], historyChanges: [{ threadId: child, mode: "append", expectedGeneration: 1, expectedItemCount: 1, items: [{ type: "future_child_item" }] }] }, headers());
  assert.equal(appended.status, 200);
  assert.equal((await listImportedThreads(pool, storeId, 1, parent))[0].title, winner, "runtime previews cannot replace a chosen name");
  const v1 = await getSnapshot(pool, storeId);
  assert.equal(v1.snapshot.names[parent], winner);
  assert.deepEqual(v1.snapshot.histories[parent], snapshot.histories[parent]);
  assert.equal(v1.snapshot.created_threads[child].parent_thread_id, parent);
  assert.equal(v1.snapshot.histories[child].length, 2);
  await rebuildSnapshot(pool, storeId);
  assert.equal((await getSnapshot(pool, storeId)).snapshot.names[parent], winner);
  assert.equal((await listImportedThreads(pool, storeId, 1, parent))[0].title, winner);
  assert.equal((await renameThread(pool, storeId, parent, rename("wrong generation", winner, 99))).status, 409);
  assert.equal((await renameThread(pool, storeId, crypto.randomUUID(), rename("missing"))).status, 404);
  console.log("PASS: durable names, idempotent retries, concurrent CAS, v1/v2 compatibility, projection rebuilds, raw history and subagent preservation");
} finally { await pool.end(); }

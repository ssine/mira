// Run against a disposable DB: MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL=postgresql://...
import assert from "node:assert/strict";
import crypto from "node:crypto";
import pg from "../server/node_modules/pg/lib/index.js";
import { initializeDatabase } from "../server/db.mjs";
import { putSnapshot, getSnapshot, getStoreHead, getThreadHistory, commitDelta, commitImportedHistory, rebuildSnapshot, listStoreEvents, listThreadEvents } from "../server/thread-store.mjs";
import { listImportedThreads } from "../server/codex-session-import.mjs";
import { manageThread, renameThread } from "../server/thread-management.mjs";
import { processThreadErasureBatch, threadErasureStatus } from "../server/thread-erasure.mjs";

if (!process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL) throw new Error("a disposable test database is required");
const owner = new pg.Pool({ connectionString: process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL });
const database = `erasure_test_${crypto.randomUUID().replaceAll("-", "")}`;
await owner.query(`CREATE DATABASE ${database}`);
// Isolate fault-injection jobs from a live disposable Server's background worker.
const databaseUrl = new URL(process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL);
databaseUrl.pathname = `/${database}`;
const pool = new pg.Pool({ connectionString: databaseUrl.toString() });
async function drain(storeId) {
  for (let batches = 0; (await threadErasureStatus(pool, storeId)).pending; batches++) {
    assert(batches < 500, "erasure did not finish");
    assert(await processThreadErasureBatch(pool, { storeId, eventBatchSize: 2, itemBatchSize: 1 }));
  }
}
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
  const actionBody = () => ({ generation: 1, operationId: crypto.randomUUID(), itemCount: 1 });
  const archived = actionBody();
  assert.equal((await manageThread(pool, storeId, parent, "archive", archived)).status, 200);
  assert.equal((await manageThread(pool, storeId, parent, "archive", archived)).body.duplicate, true);
  assert.equal((await listImportedThreads(pool, storeId, 100, null, false)).some(row => row.threadId === parent), false);
  assert.equal((await listImportedThreads(pool, storeId, 100, null, true))[0].threadId, parent);
  await rebuildSnapshot(pool, storeId);
  assert.equal((await listImportedThreads(pool, storeId, 1, parent))[0].archived, true);
  assert.deepEqual((await getSnapshot(pool, storeId)).snapshot.histories, v1.snapshot.histories, "archive never removes history");
  assert.equal((await manageThread(pool, storeId, parent, "restore", actionBody())).status, 200);
  assert.equal((await listImportedThreads(pool, storeId, 1, parent))[0].archived, false);
  assert.equal((await manageThread(pool, storeId, parent, "delete", { ...actionBody(), itemCount: 0 })).status, 409);

  const nodeId = crypto.randomUUID(), ownImport = crypto.randomUUID(), sharedImport = crypto.randomUUID(), childImport = crypto.randomUUID();
  await pool.query(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations)
    VALUES($1::uuid,$1::text,'deletion-test','linux','amd64','linux','test','{}','[]')`, [nodeId]);
  for (const [importId, threadId] of [[ownImport,parent],[sharedImport,parent],[childImport,child]]) {
    await pool.query(`INSERT INTO mira_codex_session_imports(import_id,store_id,thread_id,source_node_id,source_path,source_sha256,source_size_bytes,source_item_count,status)
      VALUES($1::uuid,$2,$3,$4,$1::text,'fixture',1,1,'imported')`, [importId,storeId,threadId,nodeId]);
    await pool.query("INSERT INTO mira_codex_session_import_records(import_id,line_seq,raw_record,raw_sha256) VALUES($1,1,$2::json,'fixture')", [importId, JSON.stringify({ text: "raw\u0000" })]);
  }
  await pool.query("INSERT INTO mira_codex_session_import_segments(import_id,segment_index,source_import_id,first_line_seq,item_count) VALUES($1,0,$1,1,1),($2,0,$3,1,1)", [ownImport,childImport,sharedImport]);
  const beforeDelete = await getSnapshot(pool, storeId);
  const deleted = actionBody();
  assert.equal((await manageThread(pool, storeId, parent, "delete", deleted)).status, 200);
  const afterDelete = await getStoreHead(pool, storeId);
  assert.equal(afterDelete.version, beforeDelete.version + 1);
  assert.equal((await manageThread(pool, storeId, parent, "delete", deleted)).body.duplicate, true);
  assert.equal((await manageThread(pool, storeId, parent, "delete", { ...deleted, itemCount: 99 })).status, 409);
  await assert.rejects(pool.query(`INSERT INTO codex_thread_events(store_id,thread_id,generation,item_seq,operation_id,event_format_version,payload,payload_sha256)
    VALUES($1,$2,1,99,$3,1,'{}','fixture')`, [storeId,parent,deleted.operationId]), { code: "23514" }, "direct SQL cannot bypass permanent deletion");
  assert.equal((await threadErasureStatus(pool, storeId)).pending, 1);
  assert((await pool.query("SELECT 1 FROM codex_thread_events WHERE store_id=$1 AND thread_id=$2", [storeId,parent])).rowCount > 0, "physical cleanup is deferred");
  assert.equal((await getThreadHistory(pool, storeId, parent, 1, beforeDelete.version)).status, 404, "old-version history is inaccessible before cleanup");
  assert.deepEqual(await listThreadEvents(pool, storeId, parent, null, 0, 100), []);
  for (const event of await listStoreEvents(pool, storeId, 0, 100)) assert.equal(event.historyManifest[parent], undefined);
  assert.equal((await getSnapshot(pool, storeId)).snapshot.histories[parent], undefined);
  assert.equal((await listImportedThreads(pool, storeId, 1, parent)).length, 0);
  await drain(storeId);
  assert.equal((await manageThread(pool, storeId, parent, "delete", deleted)).body.cleanupPending, false);
  assert.equal((await pool.query("SELECT 1 FROM codex_thread_events WHERE store_id=$1 AND thread_id=$2", [storeId,parent])).rowCount, 0);
  assert.equal((await pool.query("SELECT 1 FROM codex_store_state_changes WHERE store_id=$1 AND thread_id=$2", [storeId,parent])).rowCount, 0, "older store versions cannot recover deleted metadata");
  assert.equal((await pool.query("SELECT 1 FROM mira_codex_session_import_records WHERE import_id=$1", [ownImport])).rowCount, 0);
  assert.equal((await pool.query("SELECT 1 FROM mira_codex_session_import_records WHERE import_id=$1", [sharedImport])).rowCount, 1, "a surviving fork retains its shared source provenance");
  assert.equal((await getThreadHistory(pool, storeId, parent, 1, beforeDelete.version)).status, 404);
  await rebuildSnapshot(pool, storeId);
  const remaining = (await getSnapshot(pool, storeId)).snapshot;
  assert.equal(remaining.histories[parent], undefined);
  assert.deepEqual(remaining.histories[child], beforeDelete.snapshot.histories[child]);
  assert.equal((await listImportedThreads(pool, storeId, 1, parent)).length, 0);
  await assert.rejects(putSnapshot(pool, storeId, { expectedVersion: afterDelete.version, snapshot: beforeDelete.snapshot }, headers()), { code: "thread_deleted", statusCode: 410 });
  await assert.rejects(commitDelta(pool, storeId, { expectedVersion: afterDelete.version, stateChanges: [], historyChanges: [{ threadId: parent, mode: "append", expectedGeneration: 0, expectedItemCount: 0, items: [] }] }, headers()), { code: "thread_deleted" });
  await assert.rejects(commitImportedHistory(pool, storeId, { threadId: parent }), { code: "thread_deleted" });
  assert.equal((await putSnapshot(pool, storeId, { expectedVersion: afterDelete.version, snapshot: remaining }, headers())).status, 200, "unrelated v1 writers remain usable");
  const raceId = crypto.randomUUID();
  let raceHead = await getStoreHead(pool, storeId);
  await commitDelta(pool, storeId, { expectedVersion: raceHead.version, stateChanges: [], historyChanges: [{ threadId: raceId, mode: "append", expectedGeneration: 0, expectedItemCount: 0, items: [] }] }, headers());
  raceHead = await getStoreHead(pool, storeId);
  const race = await Promise.all([
    manageThread(pool, storeId, raceId, "delete", { ...actionBody(), itemCount: 0 }),
    commitDelta(pool, storeId, { expectedVersion: raceHead.version, stateChanges: [], historyChanges: [{ threadId: raceId, mode: "append", expectedGeneration: 1, expectedItemCount: 0, items: [{ type: "new_item" }] }] }, headers()).catch(error => ({ status: error.statusCode })),
  ]);
  assert.equal(race.filter(result => result.status === 200).length, 1, "delete and an overlapping append cannot both commit");
  assert(race.some(result => [409,410].includes(result.status)));

  // Production stores contain thousands of large versions preceding a new
  // thread. Deleting it must not physically rewrite those unrelated rows.
  const scopeStore = `delete-scope-${crypto.randomUUID()}`, scopeThread = crypto.randomUUID();
  const unrelated = { histories: { [child]: [] }, future_metadata: { [child]: { reference: scopeThread } } };
  let scopeVersion = 0;
  for (const state of [unrelated, { ...unrelated, scalar: true }, { ...unrelated, rollout_paths: null },
    { ...unrelated, future_metadata: { ...unrelated.future_metadata, [scopeThread]: { secret: "erase" } } },
    { ...unrelated, rollout_paths: { "/deleted/rollout": scopeThread, "/surviving/rollout": child } },
    { ...unrelated, histories: { ...unrelated.histories, [scopeThread]: [] } }]) {
    assert.equal((await putSnapshot(pool, scopeStore, { expectedVersion: scopeVersion++, snapshot: state }, headers())).status, 200);
  }
  const physicalRows = () => pool.query("SELECT event_seq::text,ctid::text FROM codex_store_events WHERE store_id=$1 AND event_seq<=3 ORDER BY event_seq", [scopeStore]);
  const untouched = (await physicalRows()).rows;
  assert.equal((await manageThread(pool, scopeStore, scopeThread, "delete", { ...actionBody(), itemCount: 0 })).status, 200);
  scopeVersion++;
  // Fail after a batch's physical update but before its durable checkpoint.
  // Retrying from the persisted cursor must neither skip data nor lose history.
  const faultPool = { async connect() {
    const client = await pool.connect();
    return { release: () => client.release(), async query(sql, values) {
      const result = await client.query(sql, values);
      if (sql.startsWith("DELETE FROM codex_store_state_changes")) throw Object.assign(new Error("injected batch failure"), { code: "injected" });
      return result;
    } };
  } };
  await assert.rejects(processThreadErasureBatch(faultPool, { storeId: scopeStore, eventBatchSize: 4 }), { code: "injected" });
  const failedJob = (await pool.query("SELECT after_event_seq,last_error_code FROM mira_thread_erasures WHERE store_id=$1", [scopeStore])).rows[0];
  assert.equal(Number(failedJob.after_event_seq), 0);
  assert.equal(failedJob.last_error_code, "injected");
  assert((await pool.query("SELECT 1 FROM codex_store_state_changes WHERE store_id=$1 AND thread_id=$2", [scopeStore,scopeThread])).rowCount>0);
  await pool.query("UPDATE mira_thread_erasures SET retry_at=NOW() WHERE store_id=$1", [scopeStore]);
  let scanStarted, finishScan;
  const scanning = new Promise(resolve => { scanStarted = resolve; });
  const scanDelay = new Promise(resolve => { finishScan = resolve; });
  const slowPool = { async connect() {
    const client = await pool.connect();
    return { release: () => client.release(), async query(sql, values) {
      if (sql.startsWith("DELETE FROM codex_store_state_changes")) { scanStarted(); await scanDelay; }
      return client.query(sql, values);
    } };
  } };
  const deletion = processThreadErasureBatch(slowPool, { storeId: scopeStore, eventBatchSize: 4 });
  await scanning;
  let writeTimer;
  try {
    const writing = commitDelta(pool, scopeStore, { expectedVersion: scopeVersion, stateChanges: [], historyChanges: [
      { threadId: child, mode: "append", expectedGeneration: 1, expectedItemCount: 0, items: [{ type: "during_deletion_scan" }] },
    ] }, headers());
    const write = await Promise.race([writing, new Promise((_, reject) => {
      writeTimer = setTimeout(() => reject(new Error("deletion scan blocked an unrelated writer")), 3000);
    })]);
    assert.equal(write.status, 200, "other conversations can commit while deletion scans old versions");
  } finally { clearTimeout(writeTimer); finishScan(); }
  assert(Number((await deletion).afterEventSeq)>0);
  await drain(scopeStore);
  assert.deepEqual((await physicalRows()).rows, untouched, "deletion must not rewrite versions without the target thread");
  assert.equal((await pool.query("SELECT 1 FROM codex_store_state_changes WHERE store_id=$1 AND thread_id=$2",[scopeStore,scopeThread])).rowCount,0);
  const state=(await getStoreHead(pool,scopeStore)).state;
  assert.equal(state.future_metadata?.[scopeThread],undefined);
  assert.equal(state.rollout_paths?.["/deleted/rollout"],undefined);
  assert.equal(state.future_metadata[child].reference,scopeThread,"unrelated nested references remain unchanged");
  assert.deepEqual((await getSnapshot(pool, scopeStore)).snapshot.histories[child], [{ type: "during_deletion_scan" }]);
  console.log("PASS: durable names, idempotent retries, concurrent CAS, v1/v2 compatibility, projection rebuilds, raw history and subagent preservation");
  console.log("PASS: archive/restore, permanent content deletion, old-version erasure, shared fork provenance, stale-writer fencing, retry and concurrent deletion");
  console.log("PASS: immediate read/write fencing, bounded erasure, rollback/retry checkpoints, concurrent writes, physical row preservation and shared fork provenance");
} finally { await pool.end(); await owner.query(`DROP DATABASE ${database}`); await owner.end(); }

// Uses an isolated store in the required disposable database, including concurrent one-connection pools.
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import pg from '../server/node_modules/pg/lib/index.js';
import { putSnapshot, getSnapshot, getStoreHead, rebuildSnapshot } from '../server/thread-store.mjs';
import { nameForkThread, renameThread, manageThread } from '../server/thread-management.mjs';
if (!process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL) throw new Error('a disposable database is required');
const connectionString = process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL;
const pool = new pg.Pool({ connectionString, max: 1 });
const other = new pg.Pool({ connectionString, max: 1 });
const store = `fork-titles-${crypto.randomUUID()}`;
const [source, occupiedOne, occupiedThree, first, second, nested, stale, changed] = Array.from({ length: 8 }, () => crypto.randomUUID());
const histories = Object.fromEntries([source, occupiedOne, occupiedThree, first, second, nested, stale, changed].map(id => [id, [{ type: 'unknown_future_record', payload: { text: 'unchanged\u0000', id } }]]));
const request = (sourceThreadId = source, extra = {}) => ({ sourceThreadId, generation: 1, expectedName: null, operationId: crypto.randomUUID(), ...extra });
try {
  assert.equal((await putSnapshot(pool, store, { expectedVersion: 0, snapshot: {
    histories,
    names: { [source]: 'Build Mira', [occupiedOne]: 'Build Mira (1)', [occupiedThree]: 'Build Mira (3)' },
    metadata_updates: Object.fromEntries(Object.keys(histories).map(id => [id, { title: 'original preview', cwd: '/work' }])),
    created_threads: { [nested]: { source: 'subagent', parent_thread_id: source } },
  } }, { 'x-codex-operation-id': crypto.randomUUID() })).status, 200);
  await manageThread(pool, store, occupiedOne, 'archive', { generation: 1, operationId: crypto.randomUUID() });
  const firstBody = request(), secondBody = request();
  const results = await Promise.all([nameForkThread(pool, store, first, firstBody), nameForkThread(other, store, second, secondBody)]);
  assert.deepEqual(results.map(r => r.status), [200, 200]);
  let head = await getStoreHead(pool, store);
  assert.deepEqual([head.state.names[first], head.state.names[second]].sort(), ['Build Mira (2)', 'Build Mira (4)']);
  const initialVersion = head.version;
  assert.equal((await nameForkThread(pool, store, first, firstBody)).body.duplicate, true);
  assert.equal((await getStoreHead(pool, store)).version, initialVersion, 'retry does not allocate another number');
  assert.equal((await nameForkThread(pool, store, nested, request(first))).status, 200);
  assert.equal((await getStoreHead(pool, store)).state.names[nested], 'Build Mira (5)', 'forking a fork increments the same title family');
  assert.equal((await nameForkThread(pool, store, stale, request(source, { generation: 2 }))).status, 409);
  assert.equal((await renameThread(pool, store, changed, { name: 'User title', expectedName: null, generation: 1, operationId: crypto.randomUUID() })).status, 200);
  assert.equal((await nameForkThread(pool, store, changed, request())).status, 409, 'a concurrent manual rename is preserved');
  assert.equal((await getStoreHead(pool, store)).state.names[changed], 'User title');
  head = await getStoreHead(pool, store);
  assert.equal((await renameThread(pool, store, first, { name: 'Custom fork title', expectedName: head.state.names[first], generation: 1, operationId: crypto.randomUUID() })).status, 200);
  assert.equal((await nameForkThread(pool, store, first, firstBody)).body.duplicate, true);
  assert.equal((await getStoreHead(pool, store)).state.names[first], 'Custom fork title', 'late response retry cannot overwrite later edits');
  assert.equal((await nameForkThread(pool, store, second, firstBody)).status, 409, 'operation UUID cannot be reused for another fork');
  assert.equal((await nameForkThread(pool, store, source, request())).status, 400);
  assert.equal((await nameForkThread(pool, store, stale, request(crypto.randomUUID()))).status, 404);
  await rebuildSnapshot(pool, store);
  const after = await getSnapshot(pool, store);
  assert.equal(after.snapshot.names[nested], 'Build Mira (5)');
  assert.deepEqual(after.snapshot.histories, histories);
  assert.equal(after.snapshot.created_threads[nested].parent_thread_id, source);
  console.log('PASS: inherited numbered fork titles, archived collisions, concurrent allocation, single-connection pools, replay, manual edits, generations, v1 snapshots, rebuild and raw/subagent history preservation');
} finally { await pool.end(); await other.end(); }

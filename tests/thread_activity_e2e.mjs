// A disposable database exercises migration/backfill, raw JSON, immutable
// generations, native CLI/subagent lifecycle records and Node reachability.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import pg from "../server/node_modules/pg/lib/index.js";
import { initializeDatabase } from "../server/db.mjs";
import { putSnapshot, getSnapshot, getStoreHead, commitDelta, rebuildSnapshot } from "../server/thread-store.mjs";
import { listImportedThreads } from "../server/codex-session-import.mjs";
const databaseUrl = process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL;
if (!databaseUrl) throw new Error("a disposable database is required");
const owner = new pg.Pool({ connectionString: databaseUrl });
const database = `activity_${crypto.randomUUID().replaceAll("-", "")}`;
await owner.query(`CREATE DATABASE ${database}`);
const url = new URL(databaseUrl); url.pathname = `/${database}`;
const pool = new pg.Pool({ connectionString: url.toString() });
const store = "activity-test", node = crypto.randomUUID();
const ids = Object.fromEntries(["running", "idle", "failed", "child", "late", "unbound", "replacement", "copied"].map(name => [name, crypto.randomUUID()]));
const timestamp = new Date().toISOString();
const event = (type, turn_id, extra = {}) => ({ type: "event_msg", payload: { type, turn_id,
  ...(type === "task_started" ? { started_at: Math.floor(Date.parse(timestamp) / 1000) } : { completed_at: Math.floor(Date.parse(timestamp) / 1000) }), ...extra } });
const headers = () => ({ "x-codex-operation-id": crypto.randomUUID() });
try {
  await initializeDatabase(pool, { throughVersion: 17 });
  await pool.query(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status,channel_status,reported_app_server)
    VALUES($1::uuid,$1::text,'activity-test','linux','amd64','linux','test','{}','[]','approved','{"connected":true}',$2::jsonb)`,
  [node, JSON.stringify({ status: "running", startedAt: new Date(Date.now() - 60_000).toISOString() })]);
  const snapshot = { created_threads: {}, histories: {} };
  for (const [name, id] of Object.entries(ids)) {
    snapshot.created_threads[id] = { source: name === "child" ? "subagent" : "cli", ...(name === "child" ? { parent_thread_id: ids.running } : {}) };
    snapshot.histories[id] = [event("task_started", name)];
  }
  snapshot.histories[ids.idle].push(event("task_complete", "idle", { last_agent_message: "raw\u0000result" }));
  snapshot.histories[ids.failed].push(event("task_complete", "failed", { error: { message: "failed\u0000" } }));
  snapshot.histories[ids.child].push(event("turn_aborted", "child"));
  snapshot.created_threads[ids.copied].metadata = { timestamp: new Date(Date.now() + 1000).toISOString() };
  snapshot.histories[ids.late].push(event("task_complete", "older"), { type: "future_tool", payload: { type: "task_complete", turn_id: "late" }, text: "\u0000" });
  assert.equal((await putSnapshot(pool, store, { expectedVersion: 0, snapshot }, headers())).status, 200);
  for (const id of Object.values(ids).filter(id => id !== ids.unbound)) await pool.query(`INSERT INTO mira_codex_thread_runtimes(store_id,thread_id,node_id,bound_at) VALUES($1,$2,$3,NOW()-INTERVAL '1 minute')`, [store,id,node]);
  await initializeDatabase(pool);
  await initializeDatabase(pool); // checksum verification on restart
  assert.equal((await pool.query("SELECT 1 FROM pg_indexes WHERE indexname='codex_thread_events_lifecycle_idx'")).rowCount, 1);
  const read = async () => Object.fromEntries((await listImportedThreads(pool, store)).map(row => [Object.keys(ids).find(name => ids[name] === row.threadId), row]));
  let rows = await read();
  for (const name of ["running", "late", "replacement"]) assert.equal(rows[name].activity.state, "running", name);
  assert.equal(rows.idle.activity.state, "idle");
  assert.equal(rows.failed.activity.state, "failed");
  assert.equal(rows.child.activity.state, "interrupted");
  assert.equal(rows.child.parentThreadId, ids.running);
  assert.equal(rows.unbound.activity.reason, "unbound");
  assert.equal(rows.copied.activity.reason, "history", "a copied parent's running turn does not claim the fork is executing");
  await pool.query("UPDATE codex_nodes SET channel_status='{\"connected\":false}' WHERE node_id=$1", [node]);
  rows = await read();
  assert.equal(rows.running.activity.reason, "offline", "Node status is not cached with immutable history");
  assert.equal(rows.idle.activity.state, "idle", "offline does not change completed history");
  await pool.query("UPDATE codex_nodes SET channel_status='{\"connected\":true}',last_seen_at=NOW(),reported_app_server=$2::jsonb WHERE node_id=$1", [node, JSON.stringify({status:"running",startedAt:new Date(Date.now()+60_000).toISOString()})]);
  await pool.query("UPDATE mira_codex_thread_runtimes SET bound_at=NOW() WHERE store_id=$1", [store]);
  assert.equal((await read()).running.activity.reason, "runtime");
  await pool.query("UPDATE codex_nodes SET reported_app_server=$2::jsonb WHERE node_id=$1", [node, JSON.stringify({status:"running",startedAt:new Date(Date.now()-60_000).toISOString()})]);
  const head = await getStoreHead(pool, store);
  const changes = { expectedVersion: head.version, stateChanges: [], historyChanges: [{ threadId: ids.running, mode: "append", expectedGeneration: 1, expectedItemCount: 1, items: [event("task_complete", "running")] }] };
  const requestHeaders = headers();
  assert.equal((await commitDelta(pool, store, changes, requestHeaders)).status, 200);
  assert.equal((await commitDelta(pool, store, changes, requestHeaders)).body.duplicate, true);
  assert.equal((await read()).running.activity.state, "idle", "a new immutable count invalidates the running cache");
  const before = await getSnapshot(pool, store);
  assert.deepEqual(before.snapshot.histories[ids.idle], snapshot.histories[ids.idle]);
  before.snapshot.histories[ids.replacement] = [event("task_started", "replacement-next"), event("task_complete", "replacement-next")];
  assert.equal((await putSnapshot(pool, store, { expectedVersion: before.version, snapshot: before.snapshot }, headers())).status, 200);
  rows = await read();
  assert.equal(rows.replacement.activity.turnId, "replacement-next");
  assert.equal(rows.replacement.activity.state, "idle");
  await rebuildSnapshot(pool, store);
  assert.equal((await read()).replacement.activity.state, "idle");
  assert.deepEqual((await getSnapshot(pool, store)).snapshot.histories[ids.idle], snapshot.histories[ids.idle]);
  console.log("PASS: lifecycle migration/backfill, CLI/subagent activity, raw NUL preservation, old completion isolation, Node offline/restart, v1/v2 commits, idempotency, generations and rebuild");
} finally {
  await pool.end();
  await owner.query(`DROP DATABASE ${database} WITH (FORCE)`);
  await owner.end();
}

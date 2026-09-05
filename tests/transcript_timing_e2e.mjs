// Run only against a disposable PostgreSQL database.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import pg from "../server/node_modules/pg/lib/index.js";
import { initializeDatabase } from "../server/db.mjs";
import { putSnapshot, commitDelta } from "../server/thread-store.mjs";
import { getCodexTranscript } from "../server/codex-transcript.mjs";

if (!process.env.MIRA_TRANSCRIPT_TEST_DATABASE_URL) throw new Error("MIRA_TRANSCRIPT_TEST_DATABASE_URL is required");
const pool = new pg.Pool({ connectionString: process.env.MIRA_TRANSCRIPT_TEST_DATABASE_URL });
const storeId = `timing-${crypto.randomUUID()}`;
const threadId = crypto.randomUUID();
const importedId = crypto.randomUUID();
const headers = () => ({ "x-codex-operation-id": crypto.randomUUID() });
const event = (payload) => ({ type: "event_msg", payload });
const history = [event({ type: "task_started", turn_id: "native", started_at: 1788602400 }),
  { type: "turn_context", payload: { turn_id: "native" } },
  ...Array.from({ length: 160 }, (_, i) => event({ type: "agent_message", message: `Message ${i}` })),
  { type: "compacted", payload: { message: "Internal context", replacement_history: [] } },
  event({ type: "task_complete", turn_id: "native", completed_at: 1788602412, duration_ms: 12542 })];
const importedHistory = [event({ type: "task_started", turn_id: "imported" }),
  event({ type: "agent_message", message: "Imported response\u0000" })];
try {
  await initializeDatabase(pool);
  assert.equal((await putSnapshot(pool, storeId, { expectedVersion: 0,
    snapshot: { histories: { [threadId]: history, [importedId]: importedHistory } } }, headers())).status, 200);
  const page = await getCodexTranscript(pool, storeId, threadId, { tail: true, limit: 10 });
  assert.equal(page.body.trace.at(-1).kind, "compaction");
  const older = await getCodexTranscript(pool, storeId, threadId, { tail: true, limit: 10, cursor: page.body.nextCursor });
  assert.ok(older.body.trace.every((item) => item.elapsedMs === 12542 && item.timingScope === "turn"),
    "older pages keep their turn's completion timing beyond the page boundary");

  const nodeId = crypto.randomUUID();
  const importId = crypto.randomUUID();
  await pool.query(`INSERT INTO codex_nodes
    (node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations)
    VALUES ($1,$2,'test','linux','amd64','linux','test','{}','[]')`, [nodeId, `test-${nodeId}`]);
  await pool.query(`INSERT INTO mira_codex_session_imports
    (import_id,store_id,thread_id,source_node_id,source_path,source_sha256,source_size_bytes,source_item_count,store_event_seq,status)
    VALUES ($1,$2,$3,$4,'/test','test',100,2,1,'imported')`, [importId, storeId, importedId, nodeId]);
  // The copied lineage starts at line 3 of its source, rather than line 1.
  await pool.query(`INSERT INTO mira_codex_session_import_segments
    (import_id,segment_index,source_import_id,first_line_seq,item_count) VALUES ($1,0,$1,3,2)`, [importId]);
  for (const [i, item] of importedHistory.entries()) {
    const raw = { ...item, timestamp: `2026-09-01T10:00:0${i * 5}.000Z` };
    await pool.query(`INSERT INTO mira_codex_session_import_records (import_id,line_seq,raw_record,raw_sha256)
      VALUES ($1,$2,$3::json,'test')`, [importId, i + 3, JSON.stringify(raw)]);
  }
  const restored = await getCodexTranscript(pool, storeId, importedId, { tail: true });
  assert.equal(restored.body.trace[0].completedAt, "2026-09-01T10:00:05.000Z");
  assert.equal(restored.body.trace[0].elapsedMs, 5000);
  assert.equal(restored.body.trace[0].timingScope, undefined);
  assert.equal((await pool.query(`SELECT payload::text FROM codex_thread_events
    WHERE store_id=$1 AND thread_id=$2 AND item_seq=2`, [storeId, importedId])).rows[0].payload.includes('"timestamp"'), false,
    "timestamp recovery does not rewrite canonical data");
  const replacement = await commitDelta(pool, storeId, { expectedVersion: 1, stateChanges: [], historyChanges: [{
    threadId: importedId, mode: "replace", expectedGeneration: 1, expectedItemCount: 2,
    items: [event({ type: "agent_message", message: "Different generation" })],
  }] }, headers());
  assert.equal(replacement.status, 200);
  const changed = await getCodexTranscript(pool, storeId, importedId, { tail: true });
  assert.notEqual(changed.body.trace[0].completedAt, "2026-09-01T10:00:00.000Z", "a changed item must not inherit old provenance clocks");
  console.log("PASS: native timing across pages, durable compaction, imported lineage clocks, NUL and replacement isolation");
} finally { await pool.end(); }

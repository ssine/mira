// Disposable PostgreSQL only. Optional MIRA_REAL_DESKTOP_SESSION is read-only;
// no private source text is logged or stored in the repository.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import { Pool } from "../server/node_modules/pg/esm/index.mjs";
import { initializeDatabase } from "../server/db.mjs";
import { importCodexSession } from "../server/codex-session-import.mjs";
import { commitDelta, getStoreHead, getSnapshot, putSnapshot } from "../server/thread-store.mjs";

if (!process.env.MIRA_TRANSFER_TEST_DATABASE_URL) throw new Error("MIRA_TRANSFER_TEST_DATABASE_URL must point to a disposable database");
const pool = new Pool({ connectionString: process.env.MIRA_TRANSFER_TEST_DATABASE_URL });
await initializeDatabase(pool);
const nodeId = crypto.randomUUID();
await pool.query(`INSERT INTO codex_nodes (node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status)
  VALUES ($1::uuid,$1::text,'transfer-desktop','windows','amd64','windows','0.12.0','{"codexSessions":true}','[]','approved')`, [nodeId]);
const wslNodeId = crypto.randomUUID();
await pool.query(`INSERT INTO codex_nodes (node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status)
  VALUES ($1::uuid,$1::text,'transfer-desktop','linux','amd64','wsl','0.12.0','{"appServer":true}','[]','approved')`, [wslNodeId]);
const principal = { kind: "admin", username: "transfer-test" };
const makeFixture = (threadId = crypto.randomUUID()) => Buffer.from([
  JSON.stringify({ type: "session_meta", payload: { id: threadId, originator: "Codex Desktop", source: "vscode", cwd: "/desktop/project", cli_version: "0.152.1", history_mode: "paginated", base_instructions: "fixture", futureField: { retain: true } } }),
  JSON.stringify({ type: "event_msg", payload: { type: "user_message", message: "Desktop import fixture 中文" } }),
  JSON.stringify({ type: "future_record", payload: { retained: "x".repeat(9 * 1024 * 1024), nul: "a\u0000b", surrogate: "\ud800" } }),
  ...Array.from({ length: 220 }, (_, index) => JSON.stringify({ type: "event_msg", payload: { type: "agent_message", message: `fixture-${index}` } })),
  "",
].join("\n"));
function serviceFor(bytes, path = "/fixture/archived_sessions/rollout-test.jsonl") {
  const meta = JSON.parse(bytes.subarray(0, bytes.indexOf(10)).toString()).payload;
  const summary = { path, threadId: meta.id, sizeBytes: bytes.length, modifiedAt: "2026-09-05T00:00:00Z", title: "Desktop fixture", cwd: meta.cwd, codexVersion: meta.cli_version, clientKind: "desktop", archived: true };
  return { summary, async invoke(_p, _n, _c, params) {
    if (params.action === "list") return { sessions: [summary] };
    const next = Math.min(bytes.length, params.cursor + params.limit);
    return { cursor: params.cursor, nextCursor: next, sizeBytes: bytes.length, modifiedAt: summary.modifiedAt,
      content: bytes.subarray(params.cursor, next).toString("base64"), encoding: "base64", eof: next === bytes.length };
  } };
}
async function run(service, storeId, context = {}) {
  return importCodexSession(pool, service, principal, nodeId, { path: service.summary.path, storeId }, null, context);
}
try {
  const bytes = makeFixture();
  const service = serviceFor(bytes);
  const storeId = `transfer-${crypto.randomUUID()}`;
  const phases = new Set();
  const imported = await run(service, storeId, { onProgress: (p) => phases.add(p.phase) });
  assert.equal(imported.status, 200);
  assert.equal(imported.body.itemCount, 223);
  assert.equal(imported.body.runtimeNodeId, wslNodeId, "Windows-hosted Desktop WSL session was not assigned to the matching WSL runtime");
  assert(phases.has("reading") && phases.has("publishing"));
  const head = await getStoreHead(pool, storeId);
  assert.equal(head.state.created_threads[service.summary.threadId].originator, "Codex Desktop");
  assert.equal(head.state.created_threads[service.summary.threadId].history_mode, "legacy");
  const provenance = await pool.query("SELECT source_sha256 FROM mira_codex_session_imports WHERE import_id=$1", [imported.body.importId]);
  assert.equal(provenance.rows[0].source_sha256, crypto.createHash("sha256").update(bytes).digest("hex"));
  const duplicate = await run(service, storeId);
  assert.equal(duplicate.body.version, imported.body.version);
  const v1 = await getSnapshot(pool, storeId);
  assert.equal(v1.snapshot.histories[service.summary.threadId].length, 223);
  assert.equal(v1.snapshot.histories[service.summary.threadId][2].payload.nul, "a\u0000b");
  assert.equal(v1.snapshot.histories[service.summary.threadId][2].payload.surrogate, "\ud800");
  v1.snapshot.names = { [service.summary.threadId]: "v1 compatibility" };
  const v1Write = await putSnapshot(pool, storeId, { expectedVersion: v1.version, snapshot: v1.snapshot }, {});
  assert.equal(v1Write.status, 200, "V1 adapter must work after a streamed import invalidates its cache");
  const v2Head = await getStoreHead(pool, storeId);
  const manifest = v2Head.historyManifest[service.summary.threadId];
  const appended = await commitDelta(pool, storeId, { expectedVersion: v2Head.version, stateChanges: [], historyChanges: [{
    threadId: service.summary.threadId, mode: "append", expectedGeneration: manifest.generation,
    expectedItemCount: manifest.itemCount, items: [{ type: "event_msg", payload: { type: "agent_message", message: "continued\u0000output" } }],
  }] }, {});
  assert.equal(appended.status, 200, "V2 must continue a lossless imported history");
  const compatibilityCache = await pool.query(
    "SELECT COUNT(*)::int AS count FROM codex_thread_store_snapshots WHERE store_id=$1",
    [storeId],
  );
  assert.equal(
    compatibilityCache.rows[0].count,
    0,
    "routine V2 commits must invalidate rather than rewrite the whole compatibility snapshot",
  );
  const shorter = await run(service, storeId);
  assert.equal(shorter.body.version, appended.body.version, "reimporting source must not erase newer turns");
  for (const phase of ["reading", "publishing"]) {
    const cancelledStore = `cancel-${crypto.randomUUID()}`;
    const fixture = serviceFor(makeFixture());
    const controller = new AbortController();
    await assert.rejects(run(fixture, cancelledStore, { signal: controller.signal, onProgress(p) {
      if (p.phase === phase && (p.bytes > 0 || p.records > 0)) controller.abort();
    } }), { name: "AbortError" });
    assert.equal((await getStoreHead(pool, cancelledStore)).version, 0, "cancel published a partial thread");
  }
  const changed = serviceFor(Buffer.from(bytes.toString().replace("Desktop import fixture 中文", "Divergent desktop message")));
  await assert.rejects(run(changed, storeId), { code: "history_diverged" });
  assert.equal((await getStoreHead(pool, storeId)).version, appended.body.version);
  const referenced = serviceFor(Buffer.from(bytes.toString().replace('"futureField":', '"history_base":{"thread_id":"ancestor","end_ordinal_exclusive":4},"futureField":')));
  const referencedStore = `referenced-${crypto.randomUUID()}`;
  await assert.rejects(run(referenced, referencedStore), { code: "invalid_history_base" });
  assert.equal((await getStoreHead(pool, referencedStore)).version, 0, "referenced fork must not silently lose ancestor context");
  // Nested referenced forks use global ordinals, but byte offsets are local
  // to each immutable rollout. Later parent turns must not leak into a child.
  const encode = (rows) => Buffer.from(rows.map((r) => JSON.stringify(r)).join("\n") + "\n");
  const rootId = crypto.randomUUID(), parentId = crypto.randomUUID(), childId = crypto.randomUUID();
  const metaRow = (id, ordinal, base = null) => ({ ordinal, type: "session_meta", payload: {
    id, cwd: "/original/workspace", source: "vscode", originator: "Codex Desktop", history_mode: "paginated", history_base: base,
    forked_from_id: base?.thread_id ?? null,
  } });
  const message = (ordinal, text) => ({ ordinal, type: "event_msg", payload: { type: "user_message", message: text } });
  const rootPrefix = encode([metaRow(rootId, 0), message(1, "inherited root")]);
  const rootBase = { thread_id: rootId, end_ordinal_exclusive: 2, end_byte_offset: rootPrefix.length };
  const parentPrefix = encode([metaRow(parentId, 2, rootBase), message(3, "inherited parent")]);
  const parentBase = { thread_id: parentId, end_ordinal_exclusive: 4, end_byte_offset: parentPrefix.length };
  const childRows = [metaRow(childId, 4, parentBase), message(5, "own child")];
  childRows[0].payload.source = { subagent: { thread_spawn: { parent_thread_id: parentId } } };
  const sources = [serviceFor(Buffer.concat([rootPrefix, encode([message(2, "future root excluded")])]), `/fixture/rollout-${rootId}.jsonl`),
    serviceFor(Buffer.concat([parentPrefix, encode([message(4, "future parent excluded")])]), `/fixture/rollout-${parentId}.jsonl`),
    serviceFor(encode(childRows), `/fixture/rollout-${childId}.jsonl`)];
  const nested = { summary: sources[2].summary, async invoke(p, n, c, params) {
    if (params.action === "list") return { sessions: sources.map((s) => s.summary) };
    if (params.action === "resolve") return sources.find((s) => s.summary.threadId === params.rolloutId)?.summary;
    return sources.find((s) => s.summary.path === params.path).invoke(p, n, c, params);
  } };
  const nestedStore = `nested-${crypto.randomUUID()}`;
  const fork = await run(nested, nestedStore);
  assert.equal(fork.body.ancestorCount, 2);
  assert.equal(fork.body.itemCount, 4);
  assert.equal(fork.body.parentThreadId, parentId);
  const snapshot = (await getSnapshot(pool, nestedStore)).snapshot;
  assert.deepEqual(Object.keys(snapshot.histories), [childId], "ancestor provenance must not create independent live threads");
  assert.equal(snapshot.histories[childId][0].payload.history_base, null);
  assert.equal(snapshot.created_threads[childId].forked_from_id, parentId);
  assert.deepEqual(snapshot.histories[childId].slice(1).map((r) => r.payload.message), ["inherited root", "inherited parent", "own child"]);
  assert.equal((await run(nested, nestedStore)).body.version, fork.body.version);
  const rawChild = await pool.query("SELECT raw_record FROM mira_codex_session_import_records WHERE import_id=$1 AND line_seq=1", [fork.body.importId]);
  assert.deepEqual(rawChild.rows[0].raw_record.payload.history_base, parentBase);
  const badSources = [...sources];
  for (const wrong of [{ ...parentBase, end_byte_offset: parentBase.end_byte_offset - 1 }, { ...parentBase, end_ordinal_exclusive: 5 }]) {
    const bad = serviceFor(encode([metaRow(crypto.randomUUID(), 4, wrong), message(5, "bad boundary")]), `/fixture/rollout-${crypto.randomUUID()}.jsonl`);
    sources[2] = bad;
    const badStore = `bad-base-${crypto.randomUUID()}`;
    await assert.rejects(run({ ...nested, summary: bad.summary }, badStore), { code: "invalid_history_base" });
    assert.equal((await getStoreHead(pool, badStore)).version, 0);
  }
  sources.splice(0, sources.length, ...badSources);
  const cancelledForkStore = `cancel-fork-${crypto.randomUUID()}`;
  const abortFork = new AbortController();
  await assert.rejects(run(nested, cancelledForkStore, { signal: abortFork.signal, onProgress(p) {
    if (p.ancestor && p.bytes > 0) abortFork.abort();
  } }), { name: "AbortError" });
  assert.equal((await getStoreHead(pool, cancelledForkStore)).version, 0);
  console.log("PASS: nested fork exact boundaries, provenance, subagent identity, exclusion of future turns, idempotence, invalid boundaries, ancestor cancellation");
  console.log("PASS: streamed desktop import, 9 MiB single record, Unicode, raw SHA-256, V1 cache rebuild/write, idempotence, divergence, read/publish cancellation");

  if (process.env.MIRA_REAL_DESKTOP_SESSION) {
    const path = process.env.MIRA_REAL_DESKTOP_SESSION;
    const file = await fs.open(path, "r");
    try {
      const stat = await file.stat();
      const first = Buffer.alloc(512 * 1024);
      const read = await file.read(first, 0, first.length, 0);
      const firstLine = first.subarray(0, first.indexOf(10, 0, read.bytesRead)).toString();
      const meta = JSON.parse(firstLine).payload;
      const summary = { path, threadId: meta.id, cwd: meta.cwd, codexVersion: meta.cli_version, sizeBytes: stat.size, modifiedAt: stat.mtime.toISOString(), title: "Desktop acceptance (local test)" };
      const real = { summary, async invoke(_p, _n, _c, params) {
        if (params.action === "list") return { sessions: [summary] };
        const chunk = Buffer.alloc(Math.min(params.limit, stat.size - params.cursor));
        const { bytesRead } = await file.read(chunk, 0, chunk.length, params.cursor);
        return { cursor: params.cursor, nextCursor: params.cursor + bytesRead, content: chunk.subarray(0, bytesRead).toString("base64"), encoding: "base64", eof: params.cursor + bytesRead === stat.size, sizeBytes: stat.size, modifiedAt: summary.modifiedAt };
      } };
      const started = Date.now();
      let lastPhase;
      let lastPrint = 0;
      const result = await run(real, `desktop-real-${crypto.randomUUID()}`, { onProgress(p) {
        if (p.phase !== lastPhase || Date.now() - lastPrint > 10000) {
          console.log(`Desktop acceptance: ${p.phase}, ${p.bytes ?? p.records ?? 0}/${p.totalBytes ?? p.totalRecords ?? 0}`);
          lastPrint = Date.now(); lastPhase = p.phase;
        }
      } });
      assert.equal(result.status, 200);
      console.log(JSON.stringify({ realDesktopImport: "passed", sizeBytes: stat.size, records: result.body.itemCount, elapsedMs: Date.now() - started }));
    } finally { await file.close(); }
  }
} finally { await pool.end(); }
